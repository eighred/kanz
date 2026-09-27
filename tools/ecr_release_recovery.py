#!/usr/bin/env python3
"""Recover reviewed, byte-identical single-platform ECR releases without Docker.

Input: JSON array of repository, digest, base64 manifest, and labels collected
from an independently reviewed deployment/cache. Never generates a new image.
AWS/GitHub credentials remain in their CLI credential stores and process memory.
"""
import argparse
import base64
import gzip
import hashlib
import json
import pathlib
import re
import subprocess
import tempfile
import urllib.error
import urllib.parse
import urllib.request

CHUNK = 1024 * 1024
MEDIA = 'application/vnd.docker.distribution.manifest.v2+json'


class RecoveryError(Exception):
    pass


def digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


def checked_digest(value):
    if not isinstance(value, str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', value):
        raise RecoveryError('Malformed SHA256 descriptor')
    return value.split(':')[1]


def descriptor(value):
    checked_digest(value['digest'])
    if type(value['size']) is not int or value['size'] < 0:
        raise RecoveryError('Invalid descriptor size')
    return value


def image_result(result):
    failures = result.get('failures', [])
    if failures:
        if len(failures) == 1 and failures[0].get('failureCode') == 'ImageNotFound' and not result.get('images'):
            return None
        raise RecoveryError('ECR image lookup failed')
    if len(result.get('images', [])) != 1:
        raise RecoveryError('ECR image lookup returned ambiguous/empty output')
    return result['images'][0]


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if urllib.parse.urlsplit(newurl).scheme != 'https':
            raise RecoveryError('Registry redirect must use HTTPS')
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        if urllib.parse.urlsplit(req.full_url).netloc != urllib.parse.urlsplit(newurl).netloc:
            redirected.remove_header('Authorization')
        return redirected


def request(url, headers=None):
    try:
        return urllib.request.build_opener(SafeRedirect()).open(
            urllib.request.Request(url, headers=headers or {}), timeout=120)
    except urllib.error.HTTPError as exc:
        # Do not include presigned URLs or authorization headers in diagnostics.
        raise RecoveryError(f'Artifact HTTP status {exc.code}') from None
    except urllib.error.URLError:
        raise RecoveryError('Artifact transport failed') from None


def copy_verified(stream, target, expected):
    descriptor(expected)
    target = pathlib.Path(target)
    target.parent.mkdir(parents=True, exist_ok=True)
    h = hashlib.sha256()
    size = 0
    with tempfile.NamedTemporaryFile(dir=target.parent, delete=False) as output:
        temp = pathlib.Path(output.name)
        try:
            while block := stream.read(CHUNK):
                size += len(block)
                if size > expected['size']:
                    raise RecoveryError('Artifact exceeds declared size')
                h.update(block)
                output.write(block)
            if size != expected['size'] or 'sha256:' + h.hexdigest() != expected['digest']:
                raise RecoveryError('Artifact digest/size mismatch')
        except BaseException:
            output.close()
            temp.unlink(missing_ok=True)
            raise
    temp.replace(target)


class ECR:
    def __init__(self, profile, region):
        self.base = ['aws', '--profile', profile, '--region', region, '--no-cli-pager', 'ecr']

    def call(self, operation, *args):
        p = subprocess.run(self.base + [operation, *args, '--output', 'json'],
                           capture_output=True, text=True, timeout=300)
        if p.returncode:
            code = re.search(r'\(([A-Za-z0-9]+(?:Exception)?)\)', p.stderr)
            raise RecoveryError(f'ECR {operation} failed: {code.group(1) if code else "CLI error"}')
        try:
            return json.loads(p.stdout)
        except ValueError:
            raise RecoveryError(f'ECR {operation} returned no valid JSON') from None

    def image(self, repo, image_id):
        r = self.call('batch-get-image', '--repository-name', repo, '--image-ids', image_id)
        return image_result(r)

    def upload(self, repo, path, desc):
        r = self.call('batch-check-layer-availability', '--repository-name', repo,
                      '--layer-digests', desc['digest'])
        if any(x.get('layerAvailability') == 'AVAILABLE' and x.get('layerDigest') == desc['digest']
               for x in r.get('layers', [])):
            return
        upload = self.call('initiate-layer-upload', '--repository-name', repo)['uploadId']
        with path.open('rb') as source, tempfile.TemporaryDirectory() as temp:
            offset = 0
            while block := source.read(10 * CHUNK):
                part = pathlib.Path(temp) / 'part'
                part.write_bytes(block)
                self.call('upload-layer-part', '--repository-name', repo, '--upload-id', upload,
                          '--part-first-byte', str(offset), '--part-last-byte', str(offset + len(block) - 1),
                          '--layer-part-blob', 'fileb://' + str(part))
                offset += len(block)
        self.call('complete-layer-upload', '--repository-name', repo, '--upload-id', upload,
                  '--layer-digests', desc['digest'])


def ghcr_headers(repo):
    p = subprocess.run(['gh', 'auth', 'token'], capture_output=True, text=True, timeout=30)
    if p.returncode or not p.stdout.strip():
        raise RecoveryError('GitHub CLI authentication unavailable')
    basic = base64.b64encode(('eighred:' + p.stdout.strip()).encode()).decode()
    with request('https://ghcr.io/token?service=ghcr.io&scope=repository:eighred/' + repo + ':pull',
                 {'Authorization': 'Basic ' + basic}) as response:
        token = json.load(response)['token']
    return {'Authorization': 'Bearer ' + token, 'Accept': MEDIA}


def validate_entry(entry):
    repo = entry['repository']
    if not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', repo):
        raise RecoveryError('Invalid repository')
    checked_digest(entry['digest'])
    raw = base64.b64decode(entry['manifest'], validate=True)
    if digest(raw) != entry['digest']:
        raise RecoveryError('Reviewed manifest digest mismatch')
    m = json.loads(raw)
    if m.get('mediaType') != MEDIA or m.get('schemaVersion') != 2:
        raise RecoveryError('Recovery requires a reviewed schema-2 single-platform manifest')
    revision = entry['labels']['org.opencontainers.image.revision']
    if not re.fullmatch(r'[0-9a-f]{40}', revision):
        raise RecoveryError('Missing full source revision')
    for d in [m['config'], *m['layers']]:
        descriptor(d)
    return repo, raw, m, revision


def recover(entry, archive, ecr, cache_url, apply):
    repo, raw, m, revision = validate_entry(entry)
    # A matching configuration binds source attribution and all uncompressed
    # layer hashes even when Docker re-compressed layers during ECR publication.
    headers = ghcr_headers(repo)
    with request(f'https://ghcr.io/v2/eighred/{repo}/manifests/sha-{revision[:7]}', headers) as response:
        upstream = json.load(response)
    if upstream.get('config') != m['config']:
        raise RecoveryError(f'{repo}: independent GHCR configuration does not match')
    blobs = archive / 'blobs' / 'sha256'
    for d in [m['config'], *m['layers']]:
        target = blobs / checked_digest(d['digest'])
        if target.exists():
            h = hashlib.sha256()
            with target.open('rb') as stream:
                while block := stream.read(CHUNK):
                    h.update(block)
            if target.stat().st_size != d['size'] or 'sha256:' + h.hexdigest() != d['digest']:
                raise RecoveryError('Archived blob digest/size mismatch')
            continue
        try:
            response = request(f'https://ghcr.io/v2/eighred/{repo}/blobs/{d["digest"]}', headers)
        except RecoveryError as exc:
            if str(exc) != 'Artifact HTTP status 404' or not cache_url:
                raise
            response = request(cache_url + '/' + checked_digest(d['digest']))
        with response:
            copy_verified(response, target, d)
    config = json.loads((blobs / checked_digest(m['config']['digest'])).read_bytes())
    labels = config.get('config', {}).get('Labels', {})
    if labels.get('org.opencontainers.image.revision') != revision or labels.get('org.opencontainers.image.source') != 'https://github.com/eighred/kanz':
        raise RecoveryError('Verified configuration has incorrect source attribution')
    diff_ids = config['rootfs']['diff_ids']
    if len(diff_ids) != len(m['layers']):
        raise RecoveryError('Layer chain length mismatch')
    for d, diff in zip(m['layers'], diff_ids):
        h = hashlib.sha256()
        with gzip.open(blobs / checked_digest(d['digest']), 'rb') as stream:
            while block := stream.read(CHUNK):
                h.update(block)
        if 'sha256:' + h.hexdigest() != diff:
            raise RecoveryError('Uncompressed layer does not match independent configuration')
    blobs.mkdir(parents=True, exist_ok=True)
    manifest_path = blobs / checked_digest(entry['digest'])
    manifest_path.write_bytes(raw)
    existing = ecr.image(repo, 'imageTag=' + revision)
    if existing and existing['imageId']['imageDigest'] != entry['digest']:
        raise RecoveryError(f'{repo}: immutable release tag points to a different digest')
    if apply:
        policy = json.loads(ecr.call('get-lifecycle-policy', '--repository-name', repo)['lifecyclePolicyText'])
        if any(r['selection']['tagStatus'] != 'untagged' for r in policy['rules']):
            raise RecoveryError('Tagged expiry must be removed before restoring release custody')
        if not existing:
            for d in [m['config'], *m['layers']]:
                ecr.upload(repo, blobs / checked_digest(d['digest']), d)
            result = ecr.call('put-image', '--repository-name', repo, '--image-tag', revision,
                              '--image-digest', entry['digest'], '--image-manifest', 'file://' + str(manifest_path))
            if result['image']['imageId']['imageDigest'] != entry['digest']:
                raise RecoveryError('ECR returned an unexpected restored digest')
    print(f'{repo}@{entry["digest"]}: source and every blob verified; ' +
          ('retained in ECR' if apply else 'archive prepared (no registry mutation)'), flush=True)


def verify_ecr(repo, expected, archive, ecr):
    """Read the entire graph from ECR, even when an archive/cache already exists."""
    checked_digest(expected)
    image = ecr.image(repo, 'imageDigest=' + expected)
    if not image:
        raise RecoveryError(f'{repo}@{expected}: missing from ECR')
    raw = image['imageManifest'].encode()
    if digest(raw) != expected:
        raise RecoveryError('ECR manifest digest mismatch')
    m = json.loads(raw)
    blobs = archive / 'blobs' / 'sha256'
    blobs.mkdir(parents=True, exist_ok=True)
    (blobs / checked_digest(expected)).write_bytes(raw)
    if 'manifests' in m:
        for child in m['manifests']:
            descriptor(child)
            actual = verify_ecr(repo, child['digest'], archive, ecr)
            if actual != child['size']:
                raise RecoveryError('Child manifest size mismatch')
    else:
        for d in [m['config'], *m['layers']]:
            descriptor(d)
            url = ecr.call('get-download-url-for-layer', '--repository-name', repo,
                           '--layer-digest', d['digest'])['downloadUrl']
            if urllib.parse.urlsplit(url).scheme != 'https':
                raise RecoveryError('ECR blob download must use HTTPS')
            with request(url) as stream:
                copy_verified(stream, blobs / checked_digest(d['digest']), d)
    return len(raw)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inventory', type=pathlib.Path, required=True)
    parser.add_argument('--archive', type=pathlib.Path, required=True)
    parser.add_argument('--profile', default='kanz-platform')
    parser.add_argument('--region', choices=['ap-northeast-1'], default='ap-northeast-1')
    parser.add_argument('--cache-url', help='Optional SSM-forwarded loopback original-blob server')
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--verify-ecr', action='store_true', help='Freshly download all pinned ECR graphs; never use node/local cached blobs')
    args = parser.parse_args()
    if args.cache_url and not re.fullmatch(r'http://127\.0\.0\.1:[0-9]{1,5}', args.cache_url):
        parser.error('cache-url must be an explicit IPv4 loopback port')
    entries = json.loads(args.inventory.read_text())
    if not entries:
        parser.error('inventory must not be empty')
    if args.apply and args.verify_ecr:
        parser.error('apply and verify-ecr are separate operations')
    # Validate the complete inventory before any registry mutation.
    for entry in entries:
        if not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', entry['repository']):
            raise RecoveryError('Invalid repository')
        checked_digest(entry['digest'])
        if digest(base64.b64decode(entry['manifest'], validate=True)) != entry['digest']:
            raise RecoveryError('Reviewed manifest digest mismatch')
        if not args.verify_ecr:
            validate_entry(entry)
    identity = subprocess.run(['aws', '--profile', args.profile, '--region', args.region,
                               'sts', 'get-caller-identity', '--query', 'Account', '--output', 'text'],
                              capture_output=True, text=True, timeout=60)
    if identity.returncode or identity.stdout.strip() != '012619468098':
        raise RecoveryError('AWS identity is not the reviewed Tokyo account')
    ecr = ECR(args.profile, args.region)
    for entry in entries:
        if args.verify_ecr:
            verify_ecr(entry['repository'], entry['digest'], args.archive, ecr)
            print(f'{entry["repository"]}@{entry["digest"]}: complete fresh ECR pull verified', flush=True)
        else:
            recover(entry, args.archive, ecr, args.cache_url, args.apply)
    args.archive.mkdir(parents=True, exist_ok=True)
    (args.archive / 'oci-layout').write_text('{"imageLayoutVersion":"1.0.0"}')
    (args.archive / 'index.json').write_text(json.dumps({'schemaVersion': 2, 'manifests': [
        {'mediaType': json.loads(base64.b64decode(e['manifest']))['mediaType'], 'digest': e['digest'], 'size': len(base64.b64decode(e['manifest'])),
         'annotations': {'org.opencontainers.image.ref.name': e['repository'] + '@' + e['digest']}}
        for e in entries]}, indent=2))


if __name__ == '__main__':
    try:
        main()
    except (RecoveryError, KeyError, ValueError, OSError, subprocess.TimeoutExpired) as exc:
        # Exceptions from a transport may include a signed URL; use only our
        # explicitly sanitized diagnostics for CLI/HTTP failures.
        raise SystemExit(str(exc) if isinstance(exc, RecoveryError) else 'Recovery failed: invalid input or unavailable artifact') from None

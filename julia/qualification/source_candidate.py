"""Qualify one reproducible OpenSSL development-source input without release authority."""
import argparse
import datetime as dt
import hashlib
import json
from pathlib import Path
import re
import tempfile
import tomllib
import zipfile

import curl_advisory
import curl_runtime
import native_inventory
import native_mapping
import openssl_advisory
import openssl_runtime
import proof
import source_candidate_oci

PINS = tomllib.loads(Path(__file__).with_suffix('.toml').read_text())
PRODUCER_RUN = '44185887107'
PRODUCER_COMMIT = '0813a109132f0d963fcb1c3fe31f1d0cedb92b1c'


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def read_producer(path):
    # Authenticate the entire artifact before interpreting a single archive member.
    if path.stat().st_size != 14035645:
        raise ValueError('unexpected producer artifact size')
    with path.open('rb') as stream:
        if hashlib.file_digest(stream, 'sha256').hexdigest() != PINS['artifact_sha256']:
            raise ValueError('producer artifact digest differs')
        stream.seek(0)
        with zipfile.ZipFile(stream) as archive:
            receipt = json.loads(archive.read('receipt.json'))
            native = archive.read('one_native.tar.gz')
            if native != archive.read('two_native.tar.gz') or digest(native) != PINS['archive_sha256']:
                raise ValueError('independent native archives differ')
    if (receipt['sourceSHA'] != PRODUCER_COMMIT or receipt['runID'] != PRODUCER_RUN
            or receipt['pins']['source'] != PINS['source'] or receipt['pins']['version'] != PINS['version']
            or receipt['consumerAccepted'] is not False or receipt['independentBuildsEqual'] is not True
            or receipt['ownedCleanupPassed'] is not True):
        raise ValueError('unexpected source producer receipt')
    return receipt, native


def ancestry_evidence(producer, directory):
    # The producer's authenticated raw response digests bind the independently
    # retained GitHub objects. Compare pagination omits head_commit, so check
    # the fully qualified base...head endpoint and the separate head/tree object.
    def read(name, limit):
        path = directory / name
        if not 0 < path.stat().st_size <= limit:
            raise ValueError('oversized upstream evidence')
        return path.read_bytes()

    target = producer['pins']['source']
    root = 'https://api.github.com/repos/openssl/openssl'
    raw = read('upstream.json', 128 << 10)
    head = json.loads(raw, object_pairs_hook=openssl_advisory.pairs)
    if (digest(raw) not in {producer['upstreamCommit']['sha256'], PINS['head_response_sha256']}
            or head['sha'] != target or head['tree']['sha'] != producer['pins']['tree']
            or head['url'] != root + '/git/commits/' + target):
        raise ValueError('upstream head identity differs')
    head_raw = raw
    for ident, evidence in producer['vendorFixes'].items():
        fix = evidence['fix']
        raw = read(ident + '.compare.json', 8 << 20)
        obj = json.loads(raw, object_pairs_hook=openssl_advisory.pairs)
        if (digest(raw) != evidence['compareSHA256'] or obj['status'] != 'ahead'
                or obj['url'] != root + '/compare/' + fix + '...' + target
                or obj['base_commit']['sha'] != fix or obj['merge_base_commit']['sha'] != fix
                or obj['behind_by'] != 0 or not 0 < obj['ahead_by'] == obj['total_commits']):
            raise ValueError('upstream comparison does not prove ancestry')
    return {'head': target, 'tree': head['tree']['sha'], 'headResponseSHA256': digest(head_raw),
            'comparisons': len(producer['vendorFixes'])}


def source_catalog(index, records, producer, acquired, now):
    if (not acquired.tzinfo or not now.tzinfo or not dt.timedelta(0) <= now - acquired <= dt.timedelta(hours=1)):
        raise ValueError('source catalog outside qualification window')
    matches = openssl_advisory.catalog(index, records, PINS['version_base'])
    control = openssl_advisory.catalog(index, records, '3.5.8')
    if 'CVE-2026-84782' not in control or not matches or set(matches) != set(producer['vendorFixes']):
        raise ValueError('source fixes do not cover the complete release baseline or affected control')
    hashes = {name: digest(raw.encode()) for name, raw in records.items()}
    set_hash = digest(json.dumps(hashes, sort_keys=True, separators=(',', ':')).encode())
    if set_hash != PINS['record_set_sha256']:
        raise ValueError('changed CNA catalog requires source coverage review')
    for ident in matches:
        name = ident.lower() + '.json'
        evidence = producer['vendorFixes'][ident]
        refs = json.loads(records[name])['containers']['cna']['references']
        actual = [ref['url'] for ref in refs if ref.get('name') == '3.5.10 git commit' and 'patch' in ref.get('tags', [])]
        fix = producer['pins']['fixes'][ident]
        if (not re.fullmatch('[a-f0-9]{40}', fix) or fix != evidence['fix']
                or actual != ['https://github.com/openssl/openssl/commit/' + fix]
                or evidence['cnaSHA256'] != hashes[name]
                or evidence['compareURL'] != 'https://api.github.com/repos/openssl/openssl/compare/' +
                fix + '...' + PINS['source'] + '?per_page=1&page=2'
                or not re.fullmatch('[a-f0-9]{64}', evidence['compareSHA256'])):
            raise ValueError('vendor patch/producer ancestry binding differs')
    return {'acquiredAt': acquired.isoformat(), 'indexSHA256': digest(index.encode()), 'recordSetSHA256': set_hash,
            'recordCount': len(records), 'officialAffectedControl': control, 'releaseBaseline': PINS['version_base'],
            'sourceFixes': {name: producer['vendorFixes'][name] for name in matches}}


def expected_native(inputs):
    archive = native_inventory.expected_archive(inputs / 'julia.tar.gz')
    for name, item in PINS['files'].items():
        path = 'lib/julia/' + name
        if path in archive['links'] or archive['files'].get(path) != openssl_runtime.BASE_FILES[path]:
            raise ValueError('unexpected original Julia OpenSSL target')
        archive['files'][path] = item
    return curl_runtime.overlay(archive, inputs / 'libcurl.tar.gz')


def check_inventory(inventory, source, expected):
    native_inventory.validate(inventory['nativeRuntime'], expected)
    native_mapping.validate_component(inventory, expected)
    curl_runtime.validate_backend(inventory, PINS['curl_backend'])
    openssl = {'version': PINS['version'], 'versionAPI': 'OpenSSL_version(0)',
               'archiveSHA256': PINS['archive_sha256'], 'licenseSHA256': openssl_runtime.LICENSE_SHA256,
               'originalJLLVersion': '3.5.8+0', 'files': {'lib/julia/' + n: v for n, v in PINS['files'].items()}}
    if inventory['nativeOpenSSL'] != openssl:
        raise ValueError('source OpenSSL runtime identity differs')
    proof.validate_worker_inventory(inventory, source, expected['files']['bin/julia']['sha256'], ['source_candidate.toml'])


def check_package_inputs(directory, locked_manifest, locked_pins):
    pins = json.loads(locked_pins)['packages']
    expected = {'Manifest.toml', 'offline_packages.json', *(name + '.tar.gz' for name in pins)}
    if (directory.is_symlink() or {path.name for path in directory.iterdir()} != expected
            or any(path.is_symlink() or not path.is_file() for path in directory.iterdir())):
        raise ValueError('unexpected package input inventory')
    def equal_bytes(name, trusted):
        path = directory / name
        return path.stat().st_size == len(trusted) and path.read_bytes() == trusted
    if not equal_bytes('Manifest.toml', locked_manifest) or not equal_bytes('offline_packages.json', locked_pins):
        raise ValueError('package declarations differ from the committed input contract')
    for name, pin in pins.items():
        path = directory / (name + '.tar.gz')
        if path.stat().st_size != pin['archiveBytes'] or digest(path.read_bytes()) != pin['archiveSHA256']:
            raise ValueError('repacked or changed package archive')


def build_candidate(source, inputs, output):
    if inputs.is_symlink() or {path.name for path in inputs.iterdir()} != {'packages', 'julia.tar.gz', 'openssl.tar.gz', 'libcurl.tar.gz'}:
        raise ValueError('candidate input directory must contain only reviewed public inputs')
    for name in ('julia.tar.gz', 'openssl.tar.gz', 'libcurl.tar.gz'):
        path = inputs / name
        if path.is_symlink() or not path.is_file():
            raise ValueError('linked or non-regular candidate input')
    check_package_inputs(inputs / 'packages', proof.required(['git', 'show', source + ':julia/Manifest.toml']),
                         proof.required(['git', 'show', source + ':julia/qualification/offline_packages.json']))
    context = proof.required(['git', '-c', 'core.autocrlf=false', 'archive', '--format=tar',
                              source, 'julia', 'schemas/proto'], limit=16 << 20)
    iidfile = output / 'image-id'
    proof.required(['docker', 'build', '--platform', 'linux/amd64', '--network=none',
                    '--build-context', 'candidate_inputs=' + str(inputs.resolve()), '--build-arg', 'VERSION=' + source,
                    '--iidfile', str(iidfile), '-f', 'julia/qualification/source_candidate.Dockerfile', '-'],
                   data=context, timeout=900, limit=4 << 20)
    image = iidfile.read_text().strip()
    if not re.fullmatch('sha256:[a-f0-9]{64}', image):
        raise ValueError('invalid parent-built image identity')
    metadata = json.loads(proof.required(['docker', 'image', 'inspect', image]))[0]
    labels = metadata['Config']['Labels']
    if (metadata['Id'] != image or labels.get('org.opencontainers.image.revision') != source
            or labels.get('io.eighred.qualification') != 'disposable-native-candidate'):
        raise ValueError('source candidate image/revision differs')
    return image


def verify(inputs, artifact, upstream, output):
    source = proof.required(['git', 'rev-parse', 'HEAD']).decode().strip()
    proof.required(['git', 'diff', '--quiet', 'HEAD', '--'])
    output.mkdir(exist_ok=False)
    producer, _ = read_producer(artifact)
    ancestry = ancestry_evidence(producer, upstream)
    expected = expected_native(inputs)
    image = build_candidate(source, inputs, output)
    archive = output / 'candidate.oci.tar'
    proof.required(['docker', 'image', 'save', '--output', str(archive), image], timeout=300)
    selected = {'opt/julia/lib/julia/' + name: item for name, item in PINS['files'].items()}
    selected['opt/julia/' + curl_runtime.PATH] = curl_runtime.FILE
    layers = source_candidate_oci.image_files(archive, image, selected)
    report = {'kind': 'development-source-candidate', 'sourceSHA': source, 'imageID': image,
              'ancestors': ancestry, 'oci': layers, 'upstreamSource': PINS['source'], 'producerSHA256': PINS['artifact_sha256'],
              'cases': [], 'passed': False, 'consumerAccepted': False, 'releaseQualified': False}
    names = []
    # Reserve a unique evidence path before running anything. Persist refusals as well as successes.
    with (output / 'source-candidate.json').open('x', encoding='utf-8') as stream:
        try:
            with tempfile.TemporaryDirectory(prefix='kanz-source-candidate-') as scratch:
                proof.exercise_image(image, source, Path(scratch), names, report,
                                     lambda inventory: check_inventory(inventory, source, expected), ['--source-candidate'])
            report['passed'] = False
            report['failureStage'] = 'development-source-catalog'
            acquired = dt.datetime.now(dt.timezone.utc)
            index, records = openssl_advisory.fetch_catalog(proof.required)
            report['sourceCatalog'] = source_catalog(index, records, producer, acquired, dt.datetime.now(dt.timezone.utc))
            report['failureStage'] = 'libcurl-release-catalog'
            curl_report = {}
            curl_advisory.qualify_backend(proof.required, report['inventory'], lambda: dt.datetime.now(dt.timezone.utc),
                                          curl_report, PINS['curl_backend'])
            report['libcurlCatalog'] = curl_report
            (output / 'openssl-catalog.json').write_text(json.dumps({'index': index, 'records': records}), encoding='utf-8')
            report.pop('failureStage')
            report['passed'] = True
        finally:
            report['cleanupPassed'] = proof.cleanup_containers(names, source)
            report['passed'] &= report['cleanupPassed']
            json.dump(report, stream, indent=2)
            stream.write('\n')
    if not report['passed']:
        raise RuntimeError('development source checks incomplete')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    stage = commands.add_parser('stage')
    stage.add_argument('producer', type=Path)
    stage.add_argument('native_archive', type=Path)
    check = commands.add_parser('verify')
    check.add_argument('inputs', type=Path)
    check.add_argument('producer', type=Path)
    check.add_argument('upstream', type=Path)
    check.add_argument('output', type=Path)
    args = parser.parse_args()
    if args.command == 'stage':
        _, native = read_producer(args.producer)
        with args.native_archive.open('xb') as stream:
            stream.write(native)
    else:
        verify(args.inputs, args.producer, args.upstream, args.output)


if __name__ == '__main__':
    main()

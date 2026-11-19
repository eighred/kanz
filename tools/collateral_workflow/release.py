"""Verify the released Accounting and migrator selected for the collateral rehearsal."""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import uuid


IMAGES = {
    'accounting': '91c37620d2cdfc31ba78ecbf1725f0288eee923825841f5323cd12411be69173',
    'kanz-migrate': '147db5e4edbb6151f44f1b2c799d14d44437298dc00e19d8ad983d54ab333a24',
}
PROGRAMS = {'accounting': 'services/accounting/cmd/accounting', 'kanz-migrate': 'cmd/kanz-migrate'}
# SHA-256 over sorted SQL filenames, a NUL separator and the SHA-256 of each
# file in release commit f7217a76b48d9b6075a0683ddf35be50fbdf37cb.
MIGRATIONS = '3d747eef0026202c81626663cc63abd26c5a05a34e1ad1ad6d934e0863b9b2ff'
PREDICATES = {'spdxjson': 'https://spdx.dev/Document', 'vuln': 'https://cosign.sigstore.dev/attestation/vuln/v1'}


def reference(name):
    return f'ghcr.io/eighred/{name}@sha256:{IMAGES[name]}'


def attestations(lines, name, kind):
    matches = []
    for line in lines.splitlines():
        envelope = json.loads(line)
        if envelope['payloadType'] != 'application/vnd.in-toto+json':
            raise ValueError('unexpected attestation envelope')
        statement = json.loads(base64.b64decode(envelope['payload'], validate=True))
        subjects = statement['subject']
        if (statement.get('_type') != 'https://in-toto.io/Statement/v0.1' or len(subjects) != 1
                or subjects[0].get('name') != 'ghcr.io/eighred/' + name
                or subjects[0]['digest'].get('sha256') != IMAGES[name]
                or statement['predicateType'] != PREDICATES[kind]):
            raise ValueError('release subject or predicate differs')
        predicate = statement['predicate']
        if kind == 'spdxjson':
            packages = predicate['packages']
            if not predicate['spdxVersion'].startswith('SPDX-') or not packages:
                raise ValueError('empty signed software inventory')
            matches.append(len(packages))
        else:
            scanner = predicate.get('scanner', {})
            report = scanner.get('result', {})
            version = scanner.get('version', '')
            if (not isinstance(predicate.get('invocation'), dict) or version.lstrip('v') != '0.72.0'
                    or scanner.get('uri') != 'pkg:github/aquasecurity/trivy@' + version
                    or report.get('SchemaVersion') != 2 or report.get('ArtifactType') != 'container_image'
                    or not isinstance(report.get('Results'), list)
                    or not report['Results']):
                raise ValueError('expected the released Trivy vulnerability scan')
            matches.append(len(report['Results']))
    if not matches:
        raise ValueError('missing verified release attestation')
    return matches


def rootfs(archive, name, program):
    executable = '/' + name
    seen = set()
    sql = {}
    binary_hash = None
    for member in archive:
        path = '/' + member.name.removeprefix('./')
        migration = path.startswith('/migrations/accounting/')
        if path.lower().endswith(('.jar', '.jmod', '.class', '.war', '.ear')):
            raise ValueError('JVM artifact in released filesystem')
        if not member.isfile():
            if path == executable or migration:
                raise ValueError('unexpected file type in executable or Accounting migrations')
            continue
        if path in seen:
            raise ValueError('duplicate path in flattened image')
        seen.add(path)
        handle = archive.extractfile(member)
        header = handle.read(8)
        if header.startswith(b'\xca\xfe\xba\xbe'):
            raise ValueError('JVM artifact in released filesystem')
        if header.startswith(b'\x7fELF'):
            if path != executable or header[:6] != b'\x7fELF\x02\x01' or not member.mode & 0o111:
                raise ValueError('unexpected native executable or library')
            digest = hashlib.sha256()
            with program.open('xb') as output:
                digest.update(header)
                output.write(header)
                while chunk := handle.read(1024 * 1024):
                    digest.update(chunk)
                    output.write(chunk)
            binary_hash = digest.hexdigest()
        elif migration:
            filename = path.removeprefix('/migrations/accounting/')
            if '/' in filename or not filename.endswith('.sql') or member.size > 1024 * 1024:
                raise ValueError('unexpected bundled Accounting migration')
            sql[filename] = hashlib.sha256(header + handle.read()).digest()
    if binary_hash is None:
        raise ValueError('release executable missing')
    if name == 'kanz-migrate':
        inventory = hashlib.sha256(b''.join(k.encode() + b'\0' + sql[k] for k in sorted(sql))).hexdigest()
        if inventory != MIGRATIONS or len(sql) != 20:
            raise ValueError('bundled Accounting migrations differ from the release source')
    return binary_hash, len(sql)


def inspect(folder):
    for name in IMAGES:
        ref = reference(name)
        for kind in PREDICATES:
            path = folder / 'collateral-attestations' / f'{name}.{kind}.jsonl'
            counts = attestations(path.read_bytes(), name, kind)
            print(f'{ref}: signed {kind} entries {counts}', flush=True)
        info, = json.loads(subprocess.check_output(['docker', 'image', 'inspect', ref]))
        if (info['Os'] != 'linux' or info['Architecture'] != 'amd64' or info['Config']['User'] != 'nonroot:nonroot'
                or info['Config']['Entrypoint'] != ['/' + name] or ref not in info['RepoDigests']):
            raise ValueError('release image identity or entrypoint differs')
        with tempfile.TemporaryDirectory(dir=folder) as work:
            binary = Path(work) / name
            lease = str(uuid.uuid4())
            cid = subprocess.check_output(['docker', 'create', '--pull=never', '--network=none',
                                           '--label', 'kanz.collateral.lease=' + lease, ref], text=True).strip()
            try:
                with subprocess.Popen(['docker', 'export', cid], stdout=subprocess.PIPE) as proc:
                    try:
                        with tarfile.open(fileobj=proc.stdout, mode='r|*') as archive:
                            sha, count = rootfs(archive, name, binary)
                        # Docker writes tar padding after the end marker.
                        while proc.stdout.read(65536):
                            pass
                    finally:
                        proc.stdout.close()
                    if proc.wait() != 0:
                        raise RuntimeError('Docker filesystem export failed')
            finally:
                subprocess.run(['docker', 'rm', '-v', cid], check=True, stdout=subprocess.DEVNULL)
                if subprocess.check_output(['docker', 'ps', '-aq', '--filter', 'label=kanz.collateral.lease=' + lease]).strip():
                    raise RuntimeError('leased inventory container survived cleanup')
            metadata = subprocess.check_output(['go', 'version', '-m', str(binary)], text=True)
            if (f'\tpath\tgithub.com/eighred/kanz/{PROGRAMS[name]}\n' not in metadata or
                    '\tbuild\tGOOS=linux\n' not in metadata or '\tbuild\tGOARCH=amd64\n' not in metadata or
                    '\tbuild\tCGO_ENABLED=0\n' not in metadata):
                raise ValueError('extracted program has unexpected Go build metadata')
            print(f'{ref}: ELF sha256={sha} config={info["Id"]} migrations={count}', flush=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('scratch', type=Path)
    arguments = parser.parse_args()
    inspect(arguments.scratch)

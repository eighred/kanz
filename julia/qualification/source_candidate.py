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

import curl_runtime
import native_inventory
import native_mapping
import openssl_advisory
import openssl_runtime
import proof

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


def verify(image, inputs, artifact, output):
    if not re.fullmatch('sha256:[a-f0-9]{64}', image):
        raise ValueError('an immutable local image ID is required')
    source = proof.required(['git', 'rev-parse', 'HEAD']).decode().strip()
    proof.required(['git', 'diff', '--quiet', 'HEAD', '--'])
    metadata = json.loads(proof.required(['docker', 'image', 'inspect', image]))[0]
    labels = metadata['Config']['Labels']
    if (metadata['Id'] != image or labels.get('org.opencontainers.image.revision') != source
            or labels.get('io.eighred.qualification') != 'disposable-native-candidate'):
        raise ValueError('source candidate image/revision differs')
    producer, _ = read_producer(artifact)
    expected = expected_native(inputs)
    report = {'kind': 'development-source-candidate', 'sourceSHA': source, 'imageID': image,
              'upstreamSource': PINS['source'], 'producerSHA256': PINS['artifact_sha256'],
              'cases': [], 'passed': False, 'consumerAccepted': False, 'releaseQualified': False}
    names = []
    # Reserve a unique evidence path before running anything. Persist refusals as well as successes.
    with output.open('x', encoding='utf-8') as stream:
        try:
            with tempfile.TemporaryDirectory(prefix='kanz-source-candidate-') as scratch:
                proof.exercise_image(image, source, Path(scratch), names, report,
                                     lambda inventory: check_inventory(inventory, source, expected), ['--source-candidate'])
            report['passed'] = False
            report['failureStage'] = 'development-source-catalog'
            acquired = dt.datetime.now(dt.timezone.utc)
            index, records = openssl_advisory.fetch_catalog(proof.required)
            report['sourceCatalog'] = source_catalog(index, records, producer, acquired, dt.datetime.now(dt.timezone.utc))
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
    check.add_argument('image')
    check.add_argument('inputs', type=Path)
    check.add_argument('producer', type=Path)
    check.add_argument('output', type=Path)
    args = parser.parse_args()
    if args.command == 'stage':
        _, native = read_producer(args.producer)
        with args.native_archive.open('xb') as stream:
            stream.write(native)
    else:
        verify(args.image, args.inputs, args.producer, args.output)


if __name__ == '__main__':
    main()

"""Actual-image Julia advisory qualification; never release authorization."""
import datetime as dt
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import tarfile
import tempfile
import tomllib
import uuid
from urllib.parse import parse_qs, unquote, urlsplit

import proof

ROOT = proof.ROOT
ARCHIVES = {'trivy': 'bbb64b9695866ce4a7a8f5c9592002c5961cab378577fa3f8a040df362b9b2ea',
            'syft': '1c2493cfcf52d3367d6cb25780d9190c111c2219c34c820574c43e8bae15c528'}
LIMITATIONS = ['stdlib-runtime-native-advisory-coverage-unqualified',
               'JLSEC-General-registry-only-and-incomplete-artifact-advisories',
               'no-release-or-deployment-authorization']
EXTERNAL_UUIDS = {'ProtoBuf': '3349acd9-ac6a-5e09-bcdb-63829b23a429',
                  'BufferedStreams': 'e1450e63-4bb3-523b-b2a4-4ffa8c0fd77d',
                  'EnumX': '4e289a0a-7415-4d19-859d-a7e5c4648b56'}
HTTP_UUID = 'cd3eb016-35fb-5094-929b-558a96fad6f3'


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def tool_identity(executable, archive_hash):
    archive = executable.parent / (executable.name + '.tar.gz')
    if digest(archive) != archive_hash:
        raise ValueError('scanner archive differs')
    with tarfile.open(archive, 'r:gz') as package:
        members = [m for m in package.getmembers() if m.name == executable.name]
        if len(members) != 1 or not members[0].isfile() or not 0 < members[0].size <= 256 << 20:
            raise ValueError('scanner executable archive member differs')
        with package.extractfile(members[0]) as stream:
            expected = hashlib.file_digest(stream, 'sha256').hexdigest()
    if digest(executable) != expected:
        raise ValueError('installed scanner differs from verified archive')
    return {'archiveSHA256': archive_hash, 'executableSHA256': expected}


def package_inventory():
    manifest = tomllib.loads((ROOT / 'kanz-jl/Manifest.toml').read_text())
    if any(len(rows) != 1 for rows in manifest['deps'].values()):
        raise ValueError('ambiguous locked package')
    result = {name: {'uuid': rows[0]['uuid'], 'version': rows[0]['version']}
              for name, rows in manifest['deps'].items()}
    if any(result.get(name, {}).get('uuid') != uuid for name, uuid in EXTERNAL_UUIDS.items()):
        raise ValueError('reviewed General registry package UUID differs')
    return result


def manifest_result(scan, target):
    if scan.get('Trivy', {}).get('Version') != '0.72.0':
        raise ValueError('scanner version differs')
    matches = [r for r in scan.get('Results', [])
               if r.get('Type') == 'julia' and r.get('Target', '').lstrip('/') == target]
    if len(matches) != 1:
        raise ValueError('Julia manifest scan missing or ambiguous')
    return matches[0]


def validate_packages(result, expected):
    packages = result.get('Packages', [])
    actual = {p['Name']: {'uuid': p['ID'], 'version': p['Version']} for p in packages}
    if len(actual) != len(packages) or actual != expected or result.get('ExperimentalModifiedFindings'):
        raise ValueError('Julia UUID/version scan inventory differs or findings modified')


def validate_image_scan(scan, image, expected):
    if scan.get('Metadata', {}).get('ImageID') != image:
        raise ValueError('scanner image identity differs')
    if any(r.get('ExperimentalModifiedFindings') for r in scan.get('Results', [])):
        raise ValueError('image findings suppressed or modified')
    result = manifest_result(scan, 'opt/kanz-jl/Manifest.toml')
    validate_packages(result, expected)
    if result.get('Vulnerabilities'):
        raise ValueError('known Julia dependency advisory refuses qualification')
    return result


def validate_control(scan):
    result = manifest_result(scan, 'Manifest.toml')
    validate_packages(result, {'HTTP': {'uuid': HTTP_UUID, 'version': '1.10.15'}})
    found = [v for v in result.get('Vulnerabilities', []) if v.get('PkgName') == 'HTTP'
             and v.get('PkgID') == HTTP_UUID and v.get('InstalledVersion') == '1.10.15'
             and (v.get('VulnerabilityID') == 'CVE-2025-52479' or 'JLSEC-2025-1' in v.get('VendorIDs', []))]
    if not found:
        raise ValueError('known vulnerable Julia control not detected')


def validate_spdx(sbom, expected):
    found = {}
    for package in sbom.get('packages', []):
        if package.get('sourceInfo') != 'package found in: opt/kanz-jl/Manifest.toml':
            continue
        purls = [ref['referenceLocator'] for ref in package.get('externalRefs', []) if ref.get('referenceType') == 'purl']
        if len(purls) != 1:
            raise ValueError('Julia SPDX package identity absent')
        parsed = urlsplit(purls[0])
        name_version = parsed.path.removeprefix('julia/').split('@')
        qualifiers = parse_qs(parsed.query, strict_parsing=True)
        if parsed.scheme != 'pkg' or len(name_version) != 2 or set(qualifiers) != {'uuid'} or len(qualifiers['uuid']) != 1:
            raise ValueError('Julia SPDX identity malformed')
        name, version = map(unquote, name_version)
        if name in found or package.get('name') != name or package.get('versionInfo') != version:
            raise ValueError('Julia SPDX identity duplicated or substituted')
        found[name] = {'uuid': qualifiers['uuid'][0], 'version': version}
    if found != expected:
        raise ValueError('Julia SPDX UUID/version inventory differs')


def image_config_digest(image, scratch):
    # Reuse the existing strict archive parser; Docker29 may expose a manifest
    # ID while Syft/stereoscope reports the config digest. Never compare kinds.
    module_path = ROOT / 'kanz/services/tv-sync/test/isolation.py'
    spec = importlib.util.spec_from_file_location('kanz_image_identity', module_path)
    identity = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(identity)
    alias = 'kanz-isolation/julia-advisory:kanz-tv-isolation-' + uuid.uuid4().hex[:12]
    if proof.required(['docker', 'image', 'ls', '--filter', 'reference=' + alias, '--format', '{{.ID}}']).strip():
        raise ValueError('local image alias already exists')
    proof.required(['docker', 'tag', image, alias])
    tagged = proof.required(['docker', 'image', 'inspect', alias, '--format', '{{.Id}}']).decode().strip()
    if tagged != image:
        raise ValueError('local alias Docker identity differs')
    archive = scratch / 'qualified-image.tar'
    proof.required(['docker', 'save', '--output', str(archive), alias], timeout=180)
    config = identity.archive_config_digest(archive, alias)
    if proof.required(['docker', 'image', 'inspect', alias, '--format', '{{.Id}}']).decode().strip() != image:
        raise ValueError('local alias changed during export')
    return config


def syft_identity_projection(sbom):
    metadata = sbom.get('source', {}).get('metadata', {})
    version = sbom.get('descriptor', {}).get('version')
    result = {'version': version if isinstance(version, str) and re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', version) else 'invalid',
              'type': 'image' if sbom.get('source', {}).get('type') == 'image' else 'invalid'}
    for key in ('imageID', 'manifestDigest'):
        value = metadata.get(key, '')
        result[key] = value if isinstance(value, str) and re.fullmatch('sha256:[0-9a-f]{64}', value) else 'invalid'
    return result


def validate_syft(sbom, config_digest):
    if (sbom.get('descriptor', {}).get('version') != '1.56.0' or sbom.get('source', {}).get('metadata', {}).get('imageID') != config_digest
            or sbom.get('source', {}).get('type') != 'image'):
        raise ValueError('Syft image/version differs')
    runtime = [p for p in sbom.get('artifacts', []) if p.get('name') == 'julia' and p.get('version') == '1.13.3'
               and p.get('foundBy') == 'binary-classifier-cataloger']
    if not runtime:
        raise ValueError('actual Julia runtime binary missing from Syft inventory')
    return [{'name': p['name'], 'version': p.get('version', ''), 'type': p.get('type', ''),
             'purl': p.get('purl', ''), 'foundBy': p.get('foundBy', '')} for p in sbom['artifacts']]


def validate_database(metadata, now):
    updated = dt.datetime.fromisoformat(metadata['UpdatedAt'].replace('Z', '+00:00'))
    next_update = dt.datetime.fromisoformat(metadata['NextUpdate'].replace('Z', '+00:00'))
    downloaded = dt.datetime.fromisoformat(metadata['DownloadedAt'].replace('Z', '+00:00'))
    if metadata['Version'] != 2 or not (dt.timedelta(0) <= now - updated <= dt.timedelta(hours=24)) or next_update < now or not (updated <= downloaded <= now):
        raise ValueError('advisory database missing, stale or future-dated')


def validate_report(report, qualification, expected):
    if (report.get('passed') is not True or report.get('localOnly') is not True
            or report.get('knownAdvisoryControlPassed') is not True
            or report.get('limitations') != LIMITATIONS
            or report.get('sourceSHA') != qualification['sourceSHA']
            or report.get('imageID') != qualification['imageID']
            or report.get('installedPackages') != qualification['inventory']['packages']
            or report.get('juliaManifest') != expected or report.get('spdxPackages') != expected):
        raise ValueError('advisory evidence incomplete or unbound')
    for name, checksum in ARCHIVES.items():
        tool = report.get('tools', {}).get(name, {})
        if tool.get('archiveSHA256') != checksum or not re.fullmatch('[0-9a-f]{64}', tool.get('executableSHA256', '')):
            raise ValueError('scanner identity missing')
    config = report.get('archiveConfigDigest', '')
    if (not re.fullmatch('sha256:[0-9a-f]{64}', config)
            or report.get('syftIdentity', {}).get('imageID') != config
            or report.get('syftIdentity', {}).get('version') != '1.56.0'
            or report.get('syftIdentity', {}).get('type') != 'image'):
        raise ValueError('Syft config identity not archive-bound')
    database = report['database']
    if not re.fullmatch('[0-9a-f]{64}', database.get('sha256', '')):
        raise ValueError('advisory snapshot identity missing')
    validate_database(database, dt.datetime.fromisoformat(report['scanTime']))
    inventory = report['runtimeAndOSInventory']
    if not any(p.get('name') == 'julia' and p.get('version') == '1.13.3'
               and p.get('foundBy') == 'binary-classifier-cataloger' for p in inventory):
        raise ValueError('runtime inventory absent')
    if any(v.get('severity') == 'CRITICAL' for result in report['otherScanFindings'] for v in result['advisories']):
        raise ValueError('critical image advisory present')


def main():
    directory = Path(os.environ['JULIA_OCI_EVIDENCE']).resolve()
    report = {'passed': False, 'localOnly': True, 'limitations': LIMITATIONS}
    stage = 'qualified-image-binding'
    try:
        source = proof.required(['git', 'rev-parse', 'HEAD']).decode().strip()
        qualification = json.loads((directory / 'julia-oci.json').read_text())
        proof.validate_report(qualification, source, digest(Path(shutil.which('julia')).resolve()),
            proof.native_inventory.expected_archive(Path(os.environ['RUNNER_TEMP']) / 'julia.tar.gz'))
        image = qualification['imageID']
        info = json.loads(proof.required(['docker', 'image', 'inspect', image]))[0]
        if info['Id'] != image or info['Config']['Labels']['org.opencontainers.image.revision'] != source:
            raise ValueError('actual image no longer matches qualification')
        report.update(sourceSHA=source, imageID=image, installedPackages=qualification['inventory']['packages'])
        tools = {}
        for name, archive in ARCHIVES.items():
            executable = Path(shutil.which(name)).resolve()
            tools[name] = tool_identity(executable, archive)
        report['tools'] = tools
        expected = package_inventory()
        if len(expected) != 7 or set(report['installedPackages']) != {'ProtoBuf', 'BufferedStreams', 'EnumX'}:
            raise ValueError('reviewed dependency scope changed')
        with tempfile.TemporaryDirectory(prefix='kanz-julia-advisory-') as temporary:
            scratch = Path(temporary)
            env = {'PATH': os.environ['PATH'], 'HOME': str(scratch), 'TMPDIR': str(scratch), 'LANG': 'C.UTF-8'}
            def run(args, limit=64 << 20, cwd=scratch):
                return proof.required(args, env=env, timeout=900, limit=limit, cwd=cwd)
            config = scratch / 'trivy.yaml'; config.write_text('{}')
            ignore = scratch / 'empty.ignore'; ignore.write_text('')
            common = ['--cache-dir', str(scratch / 'cache'), '--config', str(config), '--timeout', '10m', '--quiet']
            stage = 'current-advisory-database'
            run(['trivy', 'image', *common, '--download-db-only'])
            db = scratch / 'cache/db'
            metadata = json.loads((db / 'metadata.json').read_text())
            validate_database(metadata, dt.datetime.now(dt.timezone.utc))
            db_hash = digest(db / 'trivy.db')
            report['database'] = {'sha256': db_hash, **metadata}
            report['scanTime'] = dt.datetime.now(dt.timezone.utc).isoformat()
            flags = [*common, '--skip-db-update', '--scanners', 'vuln', '--ignore-unfixed=false',
                     '--ignorefile', str(ignore), '--list-all-pkgs', '--format', 'json']
            stage = 'known-vulnerable-manifest-control'
            control = scratch / 'control'; control.mkdir()
            (control / 'Project.toml').write_text('[deps]\nHTTP = "' + HTTP_UUID + '"\n')
            (control / 'Manifest.toml').write_text('julia_version = "1.13.3"\nmanifest_format = "2.0"\n[[deps.HTTP]]\nuuid = "' + HTTP_UUID + '"\nversion = "1.10.15"\n')
            control_scan = json.loads(run(['trivy', 'fs', *flags, '.'], cwd=control))
            validate_control(control_scan)
            report['knownAdvisoryControlPassed'] = True
            stage = 'actual-image-julia-scan'
            scan = json.loads(run(['trivy', 'image', *flags, '--image-src', 'docker', image]))
            validate_image_scan(scan, image, expected)
            report['juliaManifest'] = expected
            # Raw scanner reports can include image configuration; keep them only
            # in the private temporary directory, project only public evidence.
            raw = scratch / 'trivy.json'; raw.write_text(json.dumps(scan))
            stage = 'actual-image-julia-spdx'
            sbom = json.loads(run(['trivy', 'convert', '--format', 'spdx-json', str(raw)]))
            validate_spdx(sbom, expected)
            report['spdxPackages'] = expected
            stage = 'actual-image-config-binding'
            report['archiveConfigDigest'] = image_config_digest(image, scratch)
            stage = 'actual-image-runtime-syft'
            syft = json.loads(run(['syft', 'scan', 'docker:' + image, '--quiet', '-o', 'json']))
            report['syftIdentity'] = syft_identity_projection(syft)
            report['runtimeAndOSInventory'] = validate_syft(syft, report['archiveConfigDigest'])
            report['otherScanFindings'] = [{'target': r.get('Target'), 'type': r.get('Type'),
                'advisories': [{'id': v.get('VulnerabilityID'), 'package': v.get('PkgName'),
                    'version': v.get('InstalledVersion'), 'severity': v.get('Severity')} for v in r.get('Vulnerabilities', [])]}
                for r in scan.get('Results', []) if r.get('Vulnerabilities')]
            stage = 'actual-image-advisory-policy'
            if any(v.get('Severity') == 'CRITICAL' for r in scan.get('Results', []) for v in r.get('Vulnerabilities', [])):
                raise ValueError('critical actual-image advisory refuses qualification')
            if digest(db / 'trivy.db') != db_hash:
                raise ValueError('advisory snapshot changed during qualification')
            report['passed'] = True
            validate_report(report, qualification, expected)
    except Exception as error:
        report['passed'] = False
        report.update(failureStage=stage, errorType=type(error).__name__)
        raise
    finally:
        (directory / 'julia-advisory.json').write_text(json.dumps(report, indent=2) + '\n')
    validate_report(report, qualification, expected)
    print('Julia advisory qualification PASS: actual image UUID/version/SPDX identity, installed tree binding, known-advisory control; runtime/native coverage remains unqualified')


if __name__ == '__main__':
    main()

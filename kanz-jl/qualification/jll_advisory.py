"""Scanner-only JLL metadata; not a worker environment or full native clearance."""
import hashlib
import json
import re
import tomllib
import native_mapping

REGISTRY_COMMIT = 'afacc2e68d6c156d80eb9ff489370bb38d1a61f4'
REGISTRY_SHA256 = '96987d7602353122775f08ea65ae9d3ab201c826ddc5eea71f978da45ea23b39'
REGISTRY_URL = 'https://raw.githubusercontent.com/JuliaRegistries/General/' + REGISTRY_COMMIT + '/Registry.toml'
OPENSSL_UUID = '458c3c95-2e84-50aa-8efc-19380b2a3a95'
CONTROL = {'OpenSSL_jll': {'uuid': OPENSSL_UUID, 'version': '3.0.12+0'}}


def packages(native):
    result = {}
    for name, item in native['jll'].items():
        if (not re.fullmatch('[A-Za-z][A-Za-z0-9_]*_jll', name)
                or item['name'] != name
                or not re.fullmatch('[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}', item['uuid'])
                or not re.fullmatch('[0-9][0-9A-Za-z.+-]{0,127}', item['version'])):
            raise ValueError('invalid archive-bound JLL package')
        result[name] = {k: item[k] for k in ('uuid', 'version')}
    if len(result) != 23 or len({p['uuid'] for p in result.values()}) != len(result):
        raise ValueError('reviewed JLL inventory scope differs')
    return result


def registry_identity(raw, expected):
    if len(raw) > 4 << 20 or hashlib.sha256(raw).hexdigest() != REGISTRY_SHA256:
        raise ValueError('pinned General registry differs')
    registered = tomllib.loads(raw.decode())['packages']
    for name, item in expected.items():
        entry = registered.get(item['uuid'], {})
        if entry.get('name') != name or entry.get('path') != 'jll/' + name[0].upper() + '/' + name:
            raise ValueError('JLL General registry UUID/name differs')


def manifest(expected):
    # No Pkg instantiate or execution: input solely for the pinned Julia analyzer.
    lines = ['julia_version = "1.13.3"', 'manifest_format = "2.0"']
    for name, item in sorted(expected.items()):
        lines += ['[[deps.' + name + ']]', 'uuid = ' + json.dumps(item['uuid']),
                  'version = ' + json.dumps(item['version'])]
    return ('\n'.join(lines) + '\n').encode()


def project(expected):
    # Trivy0.72 resolves Manifest packages from Project direct dependencies.
    # This scanner input is never instantiated or substituted for the worker.
    return ('[deps]\n' + ''.join(name + ' = ' + json.dumps(item['uuid']) + '\n'
            for name, item in sorted(expected.items()))).encode()


def validate_control(result):
    if not any(v.get('PkgName') == 'OpenSSL_jll' and v.get('PkgID') == OPENSSL_UUID
               and v.get('InstalledVersion') == '3.0.12+0'
               and (v.get('VulnerabilityID') == 'CVE-2024-5535' or 'JLSEC-2026-252' in v.get('VendorIDs', []))
               for v in result.get('Vulnerabilities', [])):
        raise ValueError('known vulnerable JLL control not detected')


def validate_report(report, native, mapping=None, inventory=None, scan_time=None):
    expected = packages(native)
    findings = report.get('advisories') if isinstance(report, dict) else None
    if not isinstance(findings, list) or len(findings) > 4096:
        raise ValueError('JLL findings absent or unbounded')
    if findings:
        if mapping is None:
            raise ValueError('native identity qualification missing')
        native_mapping.validate_report(mapping, inventory, scan_time)
        if any(not native_mapping.resolves(finding) for finding in findings):
            raise ValueError('unmapped JLL finding refuses qualification')
    if (report != {'registryCommit': REGISTRY_COMMIT, 'registrySHA256': REGISTRY_SHA256,
                   'packages': expected, 'scannerManifestSHA256': hashlib.sha256(manifest(expected)).hexdigest(),
                   'scannerProjectSHA256': hashlib.sha256(project(expected)).hexdigest(),
                   'knownJLLAdvisoryControlPassed': True, 'advisories': findings,
                   'scope': 'archive-bound-JLL-metadata-only'}):
        raise ValueError('JLL advisory evidence missing or unbound')


def qualify(run, flags, scratch, native, manifest_result, validate_packages, report):
    expected = packages(native)
    raw = run(['curl', '--fail', '--silent', '--show-error', '--location', '--proto', '=https',
               '--max-time', '60', REGISTRY_URL], limit=4 << 20)
    registry_identity(raw, expected)
    registry_identity(raw, CONTROL)
    control = scratch / 'jll-control'; control.mkdir()
    (control / 'Manifest.toml').write_bytes(manifest(CONTROL))
    (control / 'Project.toml').write_bytes(project(CONTROL))
    result = manifest_result(json.loads(run(['trivy', 'fs', *flags, '.'], cwd=control)), 'Manifest.toml')
    validate_packages(result, CONTROL)
    validate_control(result)
    directory = scratch / 'jll-inventory'; directory.mkdir()
    encoded = manifest(expected)
    (directory / 'Manifest.toml').write_bytes(encoded)
    (directory / 'Project.toml').write_bytes(project(expected))
    result = manifest_result(json.loads(run(['trivy', 'fs', *flags, '.'], cwd=directory)), 'Manifest.toml')
    validate_packages(result, expected)
    report.update({'registryCommit': REGISTRY_COMMIT, 'registrySHA256': REGISTRY_SHA256,
              'packages': expected, 'scannerManifestSHA256': hashlib.sha256(encoded).hexdigest(),
              'scannerProjectSHA256': hashlib.sha256(project(expected)).hexdigest(),
              'knownJLLAdvisoryControlPassed': True, 'advisories': [],
              'scope': 'archive-bound-JLL-metadata-only'})
    report['advisories'] = [{k: v.get(k) for k in ('VulnerabilityID', 'PkgName', 'PkgID', 'InstalledVersion', 'Severity')}
                            for v in result.get('Vulnerabilities', [])]
    # Raw findings are retained unchanged; the caller must qualify native identity
    # and apply the closed finding policy before any successful final report.
    return report

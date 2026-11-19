"""Scan the actual saved application image and its independently reconstructed wrapper metadata."""
import argparse
from collections import Counter
import datetime as dt
import json
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import advisory
import compiler_owners
import jll_advisory
import linked_advisory
import pcre2_advisory
import pcre2_cve
from readback import CA_ARCHIVE, CA_DATA, JLL_LIBRARIES


MANIFEST = 'opt/kanz/share/kanz/Manifest.toml'


def spdx_packages(report, expected):
    selected = {}
    for package in report['packages']:
        if package.get('sourceInfo') != 'package found in: ' + MANIFEST:
            continue
        name = package['name']
        item = expected.get(name, {})
        purls = [p['referenceLocator'] for p in package['externalRefs'] if p['referenceType'] == 'purl']
        if (name in selected or package['versionInfo'] != item.get('version')
                or purls != [f"pkg:julia/{name}@{item['version']}?uuid={item['uuid']}"]):
            raise ValueError('application SPDX identity differs')
        selected[name] = item
    if selected != expected:
        raise ValueError('application SPDX manifest incomplete')


def scan(image, receipt, runtime, tools, database, registry, publisher, output):
    output.mkdir()
    linked_advisory.validate_image(receipt, runtime)
    linked_advisory.validate_sources(publisher.parent.parent / 'linked', dt.datetime.now(dt.timezone.utc))
    print('Verified seven fix commits, three published libuv CVEs, the utf8proc release fix and two runtime ELF owners.')
    if set(receipt['wrapperMetadata']) != set(JLL_LIBRARIES):
        raise ValueError('publisher evidence missing from OCI receipt')
    owners = {}
    for owner, filenames in JLL_LIBRARIES.items():
        row = receipt['wrapperMetadata'][owner]
        actual = {'lib/' + name: receipt['native']['lib/julia/' + name]['sha256'] for name in filenames}
        if row['publisherFiles'] != actual:
            raise ValueError('readback publisher scope differs')
        owners[owner] = {k: row[k] for k in ('version', 'uuid')}
    if set(receipt['dataOwners']) != {'MozillaCACerts_jll'}:
        raise ValueError('CA bundle data owner missing')
    ca = receipt['dataOwners']['MozillaCACerts_jll']
    if ca['publisherArchiveSHA256'] != CA_ARCHIVE or ca['sha256'] != CA_DATA or ca['publisherVersion'] != '2026.10.20+0':
        raise ValueError('CA publisher binding differs')
    owners['MozillaCACerts_jll'] = {k: ca[k] for k in ('version', 'uuid')}
    pcre = receipt['wrapperMetadata']['PCRE2_jll']
    if pcre.get('upstreamVersion') != '10.49' or pcre.get('version') != '10.49.1+0':
        raise ValueError('PCRE2 vendor controls require the verified patched native runtime')
    controls = pcre2_advisory.validate(json.loads(publisher.read_bytes()),
                                       publisher.with_suffix('.api.json').read_bytes(),
                                       publisher.with_suffix('.policy.md').read_bytes(), dt.datetime.now(dt.timezone.utc))
    print('PCRE2 publisher controls:', len(controls), 'old-version findings; zero in the reviewed 10.49 ranges')
    pcre2_cve.validate(publisher.parent / 'cve', dt.datetime.now(dt.timezone.utc))
    for tool, archive_hash in advisory.ARCHIVES.items():
        advisory.tool_identity(tools / tool, archive_hash)
    metadata = json.loads((database / 'db' / 'metadata.json').read_bytes())
    advisory.validate_database(metadata, dt.datetime.now(dt.timezone.utc))
    before = advisory.digest(database / 'db' / 'trivy.db')
    jll_advisory.registry_identity(registry.read_bytes(), owners)
    jll_advisory.registry_identity(registry.read_bytes(), jll_advisory.CONTROL)

    def run(args, cwd=output):
        subprocess.run(list(map(str, args)), timeout=540, cwd=cwd, check=True,
                       stdin=subprocess.DEVNULL)

    ignored, config = output / 'empty.ignore', output / 'config.yaml'
    ignored.write_bytes(b'')
    config.write_bytes(b'{}\n')
    options = ['--cache-dir', database, '--cache-backend', 'memory', '--config', config, '--timeout', '8m',
               '--quiet', '--skip-db-update', '--skip-java-db-update', '--offline-scan', '--scanners', 'vuln',
               '--ignore-unfixed=false', '--ignorefile', ignored, '--list-all-pkgs', '--format', 'json']
    http = {'HTTP': {'uuid': advisory.HTTP_UUID, 'version': '1.10.15'}}
    results = {}
    for name, expected in [('http', http), ('jll-control', jll_advisory.CONTROL), ('wrappers', owners)]:
        src = output / name
        src.mkdir()
        (src / 'Manifest.toml').write_bytes(jll_advisory.manifest(expected))
        (src / 'Project.toml').write_bytes(jll_advisory.project(expected))
        path = output / (name + '.json')
        run([tools / 'trivy', 'fs', *options, '--output', path, '.'], cwd=src)
        doc = json.loads(path.read_bytes())
        result = advisory.manifest_result(doc, 'Manifest.toml')
        advisory.validate_packages(result, expected)
        results[name] = result
        if name == 'http':
            advisory.validate_control(doc)
    jll_advisory.validate_control(results['jll-control'])

    trivy, spdx, syft = [output / (name + '.json') for name in ('trivy', 'spdx', 'syft')]
    run([tools / 'trivy', 'image', *options, '--input', image, '--output', trivy])
    run([tools / 'trivy', 'convert', '--format', 'spdx-json', '--output', spdx, trivy])
    run([tools / 'syft', 'scan', 'oci-archive:' + str(image), '-o', 'syft-json=' + str(syft)])
    t = json.loads(trivy.read_bytes())
    s = json.loads(syft.read_bytes())
    if (t['Metadata']['ImageID'] != receipt['config'] or s['source']['metadata']['manifestDigest'] != receipt['manifest']
            or any(r.get('ExperimentalModifiedFindings') for r in t['Results'])):
        raise ValueError('OCI scanner identity or unmodified findings differ')
    advisory.validate_syft(s, receipt['config'])
    packages = advisory.package_inventory()
    application = advisory.manifest_result(t, MANIFEST)
    advisory.validate_packages(application, packages)
    spdx_packages(json.loads(spdx.read_bytes()), packages)
    os_results = [r for r in t['Results'] if r['Class'] == 'os-pkgs' and r['Type'] == 'debian']
    if len(t['Results']) != 2 or len(os_results) != 1 or not os_results[0].get('Packages'):
        raise ValueError('unexpected image scan scope')
    counts = Counter(v['Severity'] for v in os_results[0].get('Vulnerabilities', []))
    if before != advisory.digest(database / 'db' / 'trivy.db'):
        raise ValueError('scanner database changed during qualification')
    print('Debian findings:', dict(counts))
    if counts['CRITICAL'] or application.get('Vulnerabilities') or results['wrappers'].get('Vulnerabilities'):
        raise ValueError('application, native wrapper or critical OS advisory present')
    print('Verified seven application packages, eleven native wrapper records, the CA data owner and both positive controls.')
    compilers = compiler_owners.review(publisher.parent.parent / 'compilers', receipt)
    (output / 'compiler-owners.json').write_text(json.dumps(compilers, sort_keys=True, indent=2) + '\n')
    compiler_owners.require_reviewed_compilers(compilers)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ('image', 'readback', 'runtime', 'tools', 'database', 'registry', 'pcre2_publisher', 'output'):
        parser.add_argument(key, type=lambda x: Path(x).resolve())
    a = parser.parse_args()
    scan(a.image, json.loads(a.readback.read_bytes()), json.loads(a.runtime.read_bytes()), a.tools, a.database, a.registry, a.pcre2_publisher, a.output)

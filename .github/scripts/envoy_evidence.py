"""Actual same-pod edge qualification, without deployment or identity authority."""
import datetime as dt
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import uuid

from check_migration_evidence import verify

ROOT = Path(__file__).resolve().parents[2]
CASES = {'anonymous', 'session_gateway_signing', 'origin_csrf', 'encoded_identifiers',
         'principal_sanitization', 'request_body', 'cookies', 'admin_surface',
         'no_retry', 'quiet_stream', 'quiet_stream_negative_control', 'rate_limit', 'cleanup'}
REQUIRED = {
    'services/web-bff/internal/edgeconfig': {
        'TestInvalidProfilesDoNotReplaceExistingConfiguration',
        'TestWriterReplacesFinalSymlinkWithoutFollowingItsTarget'},
    'services/web-bff/cmd/web-bff': {'TestEdgeConfigurationDoesNotLoadApplicationCredentials'},
    'services/web-bff/internal/server': {'TestEnvoyEdgeActualBFF',
        *{'TestEnvoyEdgeActualBFF/' + name for name in CASES}},
}


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def read_json(path, limit=64 << 20):
    if not path.is_file() or path.stat().st_size > limit:
        raise ValueError('missing or oversized evidence')
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError('duplicate JSON key')
            result[key] = value
        return result
    return json.loads(path.read_text(encoding='utf-8'), object_pairs_hook=unique)


def validate_events(events, observed):
    verify(iter(events), REQUIRED)
    ids = {line.removeprefix('ENVOY_IMAGE_OBSERVED=') for e in events
           for line in e.get('Output', '').strip().splitlines()
           if line.startswith('ENVOY_IMAGE_OBSERVED=')}
    # Go t.Log prefixes the output with its source position.
    ids |= {match.group(1) for e in events for match in
            re.finditer(r'ENVOY_IMAGE_OBSERVED=(sha256:[0-9a-f]{64})(?:\s|$)', e.get('Output', ''))}
    if ids != {observed}:
        raise ValueError('actual test image identity differs')


def validate_scan(scan, observed):
    if scan.get('Trivy', {}).get('Version') != '0.72.0' or scan.get('Metadata', {}).get('ImageID') != observed:
        raise ValueError('image scanner identity differs')
    results = scan.get('Results')
    if not isinstance(results, list) or not results or not any(r.get('Class') == 'os-pkgs' and r.get('Packages') for r in results):
        raise ValueError('actual OS package inventory missing')
    findings = []
    for result in results:
        if result.get('ExperimentalModifiedFindings'):
            raise ValueError('suppressed image findings')
        for v in result.get('Vulnerabilities') or []:
            if v.get('Severity') not in {'UNKNOWN', 'LOW', 'MEDIUM', 'HIGH', 'CRITICAL'}:
                raise ValueError('unknown severity representation')
            findings.append({k: v.get(k, '') for k in ('VulnerabilityID', 'PkgName', 'InstalledVersion', 'Severity')})
    if any(v['Severity'] == 'CRITICAL' for v in findings):
        raise ValueError('critical actual-image advisory')
    return findings


def main():
    if os.environ.get('GITHUB_ACTIONS') != 'true' or sys.platform != 'linux' or os.environ.get('DOCKER_HOST') or os.environ.get('DOCKER_CONTEXT', 'default') != 'default':
        raise ValueError('requires ephemeral local Linux CI Docker')
    directory = Path(os.environ['ENVOY_EVIDENCE'])
    lock = read_json(ROOT / 'kanz/services/web-bff/internal/edgeconfig/image.json', 4096)
    image = lock['reference']
    report = {'passed': False, 'sourceSHA': '', 'image': lock,
              'limitations': ['synthetic-identity-and-gateway', 'no-public-tunnel-or-OIDC-provider-proof',
                              'no-network-hop-mTLS-or-deployment-acceptance',
                              'local-rate-budget-is-not-an-entitlement-control']}
    alias = 'kanz-isolation/envoy-advisory:kanz-tv-isolation-' + uuid.uuid4().hex[:12]
    stage = 'identity'
    tagged = False
    try:
        with tempfile.TemporaryDirectory(prefix='kanz-envoy-audit-') as temporary:
            scratch = Path(temporary)
            # Scanner defaults must not inherit ambient severities, exclusions,
            # credentials, user configuration or cataloger selection.
            child_env = {'PATH': os.environ['PATH'], 'HOME': str(scratch),
                         'TMPDIR': str(scratch), 'LANG': 'C.UTF-8'}
            def run(args, limit=128 << 20, timeout=600, cwd=None):
                with tempfile.TemporaryFile() as output:
                    result = subprocess.run(args, stdout=output, stderr=subprocess.DEVNULL, timeout=timeout,
                                            cwd=cwd or scratch, env=child_env, check=False)
                    if result.returncode or output.tell() > limit:
                        raise ValueError('qualification command refused or output exceeded bound')
                    output.seek(0)
                    return output.read().decode('utf-8')
            report['sourceSHA'] = run(['git', 'rev-parse', 'HEAD'], 128, cwd=ROOT).strip()
            report['treeSHA'] = run(['git', 'rev-parse', 'HEAD^{tree}'], 128, cwd=ROOT).strip()
            report['runID'] = os.environ['GITHUB_RUN_ID']
            report['runAttempt'] = os.environ['GITHUB_RUN_ATTEMPT']
            info = json.loads(run(['docker', 'image', 'inspect', image]))
            if len(info) != 1 or info[0].get('Os') != 'linux' or info[0].get('Architecture') != 'amd64':
                raise ValueError('image platform differs')
            observed = info[0]['Id']
            if not re.fullmatch('sha256:[0-9a-f]{64}', observed) or not any(
                    d.rsplit('@', 1)[-1] in {lock['index'], lock['manifest']} for d in info[0].get('RepoDigests', [])):
                raise ValueError('image digest binding differs')
            report['dockerImageID'] = observed
            stage = 'executed-cases'
            path = directory / 'tests.jsonl'
            if path.stat().st_size > 32 << 20:
                raise ValueError('oversized Go evidence')
            events = [json.loads(line) for line in path.read_text().splitlines()]
            validate_events(events, observed)
            report['testsSHA256'] = digest(path)
            report['requiredCases'] = sorted(CASES)
            # The subprocess CLI must also have produced a closed static config.
            cli = read_json(directory / 'cli.json', 64 << 10)
            if cli['admin']['address']['socket_address']['address'] != '127.0.0.1' or len(cli['static_resources']['listeners']) != 1:
                raise ValueError('fresh credential-independent CLI configuration absent')
            report['cliSHA256'] = digest(directory / 'cli.json')
            stage = 'archive-config-identity'
            module_path = ROOT / 'kanz/services/tv-sync/test/isolation.py'
            spec = importlib.util.spec_from_file_location('kanz_envoy_image_identity', module_path)
            identity = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(identity)
            if run(['docker', 'image', 'ls', '--filter', 'reference=' + alias, '--format', '{{.ID}}']).strip():
                raise ValueError('image alias exists')
            run(['docker', 'tag', image, alias]); tagged = True
            if run(['docker', 'image', 'inspect', alias, '--format', '{{.Id}}']).strip() != observed:
                raise ValueError('alias identity differs')
            archive = scratch / 'image.tar'
            run(['docker', 'save', '--output', str(archive), alias], timeout=180)
            config_digest = identity.archive_config_digest(archive, alias)
            if config_digest != lock['config'] or run(['docker', 'image', 'inspect', alias, '--format', '{{.Id}}']).strip() != observed:
                raise ValueError('archive identity differs')
            report['archiveConfigDigest'] = config_digest
            stage = 'fresh-image-advisories'
            config = scratch / 'trivy.yaml'; config.write_text('{}')
            ignore = scratch / 'empty.ignore'; ignore.write_text('')
            common = ['--cache-dir', str(scratch / 'cache'), '--config', str(config), '--timeout', '10m', '--quiet']
            run(['trivy', 'image', *common, '--download-db-only'])
            db = scratch / 'cache/db'
            metadata = read_json(db / 'metadata.json')
            now = dt.datetime.now(dt.timezone.utc)
            updated = dt.datetime.fromisoformat(metadata['UpdatedAt'].replace('Z', '+00:00'))
            next_update = dt.datetime.fromisoformat(metadata['NextUpdate'].replace('Z', '+00:00'))
            downloaded = dt.datetime.fromisoformat(metadata['DownloadedAt'].replace('Z', '+00:00'))
            if metadata['Version'] != 2 or not dt.timedelta(0) <= now - updated <= dt.timedelta(hours=24) or next_update < now or not updated <= downloaded <= now:
                raise ValueError('advisory database stale or invalid')
            db_hash = digest(db / 'trivy.db')
            scan = json.loads(run(['trivy', 'image', *common, '--skip-db-update', '--scanners', 'vuln',
                 '--ignore-unfixed=false', '--ignorefile', str(ignore), '--list-all-pkgs', '--format', 'json', '--image-src', 'docker', image]))
            report['findings'] = validate_scan(scan, observed)
            sbom = json.loads(run(['syft', 'scan', 'docker:' + image, '--quiet', '-o', 'json']))
            if sbom.get('descriptor', {}).get('version') != '1.56.0' or sbom.get('source', {}).get('type') != 'image' or sbom['source'].get('metadata', {}).get('imageID') != config_digest or not sbom.get('artifacts'):
                raise ValueError('SBOM config identity or inventory missing')
            report['inventory'] = [{k: p.get(k, '') for k in ('name', 'version', 'type', 'purl', 'foundBy')} for p in sbom['artifacts']]
            if digest(db / 'trivy.db') != db_hash:
                raise ValueError('advisory database changed')
            report['database'] = {**metadata, 'sha256': db_hash}
            report['scanTime'] = now.isoformat()
            report['passed'] = True
    except Exception as error:
        report.update(passed=False, failureStage=stage, errorType=type(error).__name__)
        raise
    finally:
        if tagged:
            try:
                subprocess.run(['docker', 'image', 'rm', alias], stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL, timeout=30, check=True)
                report['aliasCleanup'] = True
            except Exception:
                report.update(passed=False, aliasCleanup=False)
        (directory / 'qualification.json').write_text(json.dumps(report, indent=2) + '\n')
    if not report['passed'] or not report.get('aliasCleanup'):
        raise ValueError('qualification or cleanup failed')
    print('Actual Envoy/BFF qualification passed; no deployment authority granted')


if __name__ == '__main__':
    main()

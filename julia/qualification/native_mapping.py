"""One qualified native identity mapping; never a CVE ignore or general clearance."""
import datetime as dt
import copy
import hashlib
import json

import openssl_runtime

PATH = 'libexec/julia/7z'
BASE = {'size': 1654376, 'sha256': 'a15c6706c64672f07d4cc7358cc7eee3aa0d0f7cfe3fdfcf31deb808d3b0b8ee'}
ARCHIVE_SHA256 = '34237526097e68036fde82041b9e20f02c3026e53c3ff91666ba7c3c189b1fce'
FILE = {'size': 1650280, 'sha256': '05800b241166de20a1c423dde760407ba3a024cd41e1af02ad4b58fd120277a9'}
LICENSES = {'License.txt': {'size': 6341, 'sha256': '9ac2b4a97ab5d523965534d8b2d5868e511b39096d51fff458ab72c38b80fccc'},
            'copying.txt': {'size': 27032, 'sha256': '1e7e6bae5a5bde32f1ae5a7c37a082d1ab03cf89354f7f936ac40be9e39a6531'}}
COMPONENT = {'path': PATH, 'sha256': FILE['sha256'], 'version': '26.06', 'versionCommand': 'i',
             'archiveSHA256': ARCHIVE_SHA256, 'originalJLLVersion': '17.8.3+0', 'licenses': LICENSES}
CVE = 'CVE-2026-58052'
LZX_CVE = 'CVE-2026-125742'
CNA_URL = 'https://cveawg.mitre.org/api/cve/' + LZX_CVE
CNA_ID = '83251b91-4cc7-4094-a5c7-464a1b83ea10'
UUID = '3f19e933-33d8-53b3-aaab-bd5110c3b7a0'
NVD_URL = 'https://services.nvd.nist.gov/rest/json/cves/2.0?cveId=' + CVE
SOURCES = {
    'julia': {'url': 'https://raw.githubusercontent.com/JuliaLang/julia/3c3663cbe7b5193db8c7254536d61c68b8277a1b/deps/p7zip.version',
              'sha256': '49dc1d08428a63fb7189820ca85a144bf17f5561fda016866c93e0930589add3'},
    'upstream': {'url': 'https://raw.githubusercontent.com/ip7z/7zip/d3296e39e1f1f92c7b41c0c3848e65640428d42d/DOC/src-history.txt',
                 'sha256': '656dbbdc32905262cf673f7cff94b3a0721afd03c990f649e73cd98279b8e945'}}
CONFIG = [{'nodes': [{'operator': 'OR', 'negate': False, 'cpeMatch': [{
    'vulnerable': True, 'criteria': 'cpe:2.3:a:7-zip:7-zip:*:*:*:*:*:*:*:*',
    'versionEndExcluding': '26.03', 'matchCriteriaId': 'C80A3EAA-DC3C-4CBD-952B-106502AB69F0'}]}]}]
LIMIT = 1 << 20


def overlay(base, path):
    members = {'bin/7z': FILE, **{'share/licenses/p7zip/' + name: item for name, item in LICENSES.items()}}
    openssl_runtime.verified_artifact(path, ARCHIVE_SHA256, members)
    if (base['files'].get(PATH) != BASE or PATH in base['links'] or
            base['jll'].get('p7zip_jll', {}).get('version') != COMPONENT['originalJLLVersion']):
        raise ValueError('unexpected original Julia 7-Zip runtime or wrapper')
    result = copy.deepcopy(base)
    result['files'][PATH] = FILE.copy()
    return result


def validate_component(inventory, archive):
    if (inventory.get('native7zip') != COMPONENT or archive.get('files', {}).get(PATH) != FILE
            or inventory.get('nativeRuntime', {}).get('files', {}).get(PATH) != FILE):
        raise ValueError('actual native component version/hash/archive binding differs')


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError('duplicate advisory JSON key')
        value[key] = item
    return value


def timestamp(text):
    value = dt.datetime.fromisoformat(text.replace('Z', '+00:00'))
    return value.replace(tzinfo=dt.timezone.utc) if value.tzinfo is None else value


def nvd_document(raw, now):
    if not isinstance(raw, str) or not 0 < len(raw.encode()) <= LIMIT:
        raise ValueError('NVD document unbounded or absent')
    data = json.loads(raw, object_pairs_hook=unique_object)
    if (data.get('format') != 'NVD_CVE' or data.get('version') != '2.0'
            or data.get('totalResults') != 1 or data.get('startIndex') != 0
            or data.get('resultsPerPage') != 1 or len(data.get('vulnerabilities', [])) != 1):
        raise ValueError('NVD response scope differs')
    if not dt.timedelta(0) <= now - timestamp(data['timestamp']) <= dt.timedelta(hours=24):
        raise ValueError('NVD snapshot stale or future-dated')
    cve = data['vulnerabilities'][0]['cve']
    if (cve.get('id') != CVE or cve.get('vulnStatus') != 'Analyzed'
            or json.dumps(cve.get('configurations'), sort_keys=True) != json.dumps(CONFIG, sort_keys=True)
            or timestamp(cve['lastModified']) > timestamp(data['timestamp'])):
        raise ValueError('unreviewed native advisory identity/range/shape')
    return cve


def affected(version, cve):
    # Closed reviewed version domain; no guessed CPE or generic version coercion.
    if version not in ('26.02', '26.03', '26.06') or json.dumps(cve.get('configurations'), sort_keys=True) != json.dumps(CONFIG, sort_keys=True):
        raise ValueError('native version or range is unknown')
    end = cve['configurations'][0]['nodes'][0]['cpeMatch'][0]['versionEndExcluding']
    return tuple(map(int, version.split('.'))) < tuple(map(int, end.split('.')))


def validate_sources(raw_sources):
    if set(raw_sources) != set(SOURCES):
        raise ValueError('native primary sources missing')
    for name, item in SOURCES.items():
        raw = raw_sources[name]
        if len(raw) > LIMIT or hashlib.sha256(raw).hexdigest() != item['sha256']:
            raise ValueError('native primary source checksum differs')
    if b'P7ZIP_VER := 26.06' not in raw_sources['julia']:
        raise ValueError('Julia native version differs')
    current = raw_sources['upstream'].split(b'26.02', 1)[0]
    if b'26.06' not in current or b'26.03' not in current or CVE.encode() not in current:
        raise ValueError('upstream fixed-version provenance differs')


def cab_affected(version, raw, checked):
    if version not in ('26.05', '26.06') or not isinstance(raw, str) or not 0 < len(raw.encode()) <= LIMIT:
        raise ValueError('unknown CAB advisory input')
    record = json.loads(raw, object_pairs_hook=unique_object)
    meta, cna = record['cveMetadata'], record['containers']['cna']
    affected_product = [{'vendor': '7-Zip', 'product': '7-Zip', 'repo': 'https://github.com/ip7z/7zip',
                         'defaultStatus': 'unaffected', 'versions': [{'version': '0', 'lessThan': '26.06',
                         'versionType': 'custom', 'status': 'affected'}]}]
    if (record.get('dataType') != 'CVE_RECORD' or record.get('dataVersion') != '5.2'
            or meta.get('cveId') != LZX_CVE or meta.get('state') != 'PUBLISHED'
            or meta.get('assignerOrgId') != CNA_ID or cna['providerMetadata'].get('orgId') != CNA_ID
            or cna.get('affected') != affected_product or timestamp(meta['dateUpdated']) > checked):
        raise ValueError('unreviewed CAB advisory identity or affected range')
    end = cna['affected'][0]['versions'][0]['lessThan']
    return tuple(map(int, version.split('.'))) < tuple(map(int, end.split('.')))


def validate_report(report, inventory, now):
    validate_component(inventory, {'files': {PATH: FILE}}) # full archive equality is also required by proof.validate_report
    if (not isinstance(report, dict) or set(report) != {'component', 'sources', 'sourceBytes', 'nvd', 'cabAdvisory', 'controls', 'status', 'checkedAt'}
            or report.get('component') != COMPONENT or report.get('sources') != SOURCES
            or report.get('controls') != {'26.02': 'affected', '26.03': 'fixed'}
            or report.get('status') != 'fixed-native-version-for-reviewed-advisory'):
        raise ValueError('native mapping evidence missing or unbound')
    validate_sources({key: value.encode('utf-8') for key, value in report['sourceBytes'].items()})
    checked = timestamp(report['checkedAt'])
    if not dt.timedelta(0) <= checked - now <= dt.timedelta(hours=1):
        raise ValueError('native mapping outside qualification window')
    nvd = report['nvd']
    if (set(nvd) != {'url', 'raw', 'sha256'} or nvd['url'] != NVD_URL
            or not isinstance(nvd['raw'], str) or len(nvd['raw'].encode()) > LIMIT
            or hashlib.sha256(nvd['raw'].encode()).hexdigest() != nvd['sha256']):
        raise ValueError('native advisory snapshot hash differs')
    cve = nvd_document(nvd['raw'], checked)
    if not affected('26.02', cve) or affected('26.03', cve) or affected(report['component']['version'], cve):
        raise ValueError('native advisory controls or actual fixed version failed')
    cab = report['cabAdvisory']
    if (set(cab) != {'url', 'raw', 'sha256', 'controls'} or cab['url'] != CNA_URL
            or cab['controls'] != {'26.05': 'affected', '26.06': 'fixed'}
            or not cab_affected('26.05', cab['raw'], checked)
            or cab_affected(report['component']['version'], cab['raw'], checked)
            or hashlib.sha256(cab['raw'].encode()).hexdigest() != cab['sha256']):
        raise ValueError('CAB advisory controls or actual fixed version failed')


def resolves(finding):
    # Only this complete package/version/advisory identity can use the separately
    # validated native mapping; every other wrapper finding remains a refusal.
    return (set(finding) == {'VulnerabilityID', 'PkgName', 'PkgID', 'InstalledVersion', 'Severity'}
            and finding['VulnerabilityID'] == CVE and finding['PkgName'] == 'p7zip_jll'
            and finding['PkgID'] == UUID and finding['InstalledVersion'] == '17.8.3+0'
            and finding['Severity'] == 'MEDIUM')


def qualify(run, inventory, now):
    def fetch(url):
        return run(['curl', '--fail', '--silent', '--show-error', '--location', '--proto', '=https',
                    '--proto-redir', '=https', '--max-time', '60', url], limit=LIMIT)
    raw_sources = {name: fetch(item['url']) for name, item in SOURCES.items()}
    validate_sources(raw_sources)
    raw = fetch(NVD_URL).decode('utf-8')
    cab = fetch(CNA_URL).decode('utf-8')
    checked = now()
    report = {'component': inventory['native7zip'], 'sources': SOURCES,
              'sourceBytes': {key: value.decode('utf-8') for key, value in raw_sources.items()},
              'checkedAt': checked.isoformat(),
              'nvd': {'url': NVD_URL, 'raw': raw, 'sha256': hashlib.sha256(raw.encode()).hexdigest()},
              'cabAdvisory': {'url': CNA_URL, 'raw': cab, 'sha256': hashlib.sha256(cab.encode()).hexdigest(),
                              'controls': {'26.05': 'affected', '26.06': 'fixed'}},
              'controls': {'26.02': 'affected', '26.03': 'fixed'},
              'status': 'fixed-native-version-for-reviewed-advisory'}
    # Compare the newly retrieved snapshot with retrieval completion, not the
    # earlier Trivy scan start (NVD query timestamps may be later than that).
    validate_report(report, inventory, checked)
    return report

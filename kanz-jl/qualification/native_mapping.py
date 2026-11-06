"""One qualified native identity mapping; never a CVE ignore or general clearance."""
import datetime as dt
import hashlib
import json

PATH = 'libexec/julia/7z'
FILE = {'size': 1654376, 'sha256': 'a15c6706c64672f07d4cc7358cc7eee3aa0d0f7cfe3fdfcf31deb808d3b0b8ee'}
COMPONENT = {'path': PATH, 'sha256': FILE['sha256'], 'version': '26.03', 'versionCommand': 'i'}
CVE = 'CVE-2026-58052'
UUID = '3f19e933-33d8-53b3-aaab-bd5110c3b7a0'
NVD_URL = 'https://services.nvd.nist.gov/rest/json/cves/2.0?cveId=' + CVE
SOURCES = {
    'julia': {'url': 'https://raw.githubusercontent.com/JuliaLang/julia/67ac47b9cd855e02134d41d5428ac9f2a73f2694/deps/p7zip.version',
              'sha256': '1a5819d805a4f2d22da27b72b60c725358139d334dfa1f7a64d4ff1b899c81bd'},
    'upstream': {'url': 'https://raw.githubusercontent.com/ip7z/7zip/0766b733fe3e06dd2a7f9a3cfbf2108ac73abd17/DOC/src-history.txt',
                 'sha256': 'c1423bb2733abe901a4250a69396cf616a1c1c1b6245eb025484edda29f33c0d'}}
CONFIG = [{'nodes': [{'operator': 'OR', 'negate': False, 'cpeMatch': [{
    'vulnerable': True, 'criteria': 'cpe:2.3:a:7-zip:7-zip:*:*:*:*:*:*:*:*',
    'versionEndExcluding': '26.03', 'matchCriteriaId': 'C80A3EAA-DC3C-4CBD-952B-106502AB69F0'}]}]}]
LIMIT = 1 << 20


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
    if version not in ('26.02', '26.03') or json.dumps(cve.get('configurations'), sort_keys=True) != json.dumps(CONFIG, sort_keys=True):
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
    if b'P7ZIP_VER := 26.03' not in raw_sources['julia']:
        raise ValueError('Julia native version differs')
    current = raw_sources['upstream'].split(b'26.02', 1)[0]
    if b'26.03' not in current or CVE.encode() not in current:
        raise ValueError('upstream fixed-version provenance differs')


def validate_report(report, inventory, now):
    validate_component(inventory, {'files': {PATH: FILE}}) # full archive equality is also required by proof.validate_report
    if (not isinstance(report, dict) or set(report) != {'component', 'sources', 'sourceBytes', 'nvd', 'controls', 'status', 'checkedAt'}
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
    checked = now()
    report = {'component': inventory['native7zip'], 'sources': SOURCES,
              'sourceBytes': {key: value.decode('utf-8') for key, value in raw_sources.items()},
              'checkedAt': checked.isoformat(),
              'nvd': {'url': NVD_URL, 'raw': raw, 'sha256': hashlib.sha256(raw.encode()).hexdigest()},
              'controls': {'26.02': 'affected', '26.03': 'fixed'},
              'status': 'fixed-native-version-for-reviewed-advisory'}
    # Compare the newly retrieved snapshot with retrieval completion, not the
    # earlier Trivy scan start (NVD query timestamps may be later than that).
    validate_report(report, inventory, checked)
    return report

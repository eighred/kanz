"""Cross-check the vendor directory against NVD discovery and CVE Program records."""
from concurrent.futures import ThreadPoolExecutor
import datetime as dt
import hashlib
import json
import re
import time

SOURCE = 'openssl-security@openssl.org'
CNA = '3a12439a-ef3a-4c79-92e6-6081a721f1e5'
DISCOVERY = 'https://services.nvd.nist.gov/rest/json/cves/2.0?sourceIdentifier=openssl-security%40openssl.org&resultsPerPage=512'
REGISTRY = 'https://cveawg.mitre.org/api/cve/'
DISCOVERY_LIMIT = 8 << 20
RECORD_LIMIT = 128 << 10


def pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            raise ValueError('duplicate advisory JSON key')
        result[key] = value
    return result


def missing_records(directory_names, raw, now):
    if not isinstance(raw, str) or not 0 < len(raw.encode()) <= DISCOVERY_LIMIT:
        raise ValueError('invalid OpenSSL source discovery size')
    page = json.loads(raw, object_pairs_hook=pairs)
    rows = page.get('vulnerabilities')
    if (page.get('format') != 'NVD_CVE' or page.get('version') != '2.0' or page.get('startIndex') != 0
            or not isinstance(rows, list) or not 203 <= len(rows) <= 512
            or page.get('totalResults') != len(rows) or page.get('resultsPerPage') != len(rows)):
        raise ValueError('incomplete OpenSSL NVD source census')
    stamp = dt.datetime.fromisoformat(page['timestamp'])
    if stamp.tzinfo is None:  # The NVD API returns UTC without a suffix.
        stamp = stamp.replace(tzinfo=dt.timezone.utc)
    if now.tzinfo is None or abs(now - stamp) > dt.timedelta(hours=1):
        raise ValueError('stale OpenSSL NVD source census')
    states = {}
    for row in rows:
        item = row['cve']
        ident = item['id']
        status = item['vulnStatus']
        if (item.get('sourceIdentifier') != SOURCE or not re.fullmatch(r'CVE-[0-9]{4}-[0-9]+', ident)
                or ident in states or status not in {'Received', 'Awaiting Analysis', 'Undergoing Analysis',
                                                      'Analyzed', 'Modified', 'Deferred', 'Rejected'}):
            raise ValueError('ambiguous OpenSSL NVD source record')
        states[ident] = 'REJECTED' if status == 'Rejected' else 'PUBLISHED'
    if states.get('CVE-2026-121482') != 'PUBLISHED':
        raise ValueError('known missing-directory discovery control absent')
    listed = {name[:-5].upper() for name in directory_names}
    if any(states[ident] != 'PUBLISHED' for ident in states.keys() & listed):
        raise ValueError('vendor directory and CVE discovery states disagree')
    return {ident: state for ident, state in states.items() if ident not in listed}


def fetch(run, names, now):
    deadline = time.monotonic() + 120
    def read(url, limit):
        if time.monotonic() >= deadline:
            raise ValueError('OpenSSL cross-index retrieval deadline exceeded')
        return run(['curl', '--disable', '--fail', '--silent', '--show-error', '--proto', '=https',
                    '--max-time', '35', url], limit=limit).decode()
    raw = read(DISCOVERY, DISCOVERY_LIMIT)
    missing = sorted(missing_records(names, raw, now()))
    records = {}
    total = 0
    with ThreadPoolExecutor(max_workers=8) as pool:
        for offset in range(0, len(missing), 8):
            batch = missing[offset:offset + 8]
            for ident, text in zip(batch, pool.map(lambda key: read(REGISTRY + key, RECORD_LIMIT), batch)):
                total += len(text.encode())
                if total > 16 << 20 or time.monotonic() >= deadline:
                    raise ValueError('OpenSSL CVE registry acquisition bound exceeded')
                records[ident] = text
    return {'sourcePage': raw, 'sourcePageSHA256': hashlib.sha256(raw.encode()).hexdigest(),
            'records': records, 'recordSHA256': {key: hashlib.sha256(text.encode()).hexdigest() for key, text in records.items()}}


def published_records(names, evidence, now):
    if not isinstance(evidence, dict) or set(evidence) != {'sourcePage', 'sourcePageSHA256', 'records', 'recordSHA256'}:
        raise ValueError('OpenSSL cross-index evidence missing')
    raw, records = evidence['sourcePage'], evidence['records']
    missing = missing_records(names, raw, now)
    if (not isinstance(records, dict) or records.keys() != missing.keys()
            or hashlib.sha256(raw.encode()).hexdigest() != evidence['sourcePageSHA256']):
        raise ValueError('incomplete OpenSSL CVE registry coverage')
    parsed, hashes, total = {}, {}, 0
    for ident, text in records.items():
        if not isinstance(text, str) or not 0 < len(text.encode()) <= RECORD_LIMIT:
            raise ValueError('invalid OpenSSL registry record size')
        total += len(text.encode())
        if total > 16 << 20:
            raise ValueError('oversized OpenSSL registry evidence')
        hashes[ident] = hashlib.sha256(text.encode()).hexdigest()
        record = json.loads(text, object_pairs_hook=pairs)
        meta = record.get('cveMetadata', {})
        owner = record.get('containers', {}).get('cna', {}).get('providerMetadata', {})
        if (record.get('dataType') != 'CVE_RECORD' or record.get('dataVersion') not in {'5.0', '5.1', '5.2'}
                or meta.get('cveId') != ident or meta.get('state') != missing[ident]
                or meta.get('assignerOrgId') != CNA or owner.get('orgId') != CNA
                or owner.get('shortName') != 'openssl' or meta.get('assignerShortName') != 'openssl'):
            raise ValueError('CVE registry publisher, state or identity differs')
        if meta['state'] == 'PUBLISHED':
            parsed[ident.lower() + '.json'] = text
    if hashes != evidence['recordSHA256']:
        raise ValueError('OpenSSL CVE registry record digest differs')
    return parsed

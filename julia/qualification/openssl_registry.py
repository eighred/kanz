"""Cross-check the vendor directory against NVD discovery and CVE Program records."""
from concurrent.futures import ThreadPoolExecutor
import datetime as dt
import hashlib
import json
from pathlib import Path
import re
import time

SOURCE = 'openssl-security@openssl.org'
CNA = '3a12439a-ef3a-4c79-92e6-6081a721f1e5'
DISCOVERY = 'https://services.nvd.nist.gov/rest/json/cves/2.0?sourceIdentifier=openssl-security%40openssl.org&resultsPerPage=512'
REGISTRY = 'https://cveawg.mitre.org/api/cve/'
DISCOVERY_LIMIT = 8 << 20
RECORD_LIMIT = 128 << 10
DISCOVERY_CONTROLS = frozenset({'CVE-2026-107494', 'CVE-2026-121481', 'CVE-2026-121482',
                                'CVE-2026-84786', 'CVE-2026-95690'})


def pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            raise ValueError('duplicate advisory JSON key')
        result[key] = value
    return result


def source_states(raw, now):
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
    if not DISCOVERY_CONTROLS.issubset(states):
        raise ValueError('known OpenSSL discovery control absent')
    return states


def fetch(run, now):
    deadline = time.monotonic() + 120
    def read(url, limit):
        if time.monotonic() >= deadline:
            raise ValueError('OpenSSL cross-index retrieval deadline exceeded')
        return run(['curl', '--disable', '--fail', '--silent', '--show-error', '--proto', '=https',
                    '--max-time', '35', url], limit=limit).decode()
    raw = read(DISCOVERY, DISCOVERY_LIMIT)
    ids = sorted(source_states(raw, now()))
    records = {}
    total = 0
    with ThreadPoolExecutor(max_workers=8) as pool:
        for offset in range(0, len(ids), 8):
            batch = ids[offset:offset + 8]
            for ident, text in zip(batch, pool.map(lambda key: read(REGISTRY + key, RECORD_LIMIT), batch)):
                total += len(text.encode())
                if total > 16 << 20 or time.monotonic() >= deadline:
                    raise ValueError('OpenSSL CVE registry acquisition bound exceeded')
                records[ident] = text
    return {'sourcePage': raw, 'sourcePageSHA256': hashlib.sha256(raw.encode()).hexdigest(),
            'records': records, 'recordSHA256': {key: hashlib.sha256(text.encode()).hexdigest() for key, text in records.items()}}


def published_records(vendor_records, evidence, now):
    if not isinstance(evidence, dict) or set(evidence) != {'sourcePage', 'sourcePageSHA256', 'records', 'recordSHA256'}:
        raise ValueError('OpenSSL cross-index evidence missing')
    raw, records = evidence['sourcePage'], evidence['records']
    states = source_states(raw, now)
    if (not isinstance(records, dict) or records.keys() != states.keys()
            or hashlib.sha256(raw.encode()).hexdigest() != evidence['sourcePageSHA256']):
        raise ValueError('incomplete OpenSSL CVE registry coverage')
    # Historical registry records use prose. Bind both original affected projections
    # before using the vendor's machine-readable ranges for these exact pairs.
    history = json.loads(Path(__file__).with_name('openssl_historical_ranges.json').read_bytes(),
                         object_pairs_hook=pairs)
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
                or meta.get('cveId') != ident or meta.get('state') != states[ident]
                or meta.get('assignerOrgId') != CNA or owner.get('orgId') != CNA
                or owner.get('shortName') != 'openssl' or meta.get('assignerShortName') != 'openssl'):
            raise ValueError('CVE registry publisher, state or identity differs')
        name = ident.lower() + '.json'
        if name in vendor_records:
            if meta['state'] != 'PUBLISHED':
                raise ValueError('OpenSSL vendor and CVE registry states disagree')
            vendor = json.loads(vendor_records[name], object_pairs_hook=pairs)
            vendor_ranges = vendor['containers']['cna']['affected']
            registry_ranges = record['containers']['cna']['affected']
            if registry_ranges != vendor_ranges:
                projections = json.dumps({'vendor': vendor_ranges, 'registry': registry_ranges},
                                         sort_keys=True, separators=(',', ':')).encode()
                if hashlib.sha256(projections).hexdigest() != history.get(ident):
                    raise ValueError('unreviewed OpenSSL affected range disagreement')
        elif meta['state'] == 'PUBLISHED':
            parsed[name] = text
    if hashes != evidence['recordSHA256']:
        raise ValueError('OpenSSL CVE registry record digest differs')
    return parsed

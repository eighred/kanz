"""Supplement the PCRE2 project feed with official CVE records and exact-version NVD controls."""
import datetime as dt
import json
from pathlib import Path
from urllib.parse import urlencode

from pcre2_advisory import fetch, fresh


RANGES = {
    'CVE-2026-103111': ('0', '10.49'),
    'CVE-2026-86145': ('10.32', '10.48'),
    'CVE-2026-89156': ('10.34', '10.48'),
    'CVE-2026-89157': ('10.30', '10.48'),
    'CVE-2026-89158': ('0', '10.48'),
    'CVE-2026-89160': ('10.34', '10.48'),
    'CVE-2026-89161': ('0', '10.48'),
    'CVE-2026-89162': ('10.45', '10.48'),
}
NVD = {'10.47': {'CVE-2026-89156', 'CVE-2026-89157', 'CVE-2026-89158', 'CVE-2026-89160',
                 'CVE-2026-89161', 'CVE-2026-89162'}, '10.49': set()}


def sources():
    cves = {identifier: 'https://cveawg.mitre.org/api/cve/' + identifier for identifier in RANGES}
    nvd = {version: 'https://services.nvd.nist.gov/rest/json/cves/2.0?' +
           urlencode({'cpeName': 'cpe:2.3:a:pcre:pcre2:' + version + ':*:*:*:*:*:*:*', 'isVulnerable': ''})
           for version in NVD}
    return {**cves, **nvd}


def validate_cna(identifier, doc):
    meta, cna = doc['cveMetadata'], doc['containers']['cna']
    low, high = RANGES[identifier]
    affected = cna['affected']
    expected = [{'version': low, 'lessThan': high, 'status': 'affected', 'versionType': 'custom'}]
    if (meta['cveId'] != identifier or meta['state'] != 'PUBLISHED' or cna['providerMetadata']['shortName'] != 'mitre'
            or len(affected) != 1 or affected[0]['vendor'] != 'PCRE' or affected[0]['product'] != 'PCRE2'
            or affected[0]['defaultStatus'] != 'unaffected' or affected[0]['versions'] != expected):
        raise ValueError('PCRE2 CNA ownership or affected ranges changed')
    interval = tuple(tuple(map(int, text.split('.'))) for text in (low, high))
    if not interval[0] <= (10, 47) < interval[1] or interval[0] <= (10, 49) < interval[1]:
        raise ValueError('PCRE2 CNA control result differs')


def validate(directory, now):
    dates = json.loads((directory / 'dates.json').read_bytes())
    if set(dates) != set(sources()):
        raise ValueError('incomplete PCRE2 CVE source acquisition')
    for name, source in sources().items():
        entry = dates[name]
        if entry['url'] != source:
            raise ValueError('unexpected PCRE2 advisory source')
        fresh(entry['date'], now)
        raw = (directory / (name + '.json')).read_bytes()
        if len(raw) > 2 << 20:
            raise ValueError('oversized PCRE2 CVE input')
        doc = json.loads(raw)
        if name in RANGES:
            validate_cna(name, doc)
        else:
            stamp = dt.datetime.fromisoformat(doc['timestamp'].replace('Z', '+00:00'))
            fresh(stamp.replace(tzinfo=dt.timezone.utc).isoformat(), now)
            rows = [item['cve']['id'] for item in doc['vulnerabilities']]
            if (doc['format'] != 'NVD_CVE' or doc['version'] != '2.0' or doc['startIndex'] != 0
                    or doc['totalResults'] != len(rows) or len(set(rows)) != len(rows) or set(rows) != NVD[name]):
                raise ValueError('PCRE2 exact-version NVD control changed')


def collect(directory):
    directory.mkdir()
    dates = {}
    for name, url in sources().items():
        raw, date = fetch(url, 2 << 20)
        (directory / (name + '.json')).write_bytes(raw)
        dates[name] = {'url': url, 'date': date}
    (directory / 'dates.json').write_text(json.dumps(dates, sort_keys=True) + '\n')
    validate(directory, dt.datetime.now(dt.timezone.utc))
    print('Verified eight PCRE2 CNA records and both exact-version NVD CPE controls.')

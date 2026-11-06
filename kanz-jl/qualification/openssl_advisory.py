"""Fresh complete OpenSSL CNA catalog, scoped to the installed native component."""
from concurrent.futures import ThreadPoolExecutor
import datetime as dt
import hashlib
from html.parser import HTMLParser
import json
import re
import time

import openssl_runtime

URL = 'https://openssl-library.org/news/secjson/'
SCOPE = 'OpenSSL-native-component-only-other-native-ownership-unqualified'


def pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            raise ValueError('duplicate advisory JSON key')
        result[key] = value
    return result


def version(value):
    if not isinstance(value, str) or not re.fullmatch(r'[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}', value):
        raise ValueError('unknown OpenSSL semantic version')
    return tuple(map(int, value.split('.')))


def affected(item, target):
    if set(item) == {'version', 'status'} and item['status'] == 'unaffected':
        version(item['version'])
        return False
    keys = set(item)
    upper = 'lessThan' if 'lessThan' in keys else 'lessThanOrEqual'
    if keys != {'version', upper, 'status', 'versionType'} or item['status'] != 'affected':
        raise ValueError('unknown OpenSSL affected range')
    lower, end = item['version'], item[upper]
    if item['versionType'] == 'custom':
        # Historical letter-suffixed 0.x/1.x and FIPS1 ranges are disjoint
        # from this fixed 3.5 component. Never order their letter suffixes.
        old = r'(?:fips-)?[01]\.[0-9]{1,2}\.[0-9]{1,2}(?:[a-z]{1,2}|-beta[0-9]|-cvs)?'
        if upper != 'lessThan' or not all(isinstance(v, str) and re.fullmatch(old, v) for v in (lower, end)):
            raise ValueError('unknown historical OpenSSL range')
        return False
    if item['versionType'] != 'semver':
        raise ValueError('unknown OpenSSL range type')
    low, high = version(lower), version(end)
    if low > high:
        raise ValueError('inverted OpenSSL range')
    value = version(target)
    return low <= value and (value < high if upper == 'lessThan' else value <= high)


def index_names(index):
    if not isinstance(index, str) or len(index.encode()) > 256 << 10:
        raise ValueError('missing or oversized OpenSSL index')
    class Links(HTMLParser):
        def __init__(self):
            super().__init__(convert_charrefs=True)
            self.names = []

        def handle_starttag(self, tag, attrs):
            hrefs = [value for name, value in attrs if name.lower() == 'href']
            if len(hrefs) > 1:
                raise ValueError('ambiguous OpenSSL catalog link')
            for link in hrefs:
                if not isinstance(link, str):
                    raise ValueError('missing OpenSSL catalog link value')
                if re.fullmatch(r'cve-[0-9]{4}-[0-9]+\.json', link):
                    self.names.append(link)
                elif link == 'statements.json':
                    # Official directory also links historical support-status
                    # and disputed-issue prose, not a published CNA record.
                    continue
                elif '.json' in link.lower() or 'cve-' in link.lower():
                    raise ValueError('unrecognized OpenSSL catalog entry')

    parser = Links()
    parser.feed(index)
    parser.close()
    names = parser.names
    if not 297 <= len(names) <= 1024 or len(set(names)) != len(names):
        raise ValueError('incomplete or ambiguous OpenSSL index')
    return names


def catalog(index, records, target):
    names = index_names(index)
    if not isinstance(records, dict) or set(records) != set(names):
        raise ValueError('OpenSSL catalog inventory differs')
    if sum(len(raw.encode()) for raw in records.values() if isinstance(raw, str)) > 16 << 20:
        raise ValueError('oversized OpenSSL catalog')
    matches = []
    for name, raw in sorted(records.items()):
        if not isinstance(raw, str) or not 0 < len(raw.encode()) <= 64 << 10:
            raise ValueError('invalid OpenSSL advisory size')
        record = json.loads(raw, object_pairs_hook=pairs)
        ident = name[:-5].upper()
        meta = record.get('cveMetadata', {})
        cna = record.get('containers', {}).get('cna', {})
        if (record.get('dataType') != 'CVE_RECORD' or record.get('dataVersion') not in ('5.0', '5.1')
                or meta.get('cveId') != ident or meta.get('state') != 'PUBLISHED'
                or cna.get('providerMetadata', {}).get('shortName') != 'openssl'):
            raise ValueError('unrecognized OpenSSL CNA record')
        products = cna.get('affected')
        if not isinstance(products, list) or len(products) != 1:
            raise ValueError('ambiguous OpenSSL affected product')
        product = products[0]
        if (product.get('vendor') != 'OpenSSL' or product.get('product') != 'OpenSSL'
                or product.get('defaultStatus') != 'unaffected'
                or set(product) != {'vendor', 'product', 'defaultStatus', 'versions'}):
            raise ValueError('unknown OpenSSL product/default')
        ranges = product['versions']
        if not isinstance(ranges, list) or not 0 < len(ranges) <= 64:
            raise ValueError('unbounded OpenSSL ranges')
        # Evaluate every range, including later malformed ranges after a match.
        results = [affected(item, target) for item in ranges]
        if any(results):
            matches.append(ident)
    return matches


def validate_report(report, inventory, scan_time):
    openssl_runtime.validate_component(inventory)
    if not isinstance(report, dict) or set(report) != {
            'index', 'records', 'indexSHA256', 'recordSHA256', 'checkedAt', 'scope',
            'version', 'affectedControlVersion', 'affectedControl', 'advisories'}:
        raise ValueError('incomplete native OpenSSL advisory evidence')
    checked = dt.datetime.fromisoformat(report['checkedAt'])
    if checked.tzinfo is None or abs(checked - scan_time) > dt.timedelta(hours=1):
        raise ValueError('OpenSSL catalog outside qualification window')
    index, records = report['index'], report['records']
    actual = catalog(index, records, openssl_runtime.VERSION)
    control = catalog(index, records, '3.5.8')
    if (report['scope'] != SCOPE or report['version'] != openssl_runtime.VERSION
            or report['affectedControlVersion'] != '3.5.8'
            or 'CVE-2026-84782' not in control or report['affectedControl'] != control
            or actual or report['advisories'] != actual
            or report['indexSHA256'] != hashlib.sha256(index.encode()).hexdigest()
            or report['recordSHA256'] != {k: hashlib.sha256(v.encode()).hexdigest() for k, v in records.items()}):
        raise ValueError('native OpenSSL advisory/control/identity refusal')


def qualify(run, inventory, now, report=None):
    if report is None:
        report = {}
    deadline = time.monotonic() + 120
    def fetch(name):
        if time.monotonic() >= deadline:
            raise ValueError('OpenSSL catalog retrieval deadline exceeded')
        return run(['curl', '--fail', '--silent', '--show-error', '--proto', '=https',
                    '--max-time', '45', URL + name], limit=(64 << 10) if name else (256 << 10)).decode('utf-8')
    index = fetch('')
    names = index_names(index)
    with ThreadPoolExecutor(max_workers=8) as pool:
        records = {}
        total = 0
        # At most eight result buffers exist beyond the retained catalog. Do not
        # submit the complete index: completed futures retain their result bytes.
        for start in range(0, len(names), 8):
            batch = names[start:start + 8]
            for name, raw in zip(batch, pool.map(fetch, batch)):
                total += len(raw.encode())
                if total > 16 << 20 or time.monotonic() >= deadline:
                    raise ValueError('OpenSSL catalog retrieval bound exceeded')
                records[name] = raw
    report.update({'index': index, 'records': records,
              'indexSHA256': hashlib.sha256(index.encode()).hexdigest(),
              'recordSHA256': {k: hashlib.sha256(v.encode()).hexdigest() for k, v in records.items()},
              'checkedAt': now().isoformat(), 'scope': SCOPE, 'version': openssl_runtime.VERSION,
              'affectedControlVersion': '3.5.8'})
    report['affectedControl'] = catalog(index, records, '3.5.8')
    report['advisories'] = catalog(index, records, openssl_runtime.VERSION)
    validate_report(report, inventory, now())
    return report

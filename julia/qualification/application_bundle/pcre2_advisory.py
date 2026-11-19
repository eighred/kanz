"""Collect the current PCRE2 publisher ranges outside the offline compiler and worker."""
import argparse
import datetime as dt
from email.utils import parsedate_to_datetime
import hashlib
import json
from pathlib import Path
from urllib.request import Request, urlopen


API = 'https://api.github.com/repos/PCRE2Project/pcre2/security-advisories?per_page=100'
POLICY = 'https://raw.githubusercontent.com/PCRE2Project/pcre2/main/SECURITY.md'
POLICY_HASH = '80c8a1be357ae86e25e1477ad0d842391f30e179828886677ed572a3d4c5daec'
# Bounds apply to the two observed native versions. The third row value is the
# first fixed upstream version; the wrapper build suffix has no upstream meaning.
RANGES = {
    'GHSA-r9hj-j2rw-4q3m': ('CVE-2026-103111', '<=10.48', '10.49'),
    'GHSA-2p8c-ff85-vh9x': ('CVE-2026-89156', '<= 10.47', '10.48'),
    'GHSA-3r4p-g7gg-ppmf': ('CVE-2026-86145', '>= 10.32, <= 10.47', '10.48'),
    'GHSA-q8g2-wprr-34m9': ('CVE-2026-89157', '>= 10.30, <= 10.47', '10.48'),
    'GHSA-9qww-pwc4-77qq': ('CVE-2026-89160', '>= 10.34, <= 10.47', '10.48'),
    'GHSA-fmgr-6ggq-9859': ('CVE-2026-89158', '<= 10.47', '10.48'),
    'GHSA-q7rw-r7qq-2hx6': (None, '>= 10.45', '10.48'),
    'GHSA-43gg-c3xx-7w7g': (None, '< 10.47', '10.47'),
    'GHSA-c2gv-xgf5-5cc2': ('CVE-2025-58050', '10.45', '10.46'),
}


def fresh(date, now):
    if not dt.timedelta(0) <= now - dt.datetime.fromisoformat(date) <= dt.timedelta(hours=24):
        raise ValueError('publisher response stale or future-dated')


def affected(row, version):
    if version not in ('10.47', '10.49') or row not in RANGES.values():
        raise ValueError('unreviewed PCRE2 version or advisory semantics')
    # The supported controls are 10.47 and 10.49; all rows with a later fix
    # include 10.47 in the reviewed affected interval.
    return tuple(map(int, version.split('.'))) < tuple(map(int, row[2].split('.')))


def project(api):
    rows = {}
    for item in json.loads(api):
        key = item['ghsa_id']
        ranges = item['vulnerabilities']
        if (key in rows or item['withdrawn_at'] is not None or len(ranges) != 1
                or ranges[0]['package']['name'] != 'PCRE2'):
            raise ValueError('unknown or revised PCRE2 publisher record')
        rows[key] = [item['cve_id'], ranges[0]['vulnerable_version_range'], ranges[0]['patched_versions']]
    return rows


def validate(report, api, policy, now):
    if (len(api) > 2 << 20 or len(policy) > 1 << 16
            or hashlib.sha256(api).hexdigest() != report['apiSHA256']
            or hashlib.sha256(policy).hexdigest() != POLICY_HASH or project(api) != report['rows']):
        raise ValueError('PCRE2 raw source and projected evidence differ')
    if report['api'] != API or report['policy'] != POLICY or report['policySHA256'] != POLICY_HASH:
        raise ValueError('PCRE2 publisher source differs')
    for key in ('apiDate', 'policyDate'):
        fresh(report[key], now)
    rows = report['rows']
    if len(rows) != len(RANGES) or set(rows) != set(RANGES):
        raise ValueError('PCRE2 publisher advisory set changed')
    for key, value in rows.items():
        if tuple(value) != RANGES[key]:
            raise ValueError('PCRE2 publisher advisory range changed')
    vulnerable = [k for k, row in RANGES.items() if affected(row, '10.47')]
    if len(vulnerable) != 7 or any(affected(row, '10.49') for row in RANGES.values()):
        raise ValueError('PCRE2 actual and old-version controls differ')
    return sorted(vulnerable)


def fetch(url, limit):
    with urlopen(Request(url, headers={'User-Agent': 'kanz-calibration-qualification'}), timeout=45) as response:
        if response.status != 200 or response.geturl() != url or response.headers.get('Link'):
            raise ValueError('unexpected PCRE2 publisher source or pagination')
        raw = response.read(limit + 1)
        if not 0 < len(raw) <= limit:
            raise ValueError('unbounded PCRE2 publisher response')
        return raw, parsedate_to_datetime(response.headers['Date']).isoformat()


def collect(output):
    api, api_date = fetch(API, 2 << 20)
    policy, policy_date = fetch(POLICY, 1 << 16)
    rows = project(api)
    report = {'api': API, 'apiDate': api_date, 'apiSHA256': hashlib.sha256(api).hexdigest(),
              'policy': POLICY, 'policyDate': policy_date, 'policySHA256': hashlib.sha256(policy).hexdigest(),
              'rows': rows}
    controls = validate(report, api, policy, dt.datetime.now(dt.timezone.utc))
    output.with_suffix('.api.json').write_bytes(api)
    output.with_suffix('.policy.md').write_bytes(policy)
    output.write_text(json.dumps(report, sort_keys=True, indent=2) + '\n')
    print('Reviewed PCRE2 publisher advisories:', len(rows), 'old-version controls:', len(controls))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output', type=Path)
    collect(parser.parse_args().output)

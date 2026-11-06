"""Official complete version-specific curl advisory records; no reachability waiver."""
import datetime as dt
import hashlib
import json
import re

import curl_runtime
from openssl_advisory import pairs, version

URL = 'https://curl.se/docs/vuln-'
CONTROL = '8.22.0'
SCOPE = 'libcurl-native-component-only-other-native-ownership-unqualified'


def range_affected(record, target):
    ranges = record['affected'][0].get('ranges')
    if not isinstance(ranges, list) or not 1 <= len(ranges) <= 2:
        raise ValueError('unknown curl advisory range inventory')
    semantic = [r for r in ranges if isinstance(r, dict) and r.get('type') == 'SEMVER']
    if len(semantic) != 1 or set(semantic[0]) != {'type', 'events'}:
        raise ValueError('missing curl semantic affected ranges')
    for item in ranges:
        if item is semantic[0]:
            continue
        if (not isinstance(item, dict) or set(item) != {'type', 'repo', 'events'}
                or item['type'] != 'GIT' or item['repo'] != 'https://github.com/curl/curl.git'
                or not isinstance(item['events'], list) or len(item['events']) != 2):
            raise ValueError('unknown curl additional affected range')
        for event, key in zip(item['events'], ('introduced', 'fixed')):
            if not isinstance(event, dict) or set(event) != {key} or not re.fullmatch('[0-9a-f]{40}', event[key]):
                raise ValueError('unknown curl commit range')
    events = semantic[0]['events']
    if not isinstance(events, list) or not 0 < len(events) <= 128 or len(events) % 2:
        raise ValueError('unclosed curl version interval')
    intervals = []
    for start, end in zip(events[::2], events[1::2]):
        if not isinstance(start, dict) or set(start) != {'introduced'} or not isinstance(end, dict) or set(end) != {'fixed'}:
            raise ValueError('unknown curl semantic event')
        low = (0, 0, 0) if start['introduced'] == '0' else version(start['introduced'])
        high = version(end['fixed'])
        if low >= high or (intervals and low < intervals[-1][1]):
            raise ValueError('inverted or overlapping curl affected intervals')
        intervals.append((low, high))
    return any(low <= version(target) < high for low,high in intervals)


def records(raw, target):
    if not isinstance(raw, str) or not 0 < len(raw.encode()) <= 1 << 20:
        raise ValueError('missing or oversized curl advisory response')
    data = json.loads(raw, object_pairs_hook=pairs)
    if not isinstance(data, list) or len(data) > 512:
        raise ValueError('curl advisory response is not a bounded record array')
    found = {}
    for record in data:
        if not isinstance(record, dict):
            raise ValueError('invalid curl advisory record')
        specific = record.get('database_specific', {})
        aliases = record.get('aliases')
        ident = record.get('id', '')
        if (record.get('schema_version') != '1.5.0' or specific.get('package') != 'curl'
                or not isinstance(aliases, list) or len(aliases) != 1
                or not isinstance(aliases[0], str)
                or not re.fullmatch(r'CVE-[0-9]{4}-[0-9]+', aliases[0])
                or ident != 'CURL-' + aliases[0] or ident in found
                or specific.get('severity') not in ('Low', 'Medium', 'High', 'Critical')
                or specific.get('URL') != 'https://curl.se/docs/' + aliases[0] + '.json'):
            raise ValueError('unrecognized curl advisory identity')
        affected = record.get('affected')
        if (not isinstance(affected, list) or len(affected) != 1
                or not isinstance(affected[0], dict)
                or not isinstance(affected[0].get('versions'), list)
                or target not in affected[0]['versions']):
            raise ValueError('curl version-specific response has unbound affected version')
        if not range_affected(record, target):
            raise ValueError('curl version list disagrees with semantic range')
        found[ident] = record
    return found


def validate_report(report, inventory, scan_time):
    curl_runtime.validate_component(inventory)
    if not isinstance(report, dict) or set(report) != {
            'scope', 'checkedAt', 'version', 'controlVersion', 'rawByVersion', 'sha256ByVersion',
            'affectedControl', 'advisories'}:
        raise ValueError('missing curl native advisory evidence')
    checked = dt.datetime.fromisoformat(report['checkedAt'])
    if checked.tzinfo is None or abs(checked - scan_time) > dt.timedelta(hours=1):
        raise ValueError('curl advisory response outside qualification window')
    raw = report['rawByVersion']
    if not isinstance(raw, dict) or set(raw) != {CONTROL, curl_runtime.VERSION}:
        raise ValueError('missing exact curl advisory queries')
    control = records(raw[CONTROL], CONTROL)
    current = records(raw[curl_runtime.VERSION], curl_runtime.VERSION)
    high = control.get('CURL-CVE-2026-92392', {})
    if (high.get('database_specific', {}).get('severity') != 'High'
            or curl_runtime.VERSION in high.get('affected', [{}])[0].get('versions', [])
            or range_affected(high, curl_runtime.VERSION)
            or len(control) < 21 or current
            or report['scope'] != SCOPE or report['version'] != curl_runtime.VERSION
            or report['controlVersion'] != CONTROL or report['affectedControl'] != sorted(control)
            or report['advisories'] != sorted(current)
            or report['sha256ByVersion'] != {v: hashlib.sha256(text.encode()).hexdigest() for v,text in raw.items()}):
        raise ValueError('curl native advisory/control refusal')


def qualify(run, inventory, now, report):
    raw = {}
    for value in (CONTROL, curl_runtime.VERSION):
        raw[value] = run(['curl', '--disable', '--fail', '--silent', '--show-error', '--proto', '=https',
                          '--max-time', '45', URL + value + '.json'], limit=1 << 20).decode('utf-8')
    report.update(scope=SCOPE, checkedAt=now().isoformat(), version=curl_runtime.VERSION,
                  controlVersion=CONTROL, rawByVersion=raw,
                  sha256ByVersion={v: hashlib.sha256(text.encode()).hexdigest() for v,text in raw.items()})
    report['affectedControl'] = sorted(records(raw[CONTROL], CONTROL))
    report['advisories'] = sorted(records(raw[curl_runtime.VERSION], curl_runtime.VERSION))
    validate_report(report, inventory, now())

"""Reconcile two linked native components against their published fix history and the final image."""
import datetime as dt
import hashlib
import json
from pathlib import Path

from linked_ancestry import (COMPONENTS, UTF8_HEAD, UV_HEAD, UV_MERGE, UV_RELEASE_FIXES, UV_UNIX, UV_WINDOWS,
                             collect as collect_graph, verify as verify_graph)
from pcre2_advisory import fetch, fresh


JULIA = 'https://raw.githubusercontent.com/JuliaLang/julia/67ac47b9cd855e02134d41d5428ac9f2a73f2694/'
PINS = {'src/Makefile': '28ed6010a70407d3548814dbcf0227e4c8f5f773407641f10830fca4499d6cf8',
        'deps/utf8proc.mk': '472201cb5b5628b4362b227741db1ba61460c6ecf6eb19bceea7827b9473670b',
        'deps/utf8proc.version': '4b23dd8ab055d5846197d8b98baf5c6fb8f68aefcb5a3d491790e4e02ab72ddd',
        'deps/libuv.version': 'b39d32ae7d197356a5a2d1c0faa6f75e6b2defa281593787e1f92f8e1d5fc458'}
API = 'https://api.github.com/repos/'
NVD = 'https://services.nvd.nist.gov/rest/json/cves/2.0?'
CVE = 'https://cveawg.mitre.org/api/cve/'
URLS = {**{p.replace('/', '-'): JULIA + p for p in PINS},
        'utf8-release': API + 'JuliaStrings/utf8proc/releases/tags/v2.11.3',
        'utf8-fix': API + 'JuliaStrings/utf8proc/pulls/323',
        'uv-release': API + 'libuv/libuv/releases/tags/v1.48.0',
        'uv-windows-fix': API + 'libuv/libuv/pulls/516',
        'utf8-project': API + 'JuliaStrings/utf8proc/security-advisories?per_page=100',
        'uv-project': API + 'libuv/libuv/security-advisories?per_page=100',
        'uv-fork': API + 'JuliaLang/libuv/security-advisories?per_page=100',
        'utf8-nvd': NVD + 'keywordSearch=utf8proc',
        'uv-nvd': NVD + 'virtualMatchString=cpe:2.3:a:libuv:libuv',
        'uv-nvd-alias': NVD + 'virtualMatchString=cpe:2.3:a:libuv_project:libuv',
        **{i: CVE + i for i in ('CVE-2014-9748', 'CVE-2015-0278', 'CVE-2024-24806')}}
NVD_RECORDS = {'utf8-nvd': set(), 'uv-nvd': {'CVE-2014-9748', 'CVE-2024-24806'}, 'uv-nvd-alias': {'CVE-2015-0278'}}
# Canonical NVD configurations retain the full AND/OR structure, CPE aliases,
# platform conditions and version boundaries of every current matching record.
NVD_CONFIGS = {
    'CVE-2014-9748': '8e9a2d875d7c0891f92007086569622f0267a73cc2161ea9e5f685c99766b6b2',
    'CVE-2015-0278': '341cd26284018b65befabb1e4be7247beedca896af2431a1fb622587ab8037a6',
    'CVE-2024-24806': 'a35b9ddb1fed8552b3ac2db2584259c0672cc0bac794db1ae187f1e5542f38bb',
}
CNA_PROVIDERS = {'CVE-2014-9748': 'mitre', 'CVE-2015-0278': 'redhat', 'CVE-2024-24806': 'GitHub_M'}
CORE = 'lib/julia/libjulia-internal.so.1.13.3'
UV = 'lib/julia/libuv.so.2.0.0'
FILES = {CORE: ('661ae03e0685b66d5f4155bc040cf59986d18bf73dc9175bee92bc311d6c3b03', 15788601),
         UV: ('d9923e2bf93475010c5c944c19999347c493f606308f75901fa4668be29cdeb4', 950584)}
SYMBOLS = {CORE: {'utf8proc_version': '2.11.3', 'utf8proc_unicode_version': '17.0.0', 'uv_version_string': '2.0.0-dev'},
           UV: {'uv_version_string': '2.0.0-dev'}}


def validate_nvd(number, configs):
    encoded = json.dumps(configs, sort_keys=True, separators=(',', ':')).encode()
    if hashlib.sha256(encoded).hexdigest() != NVD_CONFIGS[number]:
        raise ValueError('native NVD applicability changed')


def validate_libuv_recipe(recipe):
    selection = json.loads(Path(__file__).with_name('owner_inputs.json').read_bytes())['LibUV_jll']['recipe']
    marker = 'GitSource("https://github.com/JuliaLang/libuv.git", "' + UV_HEAD + '")'
    if (hashlib.sha256(recipe).hexdigest() != selection['sha256']
            or marker not in ' '.join(recipe.decode('utf-8').split())):
        raise ValueError('standalone libuv producer source differs')


def validate_sources(directory, now):
    validate_libuv_recipe((directory.parent / 'LibUV.recipe').read_bytes())
    dates = json.loads((directory / 'dates.json').read_bytes())
    if set(dates) != set(URLS):
        raise ValueError('linked native publisher sources missing')
    raw = {}
    for key, url in URLS.items():
        meta = dates[key]
        if meta['url'] != url:
            raise ValueError('linked native publisher identity differs')
        fresh(meta['date'], now)
        path = directory / key
        if path.stat().st_size > 2 << 20:
            raise ValueError('unbounded linked native publisher response')
        raw[key] = path.read_bytes()
        if hashlib.sha256(raw[key]).hexdigest() != meta['sha256']:
            raise ValueError('linked native publisher response differs')
    for name, sha in PINS.items():
        if hashlib.sha256(raw[name.replace('/', '-')]).hexdigest() != sha:
            raise ValueError('Julia static link or source pin differs')
    if (UV_HEAD.encode() not in raw['deps-libuv.version'] or UTF8_HEAD.encode() not in raw['deps-utf8proc.version']
            or b'$(call whole_archive,$(LIBUV)) $(call whole_archive,$(LIBUTF8PROC))' not in raw['src-Makefile']
            or b'libutf8proc.a' not in raw['deps-utf8proc.mk']):
        raise ValueError('Julia linked native provenance differs')

    docs = {name: json.loads(raw[name]) for name in URLS if not name.startswith(('src-', 'deps-'))}
    for name, expected in NVD_RECORDS.items():
        data = docs[name]
        date = dt.datetime.fromisoformat(data['timestamp'].replace('Z', '+00:00'))
        if date.tzinfo is None:
            date = date.replace(tzinfo=dt.timezone.utc)
        fresh(date.isoformat(), now)
        if (data['format'] != 'NVD_CVE' or data['version'] != '2.0' or data['startIndex'] != 0
                or data['totalResults'] != len(expected) or len(data['vulnerabilities']) != len(expected)
                or {v['cve']['id'] for v in data['vulnerabilities']} != expected):
            raise ValueError('native NVD denominator changed')
        for row in data['vulnerabilities']:
            validate_nvd(row['cve']['id'], row['cve']['configurations'])
    if docs['utf8-project'] or docs['uv-fork'] or len(docs['uv-project']) != 1:
        raise ValueError('native publisher advisory denominator changed')
    advisory = docs['uv-project'][0]
    v, = advisory['vulnerabilities']
    if (advisory['ghsa_id'] != 'GHSA-f74f-cvh7-c6q6' or advisory['cve_id'] != 'CVE-2024-24806'
            or advisory['withdrawn_at'] is not None or v['vulnerable_version_range'] != '>= v1.24.0'
            or v['patched_versions'] != 'v1.48.0'):
        raise ValueError('libuv publisher range differs')

    uv = docs['uv-release']
    if uv['tag_name'] != 'v1.48.0' or any(fix not in uv['body'] for fix in UV_RELEASE_FIXES):
        raise ValueError('libuv release no longer identifies the known fixes')
    windows = docs['uv-windows-fix']
    if not windows['merged'] or windows['head']['sha'] != UV_WINDOWS:
        raise ValueError('published Windows fix differs')
    for number, links in [('CVE-2024-24806', ['https://github.com/libuv/libuv/commit/' + x for x in (*UV_RELEASE_FIXES, UV_MERGE)]),
                          ('CVE-2015-0278', ['https://github.com/libuv/libuv/commit/' + UV_UNIX]),
                          ('CVE-2014-9748', ['https://github.com/libuv/libuv/pull/516'])]:
        cve = docs[number]
        cna = cve['containers']['cna']
        refs = {ref['url'] for ref in cna['references']}
        if (cve['cveMetadata']['state'] != 'PUBLISHED' or cve['cveMetadata']['cveId'] != number
                or cna['providerMetadata']['shortName'] != CNA_PROVIDERS[number] or not set(links) <= refs):
            raise ValueError('native CNA fix identity differs')
        if number == 'CVE-2024-24806' and cna['affected'] != [
                {'vendor': 'libuv', 'product': 'libuv', 'versions': [{'version': '>= 1.45.0, < 1.48.0', 'status': 'affected'}]}]:
            raise ValueError('libuv CNA affected range differs')
        if number != 'CVE-2024-24806' and cna['affected'] != [
                {'vendor': 'n/a', 'product': 'n/a', 'versions': [{'version': 'n/a', 'status': 'affected'}]}]:
            raise ValueError('historical CNA affected information changed')
    fix, = COMPONENTS['utf8proc'][2]
    if (docs['utf8-release']['tag_name'] != 'v2.11.3' or '#323' not in docs['utf8-release']['body']
            or not docs['utf8-fix']['merged'] or docs['utf8-fix']['merge_commit_sha'] != fix):
        raise ValueError('utf8proc release fix differs')
    for key, (_, head, fixes) in COMPONENTS.items():
        verify_graph(directory / (key + '-commits.json'), head, fixes)


def validate_image(receipt, runtime):
    if (runtime['image'] != receipt['image'] or runtime['source'] != receipt['source']
            or runtime['symbols'] != SYMBOLS):
        raise ValueError('runtime symbol proof does not match the selected application')
    for path, (sha, size) in FILES.items():
        actual = receipt['native'][path]
        if (actual['sha256'], actual['size']) != (sha, size):
            raise ValueError('actual linked native ELF differs: ' + path)


def collect(directory):
    directory.mkdir()
    dates = {}
    for key, url in URLS.items():
        raw, date = fetch(url, 2 << 20)
        (directory / key).write_bytes(raw)
        dates[key] = {'url': url, 'date': date, 'sha256': hashlib.sha256(raw).hexdigest()}
    (directory / 'dates.json').write_text(json.dumps(dates, sort_keys=True) + '\n')
    for key, (repo, head, fixes) in COMPONENTS.items():
        collect_graph(directory / (key + '-commits.json'), repo, head, fixes)
    validate_sources(directory, dt.datetime.now(dt.timezone.utc))
    print('Verified two native owners and seven actual fix commits against their pre-fix histories.')

import copy
import datetime as dt
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import openssl_advisory as advisory
import openssl_runtime as runtime


def component():
    return {'version': runtime.VERSION, 'versionAPI': 'OpenSSL_version(0)',
            'archiveSHA256': runtime.ARCHIVE_SHA256, 'files': copy.deepcopy(runtime.FILES),
            'licenseSHA256': runtime.LICENSE_SHA256, 'originalJLLVersion': '3.5.8+0'}


def fixture(now):
    records = {}
    for number in [84782, *range(1000, 1296)]:
        ident = 'CVE-2026-' + str(number)
        records[ident.lower() + '.json'] = json.dumps({
            'dataType': 'CVE_RECORD', 'dataVersion': '5.1',
            'cveMetadata': {'cveId': ident, 'state': 'PUBLISHED'},
            'containers': {'cna': {'providerMetadata': {'shortName': 'openssl'}, 'affected': [{
                'vendor': 'OpenSSL', 'product': 'OpenSSL', 'defaultStatus': 'unaffected',
                'versions': [{'version': '3.5.0', 'lessThan': '3.5.9',
                              'status': 'affected', 'versionType': 'semver'}]}]}}})
    index = ''.join('<a href="' + name + '">entry</a>' for name in records)
    result = {'index': index, 'records': records, 'indexSHA256': hashlib.sha256(index.encode()).hexdigest(),
              'recordSHA256': {k: hashlib.sha256(v.encode()).hexdigest() for k,v in records.items()},
              'checkedAt': now.isoformat(), 'scope': advisory.SCOPE, 'version': '3.5.9',
              'affectedControlVersion': '3.5.8', 'affectedControl': advisory.catalog(index, records, '3.5.8'),
              'advisories': []}
    return result


class OpenSSLTests(unittest.TestCase):
    def test_archive_members_refuse_tamper_missing_and_links(self):
        # Patching only the outer archive pin exposes the inner member checks;
        # a matching archive hash cannot substitute missing or different bytes.
        for kind in ('empty', 'wrong', 'link', 'duplicate'):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / 'archive.tar.gz'
                with tarfile.open(path, 'w:gz') as archive:
                    if kind != 'empty':
                        member = tarfile.TarInfo('lib/libcrypto.so.3')
                        if kind == 'link':
                            member.type = tarfile.SYMTYPE; member.linkname = '/etc/passwd'
                            archive.addfile(member)
                        else:
                            member.size = 1
                            archive.addfile(member, io.BytesIO(b'x'))
                            if kind == 'duplicate': archive.addfile(member, io.BytesIO(b'x'))
                with patch.object(runtime, 'ARCHIVE_SHA256', hashlib.sha256(path.read_bytes()).hexdigest()):
                    with self.assertRaises(ValueError): runtime.verified_members(path)

    def inventory(self):
        return {'nativeOpenSSL': component(), 'nativeRuntime': {
            'files': copy.deepcopy(runtime.FILES), 'jll': {'OpenSSL_jll': {'version': '3.5.8+0'}}}}

    def test_actual_component_and_original_metadata_required(self):
        good = self.inventory()
        runtime.validate_component(good)
        for key in good['nativeOpenSSL']:
            bad = copy.deepcopy(good); del bad['nativeOpenSSL'][key]
            with self.assertRaises(ValueError): runtime.validate_component(bad)
        for path in runtime.FILES:
            bad = copy.deepcopy(good); bad['nativeRuntime']['files'][path]['sha256'] = 'a' * 64
            with self.assertRaises(ValueError): runtime.validate_component(bad)
        bad = copy.deepcopy(good); bad['nativeRuntime']['jll']['OpenSSL_jll']['version'] = '3.5.9+0'
        with self.assertRaises(ValueError): runtime.validate_component(bad)

    def test_only_reviewed_base_members_can_be_replaced(self):
        base = {'files': copy.deepcopy(runtime.BASE_FILES), 'links': {},
                'jll': {'OpenSSL_jll': {'version': '3.5.8+0'}}}
        with patch.object(runtime, 'verified_members') as verified:
            result = runtime.overlay(base, 'fixture')
            verified.assert_called_once_with('fixture')
            self.assertEqual(result['files'], runtime.FILES)
            self.assertEqual(base['files'], runtime.BASE_FILES)
            for path in runtime.FILES:
                bad = copy.deepcopy(base); bad['links'][path] = 'other'
                with self.assertRaises(ValueError): runtime.overlay(bad, 'fixture')
                bad = copy.deepcopy(base); bad['files'][path]['sha256'] = 'a'*64
                with self.assertRaises(ValueError): runtime.overlay(bad, 'fixture')

    def test_range_controls_and_unknown_shapes(self):
        item = {'version': '3.5.0', 'lessThan': '3.5.9', 'status': 'affected', 'versionType': 'semver'}
        self.assertTrue(advisory.affected(item, '3.5.8'))
        self.assertFalse(advisory.affected(item, '3.5.9'))
        inclusive = dict(item); inclusive['lessThanOrEqual'] = inclusive.pop('lessThan')
        self.assertTrue(advisory.affected(inclusive, '3.5.9'))
        for bad in [dict(item, versionType='unknown'), dict(item, status='unknown'),
                    dict(item, lessThan='*'), dict(item, version='4.0.0'), dict(item, changes=[]),
                    {'version':'3.5.0','lessThan':'3.5.9','status':'affected','versionType':'custom'}]:
            with self.assertRaises(ValueError): advisory.affected(bad, '3.5.9')

    def test_full_catalog_bound_and_no_affected_current_version(self):
        now = dt.datetime(2026,11,6,tzinfo=dt.timezone.utc)
        good = fixture(now)
        advisory.validate_report(good, self.inventory(), now)
        for key in good:
            bad = copy.deepcopy(good); del bad[key]
            with self.assertRaises(ValueError): advisory.validate_report(bad, self.inventory(), now)
        mutations = []
        bad = copy.deepcopy(good); bad['records'].pop(next(iter(bad['records']))); mutations.append(bad)
        bad = copy.deepcopy(good); bad['affectedControl'] = []; mutations.append(bad)
        bad = copy.deepcopy(good); bad['checkedAt'] = (now-dt.timedelta(hours=2)).isoformat(); mutations.append(bad)
        bad = copy.deepcopy(good); bad['scope'] = 'all-native-clear'; mutations.append(bad)
        for change in ('3.5.10', '*'):
            bad = copy.deepcopy(good)
            name = next(iter(bad['records']))
            bad['records'][name] = bad['records'][name].replace('3.5.9', change)
            bad['recordSHA256'][name] = hashlib.sha256(bad['records'][name].encode()).hexdigest()
            mutations.append(bad)
        for bad in mutations:
            with self.assertRaises(ValueError): advisory.validate_report(bad, self.inventory(), now)

    def test_fetch_has_only_eight_outstanding_results_and_closed_index(self):
        now = dt.datetime(2026,11,6,tzinfo=dt.timezone.utc)
        good = fixture(now)
        batches = []
        class Pool:
            def __init__(self, max_workers): self.maximum = max_workers
            def __enter__(self): return self
            def __exit__(self, *args): pass
            def map(self, function, names):
                batches.append(len(names))
                self_outer.assertLessEqual(len(names), self.maximum)
                return map(function, names)
        self_outer = self
        def run(args, limit):
            name = args[-1].removeprefix(advisory.URL)
            raw = good['records'][name] if name else good['index']
            self.assertLessEqual(len(raw.encode()), limit)
            return raw.encode()
        with patch.object(advisory, 'ThreadPoolExecutor', Pool):
            result = advisory.qualify(run, self.inventory(), lambda: now)
        self.assertEqual(result['recordSHA256'], good['recordSHA256'])
        self.assertEqual(sum(batches), 297)
        for link in ('CVE-2026-99.json', 'cve-2026-1.json?changed=1', 'other.json'):
            with self.assertRaises(ValueError): advisory.index_names(good['index'] + '<a href="' + link + '">new</a>')
        for extra in ("<a href='cve-2026-99999.json'>new</a>",
                      '<A HREF=cve-2026-99999.json>new</A>',
                      '<a HREF="cve-2026-99999.json">new</a>'):
            self.assertIn('cve-2026-99999.json', advisory.index_names(good['index'] + extra))
            with self.assertRaises(ValueError): advisory.catalog(good['index'] + extra, good['records'], '3.5.9')
        with self.assertRaises(ValueError): advisory.index_names(good['index'] + "<a href='other.json'>new</a>")
        with patch.object(advisory.time, 'monotonic', side_effect=[0, 121]):
            with self.assertRaises(ValueError): advisory.qualify(run, self.inventory(), lambda: now)
        # Future affected findings remain available in the failed artifact.
        for name in good['records']:
            good['records'][name] = good['records'][name].replace('3.5.9', '3.5.10')
        failed = {}
        with patch.object(advisory, 'ThreadPoolExecutor', Pool):
            with self.assertRaises(ValueError): advisory.qualify(run, self.inventory(), lambda: now, failed)
        self.assertIn('CVE-2026-84782', failed['advisories'])
        self.assertEqual(len(failed['records']), 297)


if __name__ == '__main__':
    unittest.main()

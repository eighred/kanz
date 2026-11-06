import copy
import datetime as dt
import hashlib
import json
import unittest
from unittest.mock import patch

import curl_advisory as advisory
import curl_runtime as runtime


def component():
    return {'version': '8.23.0', 'versionAPI': 'curl_version_info', 'versionNumber': 0x081700,
            'sslBackend': 'OpenSSL/3.5.9', 'features': (1 << 2) | (1 << 16), 'protocols': ['http', 'https'],
            'path': runtime.PATH, 'file': runtime.FILE.copy(), 'archiveSHA256': runtime.ARCHIVE_SHA256,
            'licenseSHA256': runtime.LICENSE_SHA256, 'originalJLLVersion': '8.22.0+0'}


def fixture(now):
    records = []
    for number in [92392, *range(1000, 1020)]:
        ident = 'CVE-2026-' + str(number)
        records.append({'schema_version': '1.5.0', 'id': 'CURL-' + ident, 'aliases': [ident],
                        'database_specific': {'package': 'curl', 'severity': 'High', 'URL': 'https://curl.se/docs/' + ident + '.json'},
                        'affected': [{'versions': ['8.22.0'], 'ranges': [{'type': 'SEMVER', 'events': [
                            {'introduced': '8.12.0'}, {'fixed': '8.23.0'}]}]}]})
    raw = {'8.22.0': json.dumps(records), '8.23.0': '[]'}
    return {'scope': advisory.SCOPE, 'checkedAt': now.isoformat(), 'version': '8.23.0', 'controlVersion': '8.22.0',
            'rawByVersion': raw, 'sha256ByVersion': {k: hashlib.sha256(v.encode()).hexdigest() for k,v in raw.items()},
            'affectedControl': sorted(r['id'] for r in records), 'advisories': []}


class CurlTests(unittest.TestCase):
    def inventory(self):
        return {'nativeLibCURL': component(), 'nativeRuntime': {
            'files': {runtime.PATH: runtime.FILE.copy()}, 'jll': {'LibCURL_jll': {'version': '8.22.0+0'}}}}

    def test_component_cannot_substitute_metadata_for_native_identity(self):
        good = self.inventory()
        runtime.validate_component(good)
        for key in good['nativeLibCURL']:
            bad = copy.deepcopy(good); del bad['nativeLibCURL'][key]
            with self.assertRaises(ValueError): runtime.validate_component(bad)
        for key, value in [('sslBackend', 'GnuTLS'), ('features', 0), ('features', True),
                           ('protocols', ['https']), ('versionNumber', 0x081600),
                           ('file', runtime.BASE), ('originalJLLVersion', '8.23.0+0')]:
            bad = copy.deepcopy(good); bad['nativeLibCURL'][key] = value
            with self.assertRaises(ValueError): runtime.validate_component(bad)

    def test_overlay_validates_artifact_and_exact_original_member(self):
        base = {'files': {runtime.PATH: runtime.BASE.copy()}, 'links': {}, 'jll': {'LibCURL_jll': {'version': '8.22.0+0'}}}
        with patch.object(runtime.openssl_runtime, 'verified_artifact') as verify:
            got = runtime.overlay(base, 'fixture')
            self.assertEqual(got['files'][runtime.PATH], runtime.FILE)
            verify.assert_called_once()
            self.assertEqual(base['files'][runtime.PATH], runtime.BASE)
            base['files'][runtime.PATH] = runtime.FILE
            with self.assertRaises(ValueError): runtime.overlay(base, 'fixture')

    def test_complete_current_response_and_high_control_are_required(self):
        now = dt.datetime(2026,11,6,tzinfo=dt.timezone.utc)
        good = fixture(now)
        advisory.validate_report(good, self.inventory(), now)
        for key in good:
            bad = copy.deepcopy(good); del bad[key]
            with self.assertRaises(ValueError): advisory.validate_report(bad, self.inventory(), now)
        for raw in ('', 'null', '{}', '<html>404</html>', '[{}]'):
            bad = copy.deepcopy(good); bad['rawByVersion']['8.23.0'] = raw
            with self.assertRaises((ValueError, KeyError)): advisory.validate_report(bad, self.inventory(), now)
        bad = copy.deepcopy(good); bad['rawByVersion']['8.22.0'] = '[]'
        with self.assertRaises(ValueError): advisory.validate_report(bad, self.inventory(), now)
        bad = copy.deepcopy(good); bad['checkedAt'] = (now-dt.timedelta(hours=2)).isoformat()
        with self.assertRaises(ValueError): advisory.validate_report(bad, self.inventory(), now)
        # Resealed current-version finding must still refuse; no hash-only gate.
        changed = json.loads(good['rawByVersion']['8.22.0'])[0]
        changed['affected'][0]['versions'] = ['8.23.0']
        changed['affected'][0]['ranges'][0]['events'][1]['fixed'] = '8.24.0'
        bad = copy.deepcopy(good); bad['rawByVersion']['8.23.0'] = json.dumps([changed])
        bad['sha256ByVersion']['8.23.0'] = hashlib.sha256(bad['rawByVersion']['8.23.0'].encode()).hexdigest()
        bad['advisories'] = [changed['id']]
        with self.assertRaises(ValueError): advisory.validate_report(bad, self.inventory(), now)

    def test_range_and_version_list_must_agree(self):
        now = dt.datetime(2026,11,6,tzinfo=dt.timezone.utc)
        record = json.loads(fixture(now)['rawByVersion']['8.22.0'])[0]
        self.assertTrue(advisory.range_affected(record, '8.22.0'))
        self.assertFalse(advisory.range_affected(record, '8.23.0'))
        for events in ([{'introduced':'8.12.0'}], [{'introduced':'8.12.0'},{'fixed':'8.12.0'}],
                       [{'introduced':'8.12.0'},{'limit':'8.23.0'}]):
            bad = copy.deepcopy(record); bad['affected'][0]['ranges'][0]['events'] = events
            with self.assertRaises(ValueError): advisory.range_affected(bad, '8.22.0')


if __name__ == '__main__':
    unittest.main()

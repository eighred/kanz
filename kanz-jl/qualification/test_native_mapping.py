"""Closed identity correction controls; synthetic source bytes never qualify an image."""
import copy
import datetime as dt
import hashlib
import json
import unittest
from unittest.mock import patch

import native_mapping as n

NOW = dt.datetime(2026, 11, 6, 20, tzinfo=dt.timezone.utc)
RAW_SOURCES = {'julia': 'P7ZIP_VER := 26.03\n',
               'upstream': '26.03\nFixed CVE-2026-58052\n26.02\n'}
SOURCES = {key: {**n.SOURCES[key], 'sha256': hashlib.sha256(raw.encode()).hexdigest()}
           for key, raw in RAW_SOURCES.items()}


def fixture(now=NOW):
    data = {'format': 'NVD_CVE', 'version': '2.0', 'totalResults': 1,
            'startIndex': 0, 'resultsPerPage': 1, 'timestamp': now.isoformat(),
            'vulnerabilities': [{'cve': {'id': n.CVE, 'vulnStatus': 'Analyzed',
                'lastModified': (now-dt.timedelta(hours=1)).isoformat(),
                'configurations': copy.deepcopy(n.CONFIG)}}]}
    raw = json.dumps(data)
    inventory = {'native7zip': copy.deepcopy(n.COMPONENT),
                 'nativeRuntime': {'files': {n.PATH: copy.deepcopy(n.FILE)}}}
    report = {'component': copy.deepcopy(n.COMPONENT), 'sources': SOURCES,
              'sourceBytes': RAW_SOURCES, 'checkedAt': now.isoformat(),
              'nvd': {'url': n.NVD_URL, 'raw': raw, 'sha256': hashlib.sha256(raw.encode()).hexdigest()},
              'controls': {'26.02': 'affected', '26.03': 'fixed'},
              'status': 'fixed-native-version-for-reviewed-advisory'}
    return report, inventory


class NativeMappingTests(unittest.TestCase):
    def test_complete_mapping_and_all_required_fields(self):
        report, inventory = fixture()
        with patch.object(n, 'SOURCES', SOURCES):
            n.validate_report(report, inventory, NOW)
            for field in report:
                changed = copy.deepcopy(report); changed.pop(field)
                with self.subTest(field=field), self.assertRaises((ValueError, KeyError)):
                    n.validate_report(changed, inventory, NOW)
            for mutate in [lambda x: x['component'].update(version='26.02'),
                           lambda x: x['controls'].update({'26.02': 'fixed'}),
                           lambda x: x['sourceBytes'].update(julia='P7ZIP_VER := 26.02'),
                           lambda x: x['nvd'].update(sha256='0'*64),
                           lambda x: x.update(checkedAt=(NOW+dt.timedelta(hours=2)).isoformat()),
                           lambda x: x.update(status='all-native-cleared')]:
                changed = copy.deepcopy(report); mutate(changed)
                with self.assertRaises(ValueError): n.validate_report(changed, inventory, NOW)

    def test_actual_archive_image_and_executed_version_are_all_required(self):
        _, inventory = fixture()
        archive = {'files': {n.PATH: n.FILE}}
        n.validate_component(inventory, archive)
        for field in n.COMPONENT:
            changed = copy.deepcopy(inventory); changed['native7zip'][field] = 'wrong'
            with self.assertRaises(ValueError): n.validate_component(changed, archive)
        for field in n.FILE:
            changed = copy.deepcopy(inventory); changed['nativeRuntime']['files'][n.PATH][field] = 'wrong'
            with self.assertRaises(ValueError): n.validate_component(changed, archive)
        with self.assertRaises(ValueError): n.validate_component(inventory, {'files': {}})

    def test_exact_analyzed_range_and_version_controls(self):
        report, _ = fixture(); raw = report['nvd']['raw']
        cve = n.nvd_document(raw, NOW)
        self.assertTrue(n.affected('26.02', cve))
        self.assertFalse(n.affected('26.03', cve))
        with self.assertRaises(ValueError): n.affected('26.04', cve)
        mutations = [lambda x: x.update(id='CVE-other'), lambda x: x.update(vulnStatus='Awaiting Analysis'),
            lambda x: x['configurations'][0]['nodes'][0].update(operator='AND'),
            lambda x: x['configurations'][0]['nodes'][0].update(negate=True),
            lambda x: x['configurations'][0]['nodes'][0]['cpeMatch'][0].update(versionEndExcluding='26.04'),
            lambda x: x['configurations'][0]['nodes'][0]['cpeMatch'][0].update(versionEndIncluding='26.03'),
            lambda x: x['configurations'][0]['nodes'][0]['cpeMatch'][0].update(vulnerable=1),
            lambda x: x['configurations'][0]['nodes'][0]['cpeMatch'].append({}),
            lambda x: x['configurations'][0]['nodes'][0]['cpeMatch'][0].update(criteria='cpe:wrong')]
        for mutate in mutations:
            data = json.loads(raw); mutate(data['vulnerabilities'][0]['cve'])
            with self.assertRaises(ValueError): n.nvd_document(json.dumps(data), NOW)
        for delta in (-25, 1):
            data = json.loads(raw); data['timestamp'] = (NOW+dt.timedelta(hours=delta)).isoformat()
            with self.assertRaises(ValueError): n.nvd_document(json.dumps(data), NOW)
        for raw_bad in ['{"format":1,"format":2}', 'x'*(n.LIMIT+1), '{}']:
            with self.assertRaises(ValueError): n.nvd_document(raw_bad, NOW)

    def test_only_exact_raw_finding_can_be_resolved(self):
        finding = {'VulnerabilityID': n.CVE, 'PkgName': 'p7zip_jll', 'PkgID': n.UUID,
                   'InstalledVersion': '17.8.3+0', 'Severity': 'MEDIUM'}
        self.assertTrue(n.resolves(finding))
        for key in finding:
            self.assertFalse(n.resolves({**finding, key: 'unknown'}))
        self.assertFalse(n.resolves({**finding, 'extra': True}))

    def test_raw_wrapper_finding_is_retained_and_unmapped_additions_refuse(self):
        import jll_advisory as j
        mapping, inventory = fixture()
        native = inventory['nativeRuntime']
        native['jll'] = {'Fixture'+str(i)+'_jll': {'name':'Fixture'+str(i)+'_jll',
            'uuid':'00000000-0000-0000-0000-'+str(i).zfill(12), 'version':'1.0.0'} for i in range(22)}
        native['jll']['p7zip_jll'] = {'name':'p7zip_jll', 'uuid':n.UUID, 'version':'17.8.3+0'}
        expected = j.packages(native)
        finding = {'VulnerabilityID':n.CVE, 'PkgName':'p7zip_jll', 'PkgID':n.UUID,
                   'InstalledVersion':'17.8.3+0', 'Severity':'MEDIUM'}
        report = {'registryCommit':j.REGISTRY_COMMIT, 'registrySHA256':j.REGISTRY_SHA256,
            'packages':expected, 'scannerManifestSHA256':hashlib.sha256(j.manifest(expected)).hexdigest(),
            'scannerProjectSHA256':hashlib.sha256(j.project(expected)).hexdigest(),
            'knownJLLAdvisoryControlPassed':True, 'advisories':[finding],
            'scope':'archive-bound-JLL-metadata-only'}
        before = copy.deepcopy(report)
        with patch.object(n, 'SOURCES', SOURCES):
            j.validate_report(report, native, mapping, inventory, NOW)
            self.assertEqual(report, before)
            with self.assertRaises(ValueError): j.validate_report(report, native)
            for field in finding:
                changed = copy.deepcopy(report); changed['advisories'].append({**finding, field:'unknown'})
                with self.subTest(field=field), self.assertRaises(ValueError):
                    j.validate_report(changed, native, mapping, inventory, NOW)
            changed = copy.deepcopy(mapping); changed['component']['version'] = '26.02'
            with self.assertRaises(ValueError): j.validate_report(report, native, changed, inventory, NOW)

    def test_fetch_flow_pins_sources_and_executes_both_range_controls(self):
        report, inventory = fixture(); calls = []
        def run(args, **kwargs):
            calls.append(args)
            self.assertEqual(kwargs['limit'], n.LIMIT)
            if args[-1] == n.NVD_URL: return report['nvd']['raw'].encode()
            return next(RAW_SOURCES[key].encode() for key, value in SOURCES.items() if value['url'] == args[-1])
        with patch.object(n, 'SOURCES', SOURCES):
            result = n.qualify(run, inventory, lambda: NOW)
            self.assertEqual(result, report)
        self.assertEqual(len(calls), 3)
        self.assertTrue(all(args[0] == 'curl' and args[args.index('--proto-redir')+1] == '=https' for args in calls))


if __name__ == '__main__': unittest.main()

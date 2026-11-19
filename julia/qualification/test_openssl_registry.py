import datetime as dt
import hashlib
import json
import unittest

import openssl_advisory as advisory
import openssl_registry as registry
import source_candidate
from test_openssl import component, fixture


class OpenSSLRegistryTests(unittest.TestCase):
    def setUp(self):
        self.now = dt.datetime.now(dt.timezone.utc)
        self.report = fixture(self.now)
        self.registry = self.report['registryCoverage']
        ident = 'CVE-2026-121482'
        name = ident.lower() + '.json'
        self.report['index'] = self.report['index'].replace('<a href="' + name + '">entry</a>',
                                                           '<a href="cve-2026-9000.json">entry</a>')
        text = self.report['records'].pop(name)
        other = text.replace(ident, 'CVE-2026-9000')
        self.report['records']['cve-2026-9000.json'] = other
        self.report['recordSHA256'] = {key: hashlib.sha256(value.encode()).hexdigest()
                                       for key, value in self.report['records'].items()}
        self.report['indexSHA256'] = hashlib.sha256(self.report['index'].encode()).hexdigest()
        self.record = json.loads(text)
        self.record['dataVersion'] = '5.2'
        self.record['cveMetadata'].update(assignerOrgId=registry.CNA, assignerShortName='openssl')
        cna = self.record['containers']['cna']
        cna['providerMetadata']['orgId'] = registry.CNA
        cna['affected'][0]['versions'][0]['lessThan'] = '3.5.10'
        self.ident = ident
        self.write_record()

    def write_record(self):
        text = json.dumps(self.record)
        self.registry['records'][self.ident] = text
        self.registry['recordSHA256'][self.ident] = hashlib.sha256(text.encode()).hexdigest()

    def write_page(self, page):
        raw = json.dumps(page)
        self.registry.update(sourcePage=raw, sourcePageSHA256=hashlib.sha256(raw.encode()).hexdigest())

    def supplement(self):
        return registry.published_records(advisory.index_names(self.report['index']), self.registry,
                                          self.now)

    def test_real_directory_gap_refuses_in_release_and_development_paths(self):
        extra = self.supplement()
        name = self.ident.lower() + '.json'
        self.assertEqual(set(extra), {name})
        self.assertEqual(advisory.catalog(self.report['index'], self.report['records'], '3.5.9'), [])
        self.assertEqual(advisory.complete_matches(self.report['index'], self.report['records'], extra, '3.5.9'),
                         [self.ident])
        self.assertIn(self.ident, advisory.complete_matches(self.report['index'], self.report['records'], extra, '3.5.8'))
        self.assertNotIn(self.ident, advisory.complete_matches(self.report['index'], self.report['records'], extra, '4.1.0'))
        actual = component()
        inventory = {'nativeOpenSSL': actual, 'nativeRuntime': {'files': actual['files'],
                     'jll': {'OpenSSL_jll': {'version': '3.5.8+0'}}}}
        with self.assertRaisesRegex(ValueError, 'advisory/control/identity refusal'):
            advisory.validate_report(self.report, inventory, self.now)
        with self.assertRaisesRegex(ValueError, 'complete release baseline'):
            source_candidate.source_catalog(self.report['index'], self.report['records'], {'vendorFixes': {}},
                                            self.now, self.now, self.registry)

    def test_missing_wrong_publisher_and_conflicting_state_refuse(self):
        self.registry['records'].clear()
        with self.assertRaisesRegex(ValueError, 'incomplete'):
            self.supplement()
        self.record['cveMetadata']['state'] = 'REJECTED'
        self.write_record()
        with self.assertRaisesRegex(ValueError, 'state or identity'):
            self.supplement()
        self.record['cveMetadata']['state'] = 'PUBLISHED'
        self.record['containers']['cna']['providerMetadata']['orgId'] = 'different'
        self.write_record()
        with self.assertRaisesRegex(ValueError, 'state or identity'):
            self.supplement()

    def test_nvd_incomplete_stale_duplicate_and_wrong_source_refuse(self):
        for mutation in (lambda page: page.update(totalResults=page['totalResults'] + 1),
                         lambda page: page.update(timestamp=(self.now - dt.timedelta(hours=2)).isoformat()),
                         lambda page: page['vulnerabilities'].__setitem__(1, page['vulnerabilities'][2]),
                         lambda page: page['vulnerabilities'][0]['cve'].update(sourceIdentifier='unknown')):
            with self.subTest(mutation=mutation):
                page = json.loads(self.report['registryCoverage']['sourcePage'])
                original = self.registry.copy()
                mutation(page)
                self.write_page(page)
                with self.assertRaises(ValueError):
                    self.supplement()
                self.registry.update(original)

    def test_rejected_records_checked_against_publisher_and_excluded(self):
        other = 'CVE-2026-9999'
        page = json.loads(self.registry['sourcePage'])
        page['vulnerabilities'][-1] = {'cve': {'id': other, 'sourceIdentifier': registry.SOURCE, 'vulnStatus': 'Rejected'}}
        self.write_page(page)
        doc = json.loads(self.registry['records'][self.ident])
        doc['cveMetadata'].update(cveId=other, state='REJECTED')
        doc['containers']['cna'].pop('affected')
        text = json.dumps(doc)
        self.registry['records'][other] = text
        self.registry['recordSHA256'][other] = hashlib.sha256(text.encode()).hexdigest()
        self.assertEqual(set(self.supplement()), {self.ident.lower() + '.json'})
        self.registry['recordSHA256'][other] = 'bad'
        with self.assertRaisesRegex(ValueError, 'digest differs'):
            self.supplement()

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
        return registry.published_records(self.report['records'], self.registry, self.now)

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
        del self.registry['records'][self.ident]
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

    def test_loss_of_another_discovery_control_with_unchanged_count_refuses(self):
        page = json.loads(self.registry['sourcePage'])
        other = next(row['cve'] for row in page['vulnerabilities'] if row['cve']['id'] == 'CVE-2026-107494')
        other['id'] = 'CVE-2026-3000'
        self.write_page(page)
        with self.assertRaisesRegex(ValueError, 'discovery control absent'):
            self.supplement()

    def test_control_can_move_into_vendor_if_affected_ranges_agree(self):
        name = self.ident.lower() + '.json'
        self.report['records'][name] = json.dumps(self.record)
        self.assertEqual(self.supplement(), {})
        self.record['containers']['cna']['affected'][0]['versions'][0]['lessThan'] = '3.5.11'
        self.write_record()
        with self.assertRaisesRegex(ValueError, 'affected range disagreement'):
            self.supplement()

    def test_historical_projection_is_bound_to_both_authorities(self):
        page = json.loads(self.registry['sourcePage'])
        ident = 'CVE-2022-2274'
        # This historical CVE Program record describes a single obsolete release in prose.
        row = page['vulnerabilities'][-1]['cve']
        former = row['id']
        row['id'] = ident
        self.write_page(page)
        del self.registry['records'][former]
        del self.registry['recordSHA256'][former]
        self.ident = ident
        self.record['cveMetadata']['cveId'] = ident
        product = self.record['containers']['cna']['affected'][0]
        product['versions'] = [{'version': '3.0.4', 'lessThan': '3.0.5', 'status': 'affected', 'versionType': 'semver'}]
        name = ident.lower() + '.json'
        self.report['records'][name] = json.dumps(self.record)
        del product['defaultStatus']
        product['versions'] = [{'status': 'affected', 'version': 'Affects OpenSSL 3.0.4'}]
        self.write_record()
        self.assertNotIn(name, self.supplement())
        self.report['records'][name] = self.report['records'][name].replace('3.0.5', '3.5.10')
        with self.assertRaisesRegex(ValueError, 'affected range disagreement'):
            self.supplement()
        self.report['records'][name] = self.report['records'][name].replace('3.5.10', '3.0.5')
        product['versions'][0]['version'] += ' and later releases'
        self.write_record()
        with self.assertRaisesRegex(ValueError, 'affected range disagreement'):
            self.supplement()

    def test_rejected_control_requires_consistent_public_sources(self):
        page = json.loads(self.registry['sourcePage'])
        row = next(row['cve'] for row in page['vulnerabilities'] if row['cve']['id'] == self.ident)
        row['vulnStatus'] = 'Rejected'
        self.write_page(page)
        self.record['cveMetadata']['state'] = 'REJECTED'
        self.write_record()
        self.assertEqual(self.supplement(), {})
        self.report['records'][self.ident.lower() + '.json'] = self.registry['records'][self.ident]
        with self.assertRaisesRegex(ValueError, 'states disagree'):
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
        former = page['vulnerabilities'][-1]['cve']['id']
        del self.registry['records'][former]
        del self.registry['recordSHA256'][former]
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

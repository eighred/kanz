import copy
import unittest

import envoy_evidence as proof


class EvidenceRefusals(unittest.TestCase):
    def events(self):
        result = []
        for package, names in proof.REQUIRED.items():
            package = 'github.com/eighred/kanz/' + package
            result.extend({'Action': 'pass', 'Package': package, 'Test': name} for name in names)
            result.append({'Action': 'pass', 'Package': package})
        result.append({'Action': 'output', 'Output': '    envoy_integration_test.go:186: ENVOY_IMAGE_OBSERVED=' + self.image + '\n'})
        return result

    image = 'sha256:' + 'a' * 64

    def test_missing_negative_control_and_cleanup_refuse(self):
        proof.validate_events(self.events(), self.image)
        for name in ('quiet_stream_negative_control', 'cleanup'):
            events = [e for e in self.events() if e.get('Test') != 'TestEnvoyEdgeActualBFF/' + name]
            with self.assertRaises(ValueError):
                proof.validate_events(events, self.image)

    def test_skipped_or_wrong_image_refuses(self):
        events = self.events()
        events.append({'Action': 'skip', 'Package': 'some/package', 'Test': 'runtime'})
        with self.assertRaises(ValueError):
            proof.validate_events(events, self.image)
        with self.assertRaises(ValueError):
            proof.validate_events(self.events(), 'sha256:' + 'b' * 64)

    def test_inventory_critical_and_suppression_refuse(self):
        scan = {'Trivy': {'Version': '0.72.0'}, 'Metadata': {'ImageID': self.image},
                'Results': [{'Class': 'os-pkgs', 'Packages': [{'Name': 'synthetic'}]}]}
        self.assertEqual(proof.validate_scan(scan, self.image), [])
        for mutation in (
            lambda s: s['Metadata'].update(ImageID='sha256:' + 'b' * 64),
            lambda s: s.update(Results=[]),
            lambda s: s['Results'][0].update(ExperimentalModifiedFindings=[{'Status': 'ignored'}]),
            lambda s: s['Results'][0].update(Vulnerabilities=[{'Severity': 'CRITICAL'}]),
        ):
            changed = copy.deepcopy(scan)
            mutation(changed)
            with self.assertRaises(ValueError):
                proof.validate_scan(changed, self.image)


if __name__ == '__main__':
    unittest.main()

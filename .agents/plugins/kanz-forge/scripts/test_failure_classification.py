import unittest
from failure_classification import classify


class ClassificationTests(unittest.TestCase):
    def test_dns_precedes_consequential_missing_packages(self):
        result = classify("Temporary failure resolving 'archive.ubuntu.com'\nE: Unable to locate package fonts-unifont\nInstallation process exited with code: 100", "job/1")
        self.assertEqual(result["cause_id"], "dns:archive.ubuntu.com")
        self.assertEqual(result["provider_root_cause"], "unestablished")

    def test_openssl_requires_actual_artifact_and_positive_control(self):
        self.assertEqual(classify("native OpenSSL advisory/control/identity refusal", "job/1")["kind"], "unknown")
        artifact = dict(passed=False, knownAdvisoryControlPassed=True, failureStage="native-openssl-catalog",
                        opensslAdvisory=dict(version="3.5.9", advisories=["CVE-2026-88823"]))
        self.assertEqual(classify("", "artifact/1", artifact)["kind"], "security")
        artifact["knownAdvisoryControlPassed"] = False
        self.assertEqual(classify("", "artifact/1", artifact)["kind"], "unknown")

    def test_go_generic_failure_never_implies_advisory(self):
        self.assertEqual(classify("bounded image tool refused or exceeded its budget", "job/1")["kind"], "unknown")
        log = "Vulnerability #1: GO-2026-6859\nFound in: net/http@go1.26.10\nFixed in: net/http@go1.26.11"
        self.assertEqual(classify(log, "artifact/2")["cause_id"], "GO-2026-6859")

    def test_aggregate_retains_actual_prerequisites(self):
        result = classify("exit 1", "job/aggregate", prerequisites=[dict(id="race", state="VALIDATION_FAILED")])
        self.assertEqual(result["kind"], "dependent")
        self.assertEqual(result["requires"], ["race"])
        self.assertIsNone(result["cause_id"])


if __name__ == "__main__":
    unittest.main()

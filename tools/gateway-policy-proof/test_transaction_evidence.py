import copy
import unittest

import proof
import rollout_probe
from transaction_fixture import CASES, validate_report


class TransactionEvidenceTests(unittest.TestCase):
    def test_exact_source_attempt_complete_cases_cleanup_and_identity_required(self):
        report = {"format": "kanz-gateway-policy-transaction-proof-v1", "source": "a" * 40, "tree": "b" * 40,
                  "runID": "123", "runAttempt": "2", "renderSHA256": "c" * 64, "nodeImage": proof.feed.NODE,
                  "cluster": "cni-proof-" + "d" * 12, "cases": list(CASES), "passed": True, "cleanupPassed": True,
                  "imageIdentity": {"root": rollout_probe.IMAGE.split("@", 1)[1], "manifest": "sha256:" + "e" * 64,
                                    "config": "sha256:" + "f" * 64, "platform": "linux/amd64", "rootMediaType": "application/vnd.oci.image.manifest.v1+json"}}
        def check(value):
            validate_report(value, "a" * 40, "b" * 40, "123", "2", "c" * 64)
        check(report)
        for key, value in (("source", "e" * 40), ("tree", "e" * 40), ("runID", "124"), ("runAttempt", "1"),
                           ("renderSHA256", "d" * 64), ("cases", CASES[:-1]), ("cases", list(reversed(CASES))),
                           ("passed", 1), ("cleanupPassed", False), ("imageIdentity", {})):
            bad = copy.deepcopy(report); bad[key] = value
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                check(bad)


if __name__ == "__main__":
    unittest.main()

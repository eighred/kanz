import copy
import unittest
import json

import proof
import rollout_probe
from transaction_fixture import CASES, validate_report, deployment_diagnostic, runtime_failure_category


class TransactionEvidenceTests(unittest.TestCase):
    def test_runtime_message_only_exports_closed_category(self):
        private = " /private/token/credential=https://example.invalid/secret "
        for text, category in (("no space left on device", "disk_exhausted"),
                ("executable file not found", "missing_executable"),
                ("content digest sha256:abc not found", "missing_image_content"),
                ("error mounting volume", "mount_setup"), ("seccomp failed", "security_setup"),
                ("read-only file system", "read_only"), ("permission denied", "permission"),
                ("no such file or directory", "missing_path"), ("unrecognized", "unknown")):
            self.assertEqual(runtime_failure_category(private + text), category)
        for value in (None, {}, "no space left on device" + "x" * 4096):
            self.assertEqual(runtime_failure_category(value), "unknown")

    def test_diagnostic_is_closed_and_bounded(self):
        private = "private-message-https://example.invalid/token"
        deployment = {"status": {"replicas": 1, "readyReplicas": False, "conditions": [
            {"type": "Progressing", "status": "False", "reason": "FailedCreate", "message": private},
            {"type": private, "status": private, "reason": private}]}}
        pods = [{"metadata": {"name": private}, "status": {"phase": "Pending", "conditions": [
            {"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": private}], "containerStatuses": [
            {"ready": False, "restartCount": 2, "state": {"waiting": {"reason": "ImagePullBackOff", "message": private}}}]}}]
        result = deployment_diagnostic(deployment, pods)
        self.assertNotIn(private, json.dumps(result))
        self.assertIsNone(result["replicas"]["readyReplicas"])
        self.assertEqual(result["pods"][0]["containers"][0]["waiting"], "ImagePullBackOff")
        self.assertEqual(result["pods"][0]["conditions"][0]["reason"], "Unschedulable")
        for bad in (pods * 17, [{"status": {"containerStatuses": [{}] * 5}}]):
            with self.assertRaises(ValueError): deployment_diagnostic(deployment, bad)

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

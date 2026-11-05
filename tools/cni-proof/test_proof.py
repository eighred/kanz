import copy
import json
from pathlib import Path
import unittest
from unittest.mock import patch

import proof
import ci


class Fixture:
    def __init__(self, failure=None):
        self.failure = failure
        self.namespace = None
        self.policies = set()
        self.calls = []

    def __call__(self, *args, value=None, timeout=25):
        self.calls.append((args, value))
        if args[:2] == ("create", "-f"):
            self.namespace = copy.deepcopy(value)
            self.namespace["metadata"]["uid"] = "owned-uid"
            if self.failure == "ambiguous_create":
                raise proof.ProofError("transport")
            return json.dumps(self.namespace)
        if args[:2] == ("get", "namespace"):
            if self.failure == "cleanup_api":
                raise proof.ProofError("transport")
            if not self.namespace:
                return ""
            current = copy.deepcopy(self.namespace)
            if self.failure == "ownership":
                current["metadata"]["uid"] = "other-uid"
            return json.dumps(current)
        if args[:2] == ("delete", "--raw"):
            assert args[2] == "/api/v1/namespaces/" + self.namespace["metadata"]["name"]
            assert value["preconditions"]["uid"] == self.namespace["metadata"]["uid"]
            self.namespace = None
            return ""
        if args[0] == "wait":
            return ""
        assert args[0] == "-n" and args[1] == self.namespace["metadata"]["name"]
        if args[2] == "create":
            if value["kind"] == "NetworkPolicy":
                if self.failure != "unenforced":
                    self.policies.add(value["metadata"]["name"])
            else:
                self.objects = value
            return ""
        if args[2] == "wait":
            return ""
        if args[2:4] == ("get", "pod"):
            return json.dumps({"metadata": {"uid": args[4] + "-uid"},
                "spec": {"containers": [{"image": proof.IMAGE if self.failure != "wrong_image" else "wrong"}]}, "status": {
                "podIP": "10.42.0.2", "containerStatuses": [{
                    "ready": self.failure != "unhealthy", "restartCount": 0,
                    "imageID": proof.IMAGE if self.failure != "wrong_loaded_image" else "sha256:wrong"}]}})
        if args[2:4] == ("get", "service"):
            return json.dumps({"spec": {"clusterIP": "10.43.0.2"}})
        if args[2:4] == ("delete", "networkpolicy"):
            self.policies.clear()
            return ""
        if args[2] == "exec":
            if self.failure == "exec_error":
                raise proof.ProofError("exec")
            if self.failure == "malformed":
                return "not evidence"
            client = args[3]
            blocked = client != "server" and "deny-ingress" in self.policies
            if "selected-ingress" in self.policies and client == "selected":
                blocked = False
            return json.dumps({"outcome": "blocked" if blocked else "allowed",
                               "reason": "connect_timeout" if blocked else "echo"})
        raise AssertionError(args)


class ProofTests(unittest.TestCase):
    def run_fixture(self, failure=None):
        fixture = Fixture(failure)
        ticks = iter(range(10000))
        with patch.object(proof.time, "monotonic", side_effect=lambda: next(ticks)), \
                patch.object(proof.time, "sleep"):
            result = proof.run_proof(fixture, "a" * 40)
        return fixture, result

    def test_complete_controls_and_restoration(self):
        fixture, result = self.run_fixture()
        self.assertEqual(result["verdict"], "PASS")
        self.assertEqual(result["cleanup"], "verified_absent")
        self.assertEqual(len(result["cases"]), 16)
        self.assertEqual(len({case["case"] for case in result["cases"]}), 16)
        self.assertTrue(all(case["verdict"] == "PASS" for case in result["cases"]))
        self.assertIsNone(fixture.namespace)
        proof.validate_report(result, "a" * 40)
        self.assertTrue(result["cases"][-1]["case"].startswith("restored_"))

    def test_failure_never_becomes_denial_and_cleanup_runs(self):
        for failure in ("exec_error", "malformed", "unhealthy", "unenforced", "ambiguous_create", "wrong_image", "wrong_loaded_image"):
            with self.subTest(failure=failure):
                fixture, result = self.run_fixture(failure)
                self.assertEqual(result["verdict"], "FAIL")
                self.assertEqual(result["cleanup"], "verified_absent")
                self.assertIsNone(fixture.namespace)

    def test_cleanup_failure_and_ownership_change_refuse(self):
        for failure in ("cleanup_api", "ownership"):
            fixture, result = self.run_fixture(failure)
            self.assertEqual(result["verdict"], "FAIL")
            self.assertEqual(result["cleanup"], "unknown")
            self.assertIsNotNone(fixture.namespace)

    def test_unknown_or_generic_process_output_is_not_denial(self):
        for value in ("", "permission denied", "{}", "null", "[]",
                      '{"outcome":"unknown","reason":"connect_error"}',
                      '{"outcome":"blocked","reason":"process_exit"}',
                      '{"outcome":"blocked","reason":"connect_timeout","extra":true}'):
            with self.assertRaises(proof.ProofError):
                proof.decode_probe(value)

    def test_report_requires_unique_exact_cases_reasons_and_loaded_images(self):
        _, valid = self.run_fixture()
        for index in range(16):
            changed = copy.deepcopy(valid)
            changed["cases"][index] = copy.deepcopy(valid["cases"][(index + 1) % 16])
            with self.assertRaises(proof.ProofError):
                proof.validate_report(changed, "a" * 40)
        for mutation in ("source", "image", "loaded", "reason", "cleanup", "extra"):
            changed = copy.deepcopy(valid)
            if mutation == "source": changed["source"] = "b" * 40
            if mutation == "image": changed["image"] = "python:latest"
            if mutation == "loaded": changed["images"]["selected"] = "sha256:" + "0" * 64
            if mutation == "reason": changed["cases"][0]["observation"] = "connect_timeout"
            if mutation == "cleanup": changed["cleanup"] = "unknown"
            if mutation == "extra": changed["extra"] = True
            with self.subTest(mutation=mutation), self.assertRaises(proof.ProofError):
                proof.validate_report(changed, "a" * 40)

    def test_non_enforcing_control_requires_successful_traffic_not_exec_failure(self):
        _, result = self.run_fixture("unenforced")
        ci.check_disabled_control(result, "a" * 40)
        for mutation in ("exec_error", "unhealthy", "malformed"):
            _, changed = self.run_fixture(mutation)
            with self.assertRaises(proof.ProofError):
                ci.check_disabled_control(changed, "a" * 40)
        changed = copy.deepcopy(result)
        changed["cases"][-1]["observation"] = "unknown"
        with self.assertRaises(proof.ProofError):
            ci.check_disabled_control(changed, "a" * 40)

    def test_resources_are_strictly_bounded_and_unprivileged(self):
        namespace = proof.namespace_name()
        pods = [item for item in proof.objects(namespace, "n" * 32)["items"] if item["kind"] == "Pod"]
        self.assertEqual(len(pods), 3)
        for pod in pods:
            spec = pod["spec"]
            self.assertFalse(spec["automountServiceAccountToken"])
            self.assertFalse(spec["enableServiceLinks"])
            self.assertNotIn("volumes", spec)
            self.assertNotIn("hostNetwork", spec)
            self.assertEqual(spec["activeDeadlineSeconds"], 300)
            self.assertTrue(spec["securityContext"]["runAsNonRoot"])
            self.assertEqual(spec["securityContext"]["seccompProfile"]["type"], "RuntimeDefault")
            container = spec["containers"][0]
            self.assertEqual(container["image"], proof.IMAGE)
            self.assertRegex(container["image"], r"@sha256:[0-9a-f]{64}$")
            self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
            self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
            self.assertEqual(container["securityContext"]["capabilities"], {"drop": ["ALL"]})
            self.assertEqual(container["resources"]["limits"], {"cpu": "100m", "memory": "64Mi"})

    def test_no_caller_namespace_or_target(self):
        for namespace in ("kanz-services", "kube-system", "kanz-cni-proof-", "kanz-cni-proof-../x"):
            with self.assertRaises(proof.ProofError):
                proof.objects(namespace, "n" * 32)
        source = Path(proof.__file__).read_text()
        self.assertNotIn('add_argument("--namespace"', source)
        self.assertNotIn('add_argument("--target"', source)


if __name__ == "__main__":
    unittest.main()

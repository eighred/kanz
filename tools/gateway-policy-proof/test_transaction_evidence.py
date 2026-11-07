import copy
import unittest
import json

import inspect
import transaction_fixture as fixture

import proof
import rollout_probe
from transaction_fixture import CASES, validate_report, deployment_diagnostic, runtime_failure_category, synthetic_runtime_detail


class TransactionEvidenceTests(unittest.TestCase):
    def test_actual_ipv6_control_requires_exact_api_shape_and_refusal(self):
        created = []
        mode = "valid"
        def k(*args, value=None):
            if args[0] == "create":
                created.append(copy.deepcopy(value)); return ""
            if args[3] == "service": return "present" if mode == "service" else ""
            item = copy.deepcopy(created[-1])
            item["metadata"]["uid"] = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
            if mode == "expanded": item["endpoints"][0]["addresses"] = [fixture.IPV6_EXPANDED]
            if mode == "target": item["endpoints"][0]["targetRef"] = {"uid": "other"}
            if mode == "unknown": item["endpoints"][0]["conditions"] = {}
            return json.dumps(item)
        control = fixture.ipv6_slice_control(k)
        self.assertTrue(control["absenceRefused"])
        self.assertEqual(control["shape"]["endpoints"][0]["addresses"], ["2001:db8::1"])
        for mode in ("service", "expanded", "target", "unknown"):
            with self.subTest(mode=mode), self.assertRaises((ValueError, AssertionError)):
                fixture.ipv6_slice_control(k)
        mode = "valid"
        from unittest.mock import patch
        with patch.object(fixture.rollout_probe, "endpoint_boundary", return_value=None):
            with self.assertRaises(ValueError): fixture.ipv6_slice_control(k)
        source = inspect.getsource(fixture.run_fixture)
        call = 'report["ipv6EndpointSlice"] = ipv6_slice_control(k)'
        self.assertLess(source.index('report["cases"].append("empty_endpoint_slice_null")'), source.index(call))
        self.assertLess(source.index(call), source.index('report["cases"].append("ipv6_endpoint_metadata")'))
        self.assertLess(source.index(call), source.index("probes.create()"))

    def test_synthetic_runtime_detail_removes_variables_and_bounds(self):
        raw = 'failed to resolve image "private-name": https://example.invalid/token /private/path token=secret 127.0.0.1:123 sha256:' + 'a' * 64
        value = synthetic_runtime_detail(raw)
        for private in ('private-name', 'example.invalid', '/private', 'secret', '127.0.0.1', 'a' * 64):
            self.assertNotIn(private, value)
        self.assertIn('failed to resolve image', value)
        self.assertLessEqual(len(synthetic_runtime_detail('x' * 4096)), 512)
        self.assertEqual(synthetic_runtime_detail('x' * 4097), 'unavailable')

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

    def test_actual_null_control_shape_ownership_and_wiring(self):
        service_uid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        item = {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "addressType": "IPv4", "endpoints": None,
                "metadata": {"namespace": fixture.tx.NAMESPACE, "uid": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
                             "ownerReferences": [{"kind": "Service", "name": "empty-endpoint-control", "uid": service_uid, "controller": True}]}}
        created = []
        def k(*args, value=None):
            if args[0] == "create":
                created.append(copy.deepcopy(value)); return ""
            if args[3] == "pods": return '{"items":[]}'
            if args[3] == "service":
                return json.dumps({"metadata": {"uid": service_uid}, "spec": created[-1]["spec"]})
            if args[3] == "endpointslices": return json.dumps({"items": [item]})
            raise AssertionError("unexpected operation")
        result = fixture.empty_slice_control(k)
        self.assertIn("endpoints", result["shape"])
        self.assertIsNone(result["shape"]["endpoints"])
        for change in ("array", "missing", "owner"):
            bad = copy.deepcopy(item)
            if change == "array": bad["endpoints"] = []
            elif change == "missing": del bad["endpoints"]
            else: bad["metadata"]["ownerReferences"][0]["uid"] = "foreign"
            original = item
            item = bad
            with self.subTest(change=change), self.assertRaises((ValueError, KeyError, AssertionError)):
                fixture.empty_slice_control(k)
            item = original
        text = inspect.getsource(fixture.run_fixture)
        required = ('report["emptyEndpointSlice"] = empty_slice_control(k)',
                    'report["cases"].append("empty_endpoint_slice_null")',
                    'rollout_probe.endpoint_boundary(slices, probes.addresses)')
        def wired(source):
            return all(source.count(part) >= 1 for part in required) and source.index(required[0]) < source.index("probes.create()")
        self.assertTrue(wired(text))
        for part in required:
            with self.subTest(part=part): self.assertFalse(wired(text.replace(part, "removed")))
        self.assertEqual(len(CASES), 20)
        self.assertEqual(CASES[1:9], ["response_loss_" + str(i) for i in range(1, 9)])

    def test_exact_source_attempt_complete_cases_cleanup_and_identity_required(self):
        report = {"format": "kanz-gateway-policy-transaction-proof-v1", "source": "a" * 40, "tree": "b" * 40,
                  "runID": "123", "runAttempt": "2", "renderSHA256": "c" * 64, "nodeImage": proof.feed.NODE,
                  "cluster": "cni-proof-" + "d" * 12, "cases": list(CASES), "passed": True, "cleanupPassed": True,
                  "emptyEndpointSlice": {"serviceUID": "a"*8+"-"+"a"*4+"-"+"a"*4+"-"+"a"*4+"-"+"a"*12,
                      "sliceUID": "b"*8+"-"+"b"*4+"-"+"b"*4+"-"+"b"*4+"-"+"b"*12,
                      "shape": {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "addressType": "IPv4", "endpoints": None}},
                  "ipv6EndpointSlice": {"sliceUID": "cccccccc-cccc-cccc-cccc-cccccccccccc", "serviceName": "ipv6-control-" + "a"*32,
                      "serviceAbsent": True, "observedAddress": fixture.IPV6_EXPANDED, "literalComparisonMiss": True, "absenceRefused": True,
                      "shape": {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "addressType": "IPv6",
                                "endpoints": [{"addresses": ["2001:db8::1"], "conditions": {"ready": False}}]}},
                  "imageIdentity": {"root": rollout_probe.IMAGE.split("@", 1)[1], "manifest": "sha256:" + "e" * 64,
                                    "config": "sha256:" + "f" * 64, "platform": "linux/amd64", "rootMediaType": "application/vnd.oci.image.manifest.v1+json"}}
        def check(value):
            validate_report(value, "a" * 40, "b" * 40, "123", "2", "c" * 64)
        check(report)
        for key, value in (("source", "e" * 40), ("tree", "e" * 40), ("runID", "124"), ("runAttempt", "1"),
                           ("renderSHA256", "d" * 64), ("cases", CASES[:-1]), ("cases", list(reversed(CASES))),
                           ("passed", 1), ("cleanupPassed", False), ("imageIdentity", {}), ("emptyEndpointSlice", None), ("emptyEndpointSlice", {}),
                           ("ipv6EndpointSlice", None), ("ipv6EndpointSlice", {})):
            bad = copy.deepcopy(report); bad[key] = value
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                check(bad)
        for key, value in (("endpoints", []), ("endpoints", {}), ("endpoints", False), ("addressType", "IPv6")):
            bad = copy.deepcopy(report); bad["emptyEndpointSlice"]["shape"][key] = value
            with self.assertRaises(ValueError): check(bad)
        bad = copy.deepcopy(report); del bad["emptyEndpointSlice"]["shape"]["endpoints"]
        with self.assertRaises(ValueError): check(bad)
        for key, value in (("serviceAbsent", False), ("absenceRefused", False), ("literalComparisonMiss", False),
                           ("observedAddress", "2001:db8::1"), ("sliceUID", "unknown"), ("sliceUID", "-" * 36),
                           ("serviceName", "other")):
            bad = copy.deepcopy(report); bad["ipv6EndpointSlice"][key] = value
            with self.subTest(key=key), self.assertRaises(ValueError): check(bad)


if __name__ == "__main__":
    unittest.main()

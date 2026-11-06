import copy
import unittest
import uuid

import rollout_probe as probe


class ProbeBoundaryTests(unittest.TestCase):
    def running(self):
        desired = probe.caller_pod("kanz-services", "api-gateway", "a" * 32)
        actual = copy.deepcopy(desired)
        actual["metadata"]["uid"] = str(uuid.uuid4())
        actual["status"] = {"phase": "Running", "podIP": "10.0.0.5",
                            "conditions": [{"type": "Ready", "status": "False"}],
                            "containerStatuses": [{"name": "probe", "imageID": "sha256:" + "b" * 64,
                                                   "state": {"running": {}}}]}
        return desired, actual

    def test_never_ready_without_controller_or_tokens(self):
        desired, actual = self.running()
        self.assertEqual(probe.running_unready(actual, desired, "sha256:" + "b" * 64), "10.0.0.5")
        self.assertNotIn("pod-template-hash", desired["metadata"]["labels"])
        for mutation in ("ready", "gate", "sidecar", "token", "host", "owner", "env", "image"):
            bad = copy.deepcopy(actual)
            if mutation == "ready": bad["status"]["conditions"][0]["status"] = "True"
            elif mutation == "gate": bad["spec"]["readinessGates"] = []
            elif mutation == "sidecar": bad["spec"]["containers"].append({"name": "injected"})
            elif mutation == "token": bad["spec"]["automountServiceAccountToken"] = True
            elif mutation == "host": bad["spec"]["hostNetwork"] = True
            elif mutation == "owner": bad["metadata"]["ownerReferences"] = [{"kind": "ReplicaSet"}]
            elif mutation == "env": bad["spec"]["containers"][0]["env"] = [{"name": "UNEXPECTED", "value": "fixture"}]
            else: bad["status"]["containerStatuses"][0]["imageID"] = "sha256:" + "c" * 64
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                probe.running_unready(bad, desired, "sha256:" + "b" * 64)

    def test_matching_service_publish_unready_refuses(self):
        desired, _ = self.running()
        svc = {"metadata": {"namespace": "kanz-services"}, "spec": {
            "selector": {"app": "api-gateway"}, "publishNotReadyAddresses": True}}
        with self.assertRaises(ValueError):
            probe.service_boundary([svc], "kanz-services", desired["metadata"]["labels"])
        svc["spec"]["publishNotReadyAddresses"] = False
        probe.service_boundary([svc], "kanz-services", desired["metadata"]["labels"])

    def test_endpoint_unknown_ready_is_not_a_safe_false(self):
        for conditions in ({"ready": True}, {}, {"ready": None}):
            with self.subTest(conditions=conditions), self.assertRaises(ValueError):
                probe.endpoint_boundary([{"endpoints": [{"addresses": ["10.0.0.5"], "conditions": conditions}]}], ["10.0.0.5"])
        slices = [{"endpoints": [{"addresses": ["10.0.0.5"], "conditions": {"ready": False}}]}]
        probe.endpoint_boundary(slices, ["10.0.0.5"])
        with self.assertRaises(ValueError):
            probe.endpoint_boundary(slices, ["10.0.0.5"], require_absent=True)

    def test_minimal_labels_must_match_actual_policy_selection(self):
        policies = [{"spec": {"podSelector": {"matchLabels": {"app": "api-gateway"}}}}]
        real = {"app": "api-gateway", "pod-template-hash": "controller", "tenant": "fixture"}
        synthetic = {"app": "api-gateway", probe.OWNER: "a" * 32}
        probe.equivalent_source_selectors(policies, real, synthetic)
        policies[0]["spec"]["podSelector"]["matchLabels"]["tenant"] = "fixture"
        with self.assertRaises(ValueError):
            probe.equivalent_source_selectors(policies, real, synthetic)


if __name__ == "__main__":
    unittest.main()

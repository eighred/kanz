import copy
import unittest
import uuid

import rollout_probe as probe


class ProbeBoundaryTests(unittest.TestCase):
    def test_rendered_images_labels_and_controller_uid_lineage(self):
        labels = {"app": "audit"}
        images = {"containers": {"audit": probe.IMAGE}, "initContainers": {}}
        expected = {"audit": {"labels": labels, "selector": {"matchLabels": labels}, "images": images}}
        dep = {"metadata": {"name": "audit", "namespace": "kanz-services", "uid": "dep"},
               "spec": {"selector": {"matchLabels": labels}, "template": {"metadata": {"labels": labels},
                        "spec": {"containers": [{"name": "audit", "image": probe.IMAGE}]}}}}
        rs = copy.deepcopy(dep)
        rs["metadata"].update(name="audit-hash", uid="rs", ownerReferences=[{"kind": "Deployment", "name": "audit", "uid": "dep", "controller": True}])
        rs["spec"]["template"]["metadata"]["labels"] = {**labels, "pod-template-hash": "hash"}
        rs["spec"]["selector"] = {"matchLabels": {**labels, "pod-template-hash": "hash"}}
        pod = copy.deepcopy(rs["spec"]["template"])
        pod["metadata"].update(name="audit-hash-pod", namespace="kanz-services", uid="pod", ownerReferences=[{"kind": "ReplicaSet", "name": "audit-hash", "uid": "rs", "controller": True}])
        result = probe.workload_lineage(pod, [dep], [rs], expected)
        self.assertEqual((result["deployment"], result["replicaSet"]), ("dep", "rs"))
        for change in ("pod_image", "deployment_image", "rs_image", "labels", "pod_owner", "rs_owner", "selector", "host", "injected", "deleted"):
            p, d, r = copy.deepcopy((pod, dep, rs))
            if change.endswith("image"):
                spec = p["spec"] if change == "pod_image" else (d if change == "deployment_image" else r)["spec"]["template"]["spec"]
                spec["containers"][0]["image"] = "repo/image@sha256:" + "b" * 64
            elif change == "labels": p["metadata"]["labels"]["unexpected"] = "yes"
            elif change == "pod_owner": p["metadata"]["ownerReferences"][0]["uid"] = "other"
            elif change == "rs_owner": r["metadata"]["ownerReferences"][0]["uid"] = "other"
            elif change == "selector": d["spec"]["selector"] = {"matchLabels": {}}
            elif change == "host": p["spec"]["hostNetwork"] = True
            elif change == "injected": p["spec"]["containers"].append({"name": "extra", "image": probe.IMAGE})
            else: r["metadata"]["deletionTimestamp"] = "fixture"
            with self.subTest(change=change), self.assertRaises(ValueError):
                probe.workload_lineage(p, [d], [r], expected)

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

    def test_matching_controller_refused_before_probe_creation(self):
        labels = {"app": "api-gateway", probe.OWNER: "a" * 32}
        controller = {"kind": "ReplicaSet", "metadata": {"namespace": "kanz-services"},
                      "spec": {"selector": {"matchLabels": {"app": "api-gateway", "pod-template-hash": "production"}}}}
        probe.controller_boundary([controller], "kanz-services", labels)
        del controller["spec"]["selector"]["matchLabels"]["pod-template-hash"]
        with self.assertRaises(ValueError):
            probe.controller_boundary([controller], "kanz-services", labels)
        controller["spec"]["selector"] = {"matchExpressions": [{"key": "app", "operator": "In", "values": ["api-gateway"]}]}
        with self.assertRaises(ValueError):
            probe.controller_boundary([controller], "kanz-services", labels)


if __name__ == "__main__":
    unittest.main()

"""Offline controls for omissions that could make a green Kubernetes receipt lie."""
import copy
import hashlib
import json
import unittest

import proof


class QueryControls(unittest.TestCase):
    def response(self, values):
        return {"status": "success", "data": {"resultType": "vector", "result": [
            {"metric": {"service": key[0], "slo": key[1]}, "value": [10, str(value)]}
            for key, value in values.items()]}}

    def test_all_five_real_ratios_are_required(self):
        golden = {("api-gateway", "availability"): 100, ("api-gateway", "query-latency"): 5,
                  ("risk-engine", "recompute-success"): 40, ("risk-engine", "recompute-latency"): 30,
                  ("event-bus", "delivery-success"): 150}
        self.assertEqual(proof.oracle("burn", proof.vector(self.response(golden))), "independent_counter_ratio")
        for changed in ({}, {key: 0 for key in golden}, {**golden, ("other", "unknown"): 1},
                        {**golden, ("api-gateway", "availability"): float("nan")}):
            with self.subTest(changed=changed), self.assertRaises(proof.ProofError):
                proof.oracle("burn", proof.vector(self.response(changed)))

    def test_partial_history_cannot_be_a_clean_budget(self):
        expr = "slo:error_budget_consumed:ratio"
        proof.oracle(expr, {key: -1 for key in proof.BURNS})
        with self.assertRaises(proof.ProofError):
            proof.oracle(expr, {key: 0 for key in proof.BURNS})
        proof.oracle("slo:budget_window_coverage:ratio", {(None, None): 0.0001})
        with self.assertRaises(proof.ProofError):
            proof.oracle("slo:budget_window_coverage:ratio", {(None, None): 1})

    def test_equality_cannot_hide_a_shared_wrong_answer(self):
        wrong = {key: 0 for key in proof.BURNS}
        proof.same_values(wrong, wrong)
        with self.assertRaises(proof.ProofError):
            proof.oracle("burn", wrong)
        with self.assertRaises(proof.ProofError):
            proof.same_values(proof.BURNS, wrong)

    def test_grafana_backend_must_produce_each_labeled_value(self):
        frames = [{"schema": {"fields": [{"type": "time"}, {"type": "number",
            "labels": {"service": service, "slo": slo}}]}, "data": {"values": [[1000], [value]]}}
            for (service, slo), value in proof.BURNS.items()]
        body = {"results": {"A": {"status": 200, "frames": frames}}}
        self.assertEqual(proof.grafana_frames(body), proof.BURNS)
        for mutation in ({"results": {}}, {"results": {"A": {"frames": []}}},
                         {"results": {"A": {"error": "refused", "frames": frames}}},
                         {"results": {"A": {"frames": frames + frames[:1]}}}):
            with self.subTest(mutation=mutation), self.assertRaises(proof.ProofError):
                proof.grafana_frames(mutation)

    def test_coverage_frame_is_an_unlabeled_singleton(self):
        body = {"results": {"A": {"frames": [{"schema": {"fields": [
            {"type": "time"}, {"type": "number"}]}, "data": {"values": [[1000], [0.0001]]}}]}}}
        self.assertEqual(proof.grafana_frames(body, {(None, None)}), {(None, None): 0.0001})


class KubernetesControls(unittest.TestCase):
    def test_missing_secret_requires_the_owned_failed_mount_and_no_started_container(self):
        pod = {"metadata": {"uid": "missing-pod"}, "status": {"phase": "Pending"}}
        event = {"involvedObject": {"uid": "missing-pod"}, "reason": "FailedMount", "message": "grafana-admin not found"}
        self.assertEqual(proof.check_missing([pod], [event]), "missing-pod")
        started = copy.deepcopy(pod)
        started["status"]["containerStatuses"] = [{"containerID": "containerd://started"}]
        foreign_event = {**event, "involvedObject": {"uid": "another-pod"}}
        for pods, events in (([started], [event]), ([pod], []), ([pod], [foreign_event]), ([], [event])):
            with self.subTest(pods=pods, events=events), self.assertRaises(proof.ProofError):
                proof.check_missing(pods, events)

    def test_ready_pod_with_a_different_image_is_not_acceptance(self):
        image = "grafana@sha256:" + "a" * 64
        pod = {"metadata": {"uid": "running-pod"}, "spec": {"containers": [{"name": "grafana", "image": image}]},
               "status": {"containerStatuses": [{"name": "grafana", "ready": True, "restartCount": 0,
                    "state": {"running": {}}, "imageID": "docker-pullable://grafana@sha256:" + "b" * 64}]}}
        proof.image_identity(pod, "grafana", image)
        for key, value in (("ready", False), ("restartCount", 1), ("imageID", "latest")):
            changed = copy.deepcopy(pod)
            changed["status"]["containerStatuses"][0][key] = value
            with self.subTest(key=key), self.assertRaises(proof.ProofError):
                proof.image_identity(changed, "grafana", image)
        with self.assertRaises(proof.ProofError):
            proof.image_identity(pod, "grafana", "another-image")

    def test_policy_is_scoped_to_grafana_and_required_directions(self):
        policy = next(d for d in proof.docs(proof.OBS / "grafana.yaml") if d["kind"] == "NetworkPolicy")
        spec = policy["spec"]
        self.assertEqual(spec["podSelector"], {"matchLabels": {"app": "grafana"}})
        self.assertEqual(set(spec["policyTypes"]), {"Ingress", "Egress"})
        self.assertEqual(spec["ingress"], [{"from": [{"podSelector": {"matchLabels": {"app": "prometheus"}}}],
                                           "ports": [{"protocol": "TCP", "port": 3000}]}])
        self.assertEqual(spec["egress"][0], {"to": [{"podSelector": {"matchLabels": {"app": "prometheus"}}}],
                                            "ports": [{"protocol": "TCP", "port": 9090}]})
        self.assertEqual(len(spec["egress"]), 2)
        dns = spec["egress"][1]
        self.assertEqual(dns["to"], [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                                    "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}])
        self.assertEqual(dns["ports"], [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}])


class ReceiptControls(unittest.TestCase):
    def receipt(self):
        dashboard = json.loads((proof.OBS / "dashboards/slo-burn-rate.json").read_text(encoding="utf-8"))
        targets = proof.panel_targets(dashboard)
        images = {}
        for app in ("grafana", "prometheus"):
            deployment = next(d for d in proof.docs(proof.OBS / (app + ".yaml")) if d["kind"] == "Deployment")
            images[app] = {"declaredImage": deployment["spec"]["template"]["spec"]["containers"][0]["image"],
                           "podUID": app, "loadedImageID": "sha256:" + "b" * 64}
        panels = []
        for p, r, expr in targets:
            if expr == "slo:budget_window_coverage:ratio":
                values = {(None, None): 0.0001}
            elif expr == "slo:error_budget:ratio":
                values = proof.BUDGETS
            elif "consumed" in expr:
                values = {key: -1 for key in proof.BURNS}
            else:
                values = proof.BURNS
            panels.append({"panel": p, "ref": r, "expressionSHA256": hashlib.sha256(expr.encode()).hexdigest(),
                           "series": len(values), "oracle": proof.oracle(expr, values),
                           "values": [{"service": key[0], "slo": key[1], "value": value} for key, value in values.items()]})
        return {"schema": 1, "source": "a" * 40, "tree": "b" * 40, "cluster": "cni-proof-123456abcdef",
                "run": "123", "attempt": "1", "missingSecretPodUID": "missing-pod",
                "inputs": proof.input_hashes(), "images": images,
                "cases": [{"id": name, "verdict": "PASS"} for name in proof.CASES],
                "panels": panels,
                "verdict": "PASS", "cleanup": "verified_absent"}

    def test_full_receipt_cannot_omit_or_relabel_a_required_case(self):
        original = self.receipt()
        proof.validate_report(original, "a" * 40, "b" * 40)
        changes = []
        for index in range(len(proof.CASES)):
            changed = copy.deepcopy(original)
            changed["cases"].pop(index)
            changes.append(changed)
        for key, value in (("source", "c" * 40), ("tree", "c" * 40), ("cleanup", "unknown"),
                           ("verdict", "SKIP"), ("images", {}), ("inputs", {}), ("panels", [])):
            changed = copy.deepcopy(original)
            changed[key] = value
            changes.append(changed)
        for changed in changes:
            with self.subTest(changed=changed), self.assertRaises(proof.ProofError):
                proof.validate_report(changed, "a" * 40, "b" * 40)

    def test_panel_expression_image_and_series_are_bound(self):
        for key, value in (("expressionSHA256", "c" * 64), ("series", 0), ("panel", 999)):
            changed = self.receipt()
            changed["panels"][0][key] = value
            with self.subTest(key=key), self.assertRaises(proof.ProofError):
                proof.validate_report(changed, "a" * 40, "b" * 40)
        changed = self.receipt()
        changed["images"]["grafana"]["declaredImage"] = "grafana:latest"
        with self.assertRaises(proof.ProofError):
            proof.validate_report(changed, "a" * 40, "b" * 40)


if __name__ == "__main__":
    unittest.main()

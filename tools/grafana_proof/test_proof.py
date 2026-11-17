"""Offline controls for omissions that could make a green Kubernetes receipt lie."""
import copy
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
import urllib.parse
from unittest import mock

import proof


def owner_record():
    return {"schema": 1, "kind": "grafana-kubernetes", "nonce": "123456abcdef",
            "cluster": "cni-proof-123456abcdef", "source": "a" * 40, "tree": "b" * 40,
            "run": "123", "attempt": "1"}


class OwnershipControls(unittest.TestCase):
    def test_only_a_clean_independently_selected_source_can_start(self):
        proof.validate_source("a" * 40, "", "a" * 40)
        for head, status, expected in (("b" * 40, "", "a" * 40),
                                       ("a" * 40, " M source.py", "a" * 40),
                                       ("main", "", "main")):
            with self.subTest(head=head, status=status), self.assertRaises(proof.ProofError):
                proof.validate_source(head, status, expected)
        with mock.patch.dict(proof.os.environ, {"GITHUB_SHA": "a" * 40, "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1"}):
            self.assertEqual(proof.expected_identity(), ("a" * 40, "123", "1"))
            with mock.patch.dict(proof.os.environ, {"GITHUB_RUN_ATTEMPT": "0"}), self.assertRaises(proof.ProofError):
                proof.expected_identity()

    def test_cleanup_preserves_another_matching_prefix_cluster(self):
        clusters = {"cni-proof-123456abcdef", "cni-proof-fedcba654321", "other-task"}
        deleted = []
        def execute(args, timeout=120):
            if args == ["kind", "get", "clusters"]:
                return "\n".join(sorted(clusters))
            self.assertEqual(args[:4], ["kind", "delete", "cluster", "--name"])
            self.assertEqual(args[4], "cni-proof-123456abcdef")
            self.assertEqual(args[5], "--kubeconfig")
            self.assertTrue(Path(args[6]).is_absolute())
            deleted.append(args[4])
            clusters.remove(args[4])
            return ""
        with mock.patch.object(proof, "read_owner", return_value=owner_record()):
            proof.cleanup_owned(Path("unused"), "a" * 40, "b" * 40, "123", "1", execute)
        self.assertEqual(deleted, ["cni-proof-123456abcdef"])
        self.assertEqual(clusters, {"cni-proof-fedcba654321", "other-task"})

    def test_a_stale_or_mismatched_owner_cannot_authorize_any_command(self):
        for field, value in (("source", "c" * 40), ("tree", "c" * 40), ("run", "999"),
                             ("attempt", "2"), ("nonce", "fedcba654321"), ("kind", "other-task")):
            owner = owner_record()
            owner[field] = value
            execute = mock.Mock()
            with self.subTest(field=field), mock.patch.object(proof, "read_owner", return_value=owner):
                with self.assertRaises(proof.ProofError):
                    proof.cleanup_owned(Path("unused"), "a" * 40, "b" * 40, "123", "1", execute)
            execute.assert_not_called()

    def test_cleanup_cannot_report_success_after_removing_a_foreign_cluster(self):
        execute = mock.Mock(side_effect=["cni-proof-123456abcdef\ncni-proof-fedcba654321", "", ""])
        with mock.patch.object(proof, "read_owner", return_value=owner_record()), self.assertRaises(proof.ProofError):
            proof.cleanup_owned(Path("unused"), "a" * 40, "b" * 40, "123", "1", execute)

    def test_owner_creation_is_exclusive_and_reader_refuses_symlinks(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "owner.json"
            proof.write_owner(path, owner_record())
            self.assertEqual(proof.read_owner(path), owner_record())
            with self.assertRaises(FileExistsError):
                proof.write_owner(path, owner_record())
            link = Path(directory) / "link.json"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                proof.read_owner(link)


class ProbeProvenanceControls(unittest.TestCase):
    NONCE = "a" * 32

    def result(self, outcome, code=0, stderr=""):
        prefix = "KANZ_GRAFANA_PROBE_V1:" + self.NONCE + ":"
        return subprocess.CompletedProcess([], code, prefix + "START\n" + prefix + "RESULT:" + outcome + "\n", stderr)

    def test_only_completed_target_network_timeout_means_denied(self):
        self.assertFalse(proof.probe_result(self.result("NETWORK_TIMEOUT"), self.NONCE))
        self.assertTrue(proof.probe_result(self.result("ALLOWED"), self.NONCE))
        for result in (subprocess.CompletedProcess([], 1, "", "error: timed out waiting for the condition"),
                       self.result("NETWORK_TIMEOUT", 1, "error: timed out waiting for the condition"),
                       subprocess.CompletedProcess([], 1, "", "wget: download timed out"),
                       self.result("TOOL_MISSING"), self.result("TOOL_FAILURE"),
                       self.result("NETWORK_TIMEOUT", stderr="exec container unavailable")):
            with self.subTest(result=result), self.assertRaises(proof.ProofError):
                proof.probe_result(result, self.NONCE)

    def test_wrong_or_incomplete_nonce_cannot_credit_a_denial(self):
        for result, nonce in ((self.result("NETWORK_TIMEOUT"), "b" * 32),
                              (subprocess.CompletedProcess([], 0, "", ""), self.NONCE),
                              (subprocess.CompletedProcess([], 0, "timed out\n", ""), self.NONCE)):
            with self.subTest(result=result), self.assertRaises(proof.ProofError):
                proof.probe_result(result, nonce)

    def test_exec_names_the_container_and_carries_target_markers(self):
        with mock.patch.object(proof.uuid, "uuid4") as identity, mock.patch.object(proof.subprocess, "run") as run:
            identity.return_value.hex = self.NONCE
            run.return_value = self.result("NETWORK_TIMEOUT")
            self.assertFalse(proof.probe_http(["kubectl", "--kubeconfig", "/owned/config"], "owned", "pod", "grafana", "http://other:9090/"))
            args = run.call_args.args[0]
            self.assertEqual(args[args.index("exec") + 1:args.index("--")], ["pod", "-c", "grafana"])
            self.assertEqual(args[-2:], ["http://other:9090/", self.NONCE])
            self.assertIn("'wget: download timed out'", proof.HTTP_PROBE)
            self.assertEqual(run.call_args.kwargs["timeout"], 12)


class QueryControls(unittest.TestCase):
    def response(self, values):
        return {"status": "success", "data": {"resultType": "vector", "result": [
            {"metric": {"service": key[0], "slo": key[1]}, "value": [10, str(value)]}
            for key, value in values.items()]}}

    def readiness_targets(self):
        dashboard = json.loads((proof.OBS / "dashboards/slo-burn-rate.json").read_text(encoding="utf-8"))
        return proof.panel_targets(dashboard)

    def readiness_response(self, expr):
        if expr == "slo:budget_window_coverage:ratio":
            values = {(None, None): 0.0001}
        elif expr == "slo:error_budget:ratio":
            values = proof.BUDGETS
        elif "consumed" in expr:
            values = {key: -1 for key in proof.BURNS}
        else:
            values = proof.BURNS
        return self.response(values)

    def test_readiness_waits_for_delayed_coverage_after_the_first_ten_targets_are_ready(self):
        targets = self.readiness_targets()
        queries = []
        clock = {"now": 100}
        def sleep(seconds):
            clock["now"] += seconds
        def request(port, path, *, timeout):
            self.assertEqual(port, 12345)
            self.assertEqual(timeout, 10)
            params = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)
            expr, timestamp = params["query"][0], params["time"][0]
            queries.append((expr, timestamp))
            if expr == "slo:budget_window_coverage:ratio" and len(queries) == 11:
                return self.response({})
            return self.readiness_response(expr)
        with mock.patch.object(proof, "request", side_effect=request), \
                mock.patch.object(proof.time, "time_ns", side_effect=[1794925939123456789, 1794925939456789123]), \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock["now"]), \
                mock.patch.object(proof.time, "sleep", side_effect=sleep) as sleeping:
            timestamp_ms = proof.wait_panel_readiness(12345, targets)
        self.assertEqual(timestamp_ms, 1794925939456)
        self.assertEqual([expr for expr, _ in queries], [expr for _, _, expr in targets] * 2)
        self.assertEqual([value for _, value in queries[:11]], ["1794925939.123"] * 11)
        self.assertEqual([value for _, value in queries[11:]], ["1794925939.456"] * 11)
        sleeping.assert_called_once_with(3)

    def test_permanently_missing_coverage_exhausts_the_existing_240_second_budget(self):
        targets = self.readiness_targets()
        queries = []
        clock = {"now": 100}
        def sleep(seconds):
            clock["now"] += seconds
        def request(_port, path, *, timeout):
            expr = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)["query"][0]
            queries.append(expr)
            if expr == "slo:budget_window_coverage:ratio":
                clock["now"] = 337
                return self.response({})
            return self.readiness_response(expr)
        with mock.patch.object(proof, "request", side_effect=request), \
                mock.patch.object(proof.time, "time_ns", return_value=1794925939123456789), \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock["now"]), \
                mock.patch.object(proof.time, "sleep", side_effect=sleep) as sleeping:
            with self.assertRaisesRegex(proof.ProofError, "^bounded_observation_timeout$"):
                proof.wait_panel_readiness(12345, targets)
        self.assertEqual(queries, [expr for _, _, expr in targets])
        sleeping.assert_called_once_with(3)
        self.assertEqual(clock["now"], 340)

    def test_readiness_uses_one_fractional_millisecond_timestamp_and_refuses_invalid_time(self):
        targets = self.readiness_targets()
        queries = []
        def request(_port, path, *, timeout):
            params = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)
            queries.append(params["time"][0])
            return self.readiness_response(params["query"][0])
        with mock.patch.object(proof, "request", side_effect=request), \
                mock.patch.object(proof.time, "time_ns", return_value=1794925939999999999), \
                mock.patch.object(proof.time, "monotonic", return_value=100), \
                mock.patch.object(proof.time, "sleep") as sleep:
            timestamp_ms = proof.wait_panel_readiness(12345, targets)
        self.assertEqual(timestamp_ms, 1794925939999)
        self.assertEqual(queries, ["1794925939.999"] * 11)
        self.assertNotEqual(timestamp_ms / 1000, int(timestamp_ms / 1000))
        self.assertEqual(str(timestamp_ms - 300_000), "1794925639999")
        self.assertEqual(str(timestamp_ms), "1794925939999")
        sleep.assert_not_called()
        for invalid in (0, -1, True, 1 << 63):
            with self.subTest(time_ns=invalid), \
                    mock.patch.object(proof, "request") as request, \
                    mock.patch.object(proof.time, "time_ns", return_value=invalid), \
                    mock.patch.object(proof.time, "monotonic", return_value=100), \
                    mock.patch.object(proof.time, "sleep") as sleep:
                with self.assertRaisesRegex(proof.ProofError, "^query_timestamp_out_of_range$"):
                    proof.wait_panel_readiness(12345, targets)
                request.assert_not_called()
                sleep.assert_not_called()

    def test_complete_readiness_identities_do_not_replace_the_independent_oracle(self):
        targets = self.readiness_targets()
        clock = {"now": 100}
        def request(_port, path, *, timeout):
            expr = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)["query"][0]
            if expr == "slo:budget_window_coverage:ratio":
                return self.response({(None, None): 1})
            return self.readiness_response(expr)
        def sleep(seconds):
            self.assertEqual(seconds, 3)
            clock["now"] = 340
        with mock.patch.object(proof, "request", side_effect=request) as request, \
                mock.patch.object(proof.time, "time_ns", return_value=1794925939123456789), \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock["now"]), \
                mock.patch.object(proof.time, "sleep", side_effect=sleep) as sleeping:
            with self.assertRaisesRegex(proof.ProofError, "^bounded_observation_timeout$"):
                proof.wait_panel_readiness(12345, targets)
        self.assertEqual(request.call_count, 11)
        sleeping.assert_called_once_with(3)

    def test_readiness_refuses_successful_last_reply_after_the_joint_deadline(self):
        targets = self.readiness_targets()
        clock = {"now": 100}
        queries = []
        def request(_port, path, *, timeout):
            expr = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)["query"][0]
            queries.append(expr)
            if len(queries) == len(targets):
                clock["now"] = 340.001
            return self.readiness_response(expr)
        with mock.patch.object(proof, "request", side_effect=request), \
                mock.patch.object(proof.time, "time_ns", return_value=1794925939123456789), \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock["now"]), \
                mock.patch.object(proof.time, "sleep") as sleep:
            with self.assertRaisesRegex(proof.ProofError, "^bounded_observation_timeout$"):
                proof.wait_panel_readiness(12345, targets)
        self.assertEqual(queries, [expr for _, _, expr in targets])
        sleep.assert_not_called()

    def test_readiness_never_launches_the_next_query_after_oracle_consumes_the_budget(self):
        targets = self.readiness_targets()
        clock = {"now": 100}
        actual_oracle = proof.oracle
        def request(_port, path, *, timeout):
            expr = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)["query"][0]
            return self.readiness_response(expr)
        def oracle(expr, values):
            result = actual_oracle(expr, values)
            clock["now"] = 340
            return result
        with mock.patch.object(proof, "request", side_effect=request) as requesting, \
                mock.patch.object(proof, "oracle", side_effect=oracle) as observing, \
                mock.patch.object(proof.time, "time_ns", return_value=1794925939123456789), \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock["now"]), \
                mock.patch.object(proof.time, "sleep") as sleep:
            with self.assertRaisesRegex(proof.ProofError, "^bounded_observation_timeout$"):
                proof.wait_panel_readiness(12345, targets)
        self.assertEqual(requesting.call_count, 1)
        self.assertEqual(observing.call_count, 1)
        sleep.assert_not_called()

    def test_readiness_clips_transport_timeout_and_sleep_without_resetting_the_deadline(self):
        targets = self.readiness_targets()
        clock = {"now": 100}
        timeouts = []
        def timestamp():
            clock["now"] = 338.5
            return 1794925939123456789
        def request(_port, path, *, timeout):
            timeouts.append(timeout)
            expr = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)["query"][0]
            return self.response({}) if expr == "slo:budget_window_coverage:ratio" else self.readiness_response(expr)
        def sleep(seconds):
            clock["now"] += seconds
        with mock.patch.object(proof, "request", side_effect=request), \
                mock.patch.object(proof.time, "time_ns", side_effect=timestamp) as sampling, \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock["now"]), \
                mock.patch.object(proof.time, "sleep", side_effect=sleep) as sleeping:
            with self.assertRaisesRegex(proof.ProofError, "^bounded_observation_timeout$"):
                proof.wait_panel_readiness(12345, targets)
        self.assertEqual(timeouts, [1.5] * len(targets))
        sampling.assert_called_once_with()
        sleeping.assert_called_once_with(1.5)
        self.assertEqual(clock["now"], 340)
        # The real HTTP helper must forward the clipped timeout, while unrelated
        # callers retain their existing ten-second default. No socket is opened.
        opener = mock.MagicMock()
        opener.open.return_value.__enter__.return_value.read.return_value = b"{}"
        with mock.patch.object(proof.urllib.request, "build_opener", return_value=opener):
            proof.request(12345, "/api/v1/query?query=vector(1)", timeout=1.5)
            self.assertEqual(opener.open.call_args.kwargs["timeout"], 1.5)
            proof.request(12345, "/api/v1/query?query=vector(1)")
            self.assertEqual(opener.open.call_args.kwargs["timeout"], 10)

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
                "owner": owner_record(),
                "run": "123", "attempt": "1", "missingSecretPodUID": "missing-pod",
                "inputs": proof.input_hashes(), "images": images,
                "cases": [{"id": name, "verdict": "PASS"} for name in proof.CASES],
                "panels": panels,
                "verdict": "PASS", "cleanup": "verified_absent"}

    def test_full_receipt_cannot_omit_or_relabel_a_required_case(self):
        original = self.receipt()
        proof.validate_report(original, "a" * 40, "b" * 40, "123", "1")
        changes = []
        for index in range(len(proof.CASES)):
            changed = copy.deepcopy(original)
            changed["cases"].pop(index)
            changes.append(changed)
        for key, value in (("source", "c" * 40), ("tree", "c" * 40), ("cleanup", "unknown"),
                           ("verdict", "SKIP"), ("images", {}), ("inputs", {}), ("panels", []),
                           ("run", "999"), ("attempt", "2"), ("owner", None)):
            changed = copy.deepcopy(original)
            changed[key] = value
            changes.append(changed)
        for changed in changes:
            with self.subTest(changed=changed), self.assertRaises(proof.ProofError):
                proof.validate_report(changed, "a" * 40, "b" * 40, "123", "1")

    def test_panel_expression_image_and_series_are_bound(self):
        for key, value in (("expressionSHA256", "c" * 64), ("series", 0), ("panel", 999)):
            changed = self.receipt()
            changed["panels"][0][key] = value
            with self.subTest(key=key), self.assertRaises(proof.ProofError):
                proof.validate_report(changed, "a" * 40, "b" * 40, "123", "1")
        changed = self.receipt()
        changed["images"]["grafana"]["declaredImage"] = "grafana:latest"
        with self.assertRaises(proof.ProofError):
            proof.validate_report(changed, "a" * 40, "b" * 40, "123", "1")


if __name__ == "__main__":
    unittest.main()

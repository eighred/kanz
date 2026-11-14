"""Refusal controls for the receipt and source-derived assertions; no cluster."""
import base64
import copy
import importlib.util
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("nats_gitops_proof", Path(__file__).with_name("proof.py"))
proof = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(proof)
SHA = "a" * 40


class ProofTests(unittest.TestCase):
    def test_diagnostics_keep_categories_without_raw_content(self):
        for raw, category in (("lookup secret-host: no such host", "dns-unavailable"),
                              ("unknown flag: credential-example", "unsupported-cli-flag"),
                              ("ImagePullBackOff private-registry", "image-pull-unavailable"),
                              ("opaque token-looking diagnostic", "unclassified-refusal")):
            self.assertEqual(proof.diagnostic_category(raw), category)

    def canonical(self):
        docs = proof.documents("kanz/infra/nats/bootstrap-job.yaml")
        script = next(d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "nats-bootstrap")["data"]["bootstrap.sh"]
        return proof.expected_streams(script, 3)

    def test_full_source_inventory_and_semantics(self):
        expected = self.canonical()
        self.assertEqual(len(expected), 18)
        self.assertEqual(expected["AUTHORITY_AUDIT"]["discard"], "new")
        self.assertEqual(expected["AUTHORITY_AUDIT"]["max_age"], 0)
        self.assertEqual(expected["POSITION"]["max_msgs_per_subject"], 1)
        actual = {name: {"config": value} for name, value in expected.items()}
        proof.check_inventory(expected, actual)
        for mutation in ("missing", "extra", "memory", "subjects", "retention", "replicas", "age", "discard", "dedup", "cap"):
            with self.subTest(mutation=mutation):
                broken = copy.deepcopy(actual)
                if mutation == "missing":
                    broken.pop("RISK")
                elif mutation == "extra":
                    broken["UNTRACKED"] = broken["RISK"]
                else:
                    field, value = {"memory": ("storage", "memory"), "subjects": ("subjects", [">"]),
                        "retention": ("retention", "workqueue"), "replicas": ("num_replicas", 1),
                        "age": ("max_age", 1), "discard": ("discard", "old"), "dedup": ("duplicate_window", 0),
                        "cap": ("max_msgs_per_subject", 9)}[mutation]
                    name = "POSITION" if mutation == "cap" else "AUTHORITY_AUDIT"
                    broken[name]["config"][field] = value
                with self.assertRaises(ValueError):
                    proof.check_inventory(expected, broken)

    def test_empty_unresolved_and_duplicate_source_refused(self):
        for source in ("", 'ensure_stream X "x.>" 1h', 'ensure_stream X "x.>" mystery',
                       'ensure_stream X "x.>" 1h\nensure_stream X "x.>" 1h'):
            with self.assertRaises(ValueError):
                proof.expected_streams(source, 3)

    def test_exact_pr_merge_and_main_sources_only(self):
        self.assertEqual(proof.source_ref("pull_request", {"number": 123}, SHA), "refs/pull/123/merge")
        self.assertEqual(proof.source_ref("push", {"ref": "refs/heads/main", "after": SHA}, SHA), SHA)
        for name, event in (("workflow_dispatch", {}), ("pull_request", {"number": "123"}),
                            ("push", {"ref": "refs/heads/main", "after": "b" * 40})):
            with self.assertRaises(ValueError):
                proof.source_ref(name, event, SHA)

    def operation(self):
        token = [{"name": "case", "value": "one"}]
        hook = {"kind": "Job", "name": "nats-bootstrap", "namespace": proof.NAMESPACE,
                "syncPhase": "PostSync", "hookPhase": "Succeeded"}
        state = {"operation": {"info": token}, "phase": "Succeeded", "startedAt": "one", "finishedAt": "two",
                 "syncResult": {"revision": SHA, "resources": [hook]}}
        return {"status": {"operationState": state}}, token

    def test_controller_result_requires_source_hook_and_phase(self):
        app, token = self.operation()
        self.assertEqual(proof.check_operation(app, SHA, token)["revision"], SHA)
        for mutation in ("source", "missing-hook", "phase", "manual-job", "wrong-namespace"):
            broken = copy.deepcopy(app)
            state = broken["status"]["operationState"]
            if mutation == "source":
                state["syncResult"]["revision"] = "b" * 40
            elif mutation == "missing-hook":
                state["syncResult"]["resources"] = []
            elif mutation == "phase":
                state["phase"] = "Failed"
            else:
                hook = state["syncResult"]["resources"][0]
                hook["syncPhase" if mutation == "manual-job" else "namespace"] = "Other"
            with self.assertRaises(ValueError):
                proof.check_operation(broken, SHA, token)
        self.assertIsNone(proof.check_operation(app, SHA, [{"name": "other"}]))

    def test_exact_record_payload_headers_and_sequence(self):
        fact = {"subject": "market.crypto.quote", "frame": base64.b64encode(b"canonical-fixture").decode(),
                "headers": {"Nats-Msg-Id": "fixture", "X-Kanz-Fixture": "nats-gitops"}}
        record = {"subject": fact["subject"], "seq": 1, "data": fact["frame"], "time": "2026-11-15T00:00:00Z",
                  "hdrs": base64.b64encode(b"NATS/1.0\r\nNats-Msg-Id:fixture\r\nX-Kanz-Fixture:nats-gitops\r\n\r\n").decode()}
        self.assertEqual(proof.checked_record(record, fact), record)
        for key, value in (("seq", 2), ("subject", "order.order.submit"), ("data", "b3RoZXI="), ("hdrs", "")):
            with self.assertRaises(ValueError):
                proof.checked_record(record | {key: value}, fact)

    def receipt(self):
        operation = {"revision": SHA, "phase": "Succeeded", "hookPhase": "Succeeded", "syncPhase": "PostSync"}
        claim = {"podUID": "pod-one", "pvc": "data-nats-0", "pvcUID": "pvc", "pv": "pv", "pvUID": "pv-uid"}
        return {"source": SHA, "tree": "b" * 40, "passed": True, "cleanupPassed": True, "cases": proof.CASES[:],
                "syncs": [operation | {"jobUID": "job-one", "operation": "first"}, operation | {"jobUID": "job-two", "operation": "second"}],
                "storage": [claim, claim | {"podUID": "pod-two"}], "factBefore": {"record": "one"},
                "factAfter": {"record": "one"}, "canonicalValidated": True, "negativeControlPassed": True}

    def test_receipt_refuses_partial_skipped_or_reused_proof(self):
        proof.check_report(self.receipt())
        for key, value in (("cases", proof.CASES[:-1]), ("cleanupPassed", False), ("canonicalValidated", False),
                           ("negativeControlPassed", False), ("passed", False), ("factAfter", {"record": "other"})):
            with self.assertRaises(ValueError):
                proof.check_report(self.receipt() | {key: value})
        for group, key in (("syncs", "jobUID"), ("syncs", "operation"), ("storage", "podUID")):
            broken = self.receipt()
            broken[group][1][key] = broken[group][0][key]
            with self.assertRaises(ValueError):
                proof.check_report(broken)
        broken = self.receipt()
        broken["storage"][1]["pvcUID"] = "replacement-volume"
        with self.assertRaises(ValueError):
            proof.check_report(broken)


if __name__ == "__main__":
    unittest.main()

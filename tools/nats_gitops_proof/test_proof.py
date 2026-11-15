"""Refusal controls for the receipt and source-derived assertions; no cluster."""
import base64
import copy
import fnmatch
import importlib.util
import json
from pathlib import Path
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("nats_gitops_proof", Path(__file__).with_name("proof.py"))
proof = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(proof)
SHA = "a" * 40


class ProofTests(unittest.TestCase):
    def test_go_writer_uses_readonly_socket_while_cli_keeps_canonical_pem_path(self):
        job = next(d for d in proof.documents("kanz/infra/nats/bootstrap-job.yaml") if d["kind"] == "Job")
        inspector = proof.client_spec(job, "nats-bootstrap")
        writer = proof.client_spec(job, "market-ingest", workload_api=True)
        proof.check_writer_socket(writer)
        self.assertFalse(any(m["mountPath"] == "/run/spiffe" for m in inspector["containers"][0]["volumeMounts"]))
        self.assertTrue(any(m["mountPath"] == "/etc/nats-certs" for m in inspector["containers"][0]["volumeMounts"]))
        for mutation in ("missing", "writable", "wrong-path", "wrong-identity"):
            with self.subTest(mutation=mutation):
                broken = copy.deepcopy(writer)
                mounts = broken["containers"][0]["volumeMounts"]
                socket = next(m for m in mounts if m["mountPath"] == "/run/spiffe")
                if mutation == "missing":
                    mounts.remove(socket)
                elif mutation == "writable":
                    socket["readOnly"] = False
                elif mutation == "wrong-path":
                    socket["mountPath"] = "/run/other"
                else:
                    broken["serviceAccountName"] = "nats-bootstrap"
                with self.assertRaises(ValueError):
                    proof.check_writer_socket(broken)

    def test_workflow_selects_resolved_canonical_publisher_inputs(self):
        workflow = proof.documents(".github/workflows/nats-gitops-verification.yml")[0]
        # PyYAML's YAML 1.1 resolver represents the workflow `on` key as True.
        triggers = workflow.get("on", workflow.get(True))
        # These edges are code and generated-SDK consumers, even though the
        # diagnostic publishes a FACT. bus/command.go still compiles with bus;
        # consumer.go and producer.go call the observability trace helpers.
        inputs = ("kanz/tools/nats_gitops_proof/main.go", "kanz/pkg/bus/consumer.go",
                  "kanz/pkg/transport/tls.go", "kanz/pkg/observability/trace.go",
                  "kanz-schemas/proto/command/v1/command.proto", "kanz-schemas/proto/common/v1/decimal.proto",
                  "kanz/go.mod", "kanz/go.sum")
        for event in ("pull_request", "push"):
            patterns = triggers[event]["paths"]
            def selected(values):
                return {path for path in inputs if any(fnmatch.fnmatchcase(path, pattern) for pattern in values)}
            for path in inputs:
                with self.subTest(event=event, path=path):
                    self.assertTrue((proof.ROOT / path).is_file())
            self.assertEqual(selected(patterns), set(inputs), "a resolved publisher input does not select its actual proof")
            for selector in ("kanz/pkg/observability/**", "kanz-schemas/proto/command/**"):
                # Delete only the reviewed edge, keeping every other filter.
                # The original missing-consumer defect must become observable.
                narrowed = [pattern for pattern in patterns if pattern != selector]
                self.assertNotEqual(selected(narrowed), set(inputs))
            self.assertFalse(any(fnmatch.fnmatchcase("kanz/internal/risk/pricing/curve/curve.go", pattern) for pattern in patterns))

    def test_diagnostics_keep_categories_without_raw_content(self):
        for raw, category in (("lookup secret-host: no such host", "dns-unavailable"),
                              ("unknown flag: credential-example", "unsupported-cli-flag"),
                              ("unknown long flag '--jetstream'", "unsupported-cli-flag"),
                              ('{"fixtureError":"fixture-publish-ack-refused"}', "fixture-publish-ack-refused"),
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

    def record_fixture(self):
        fact = {"subject": "market.crypto.quote", "frame": base64.b64encode(b"canonical-fixture").decode(),
                "headers": {"Nats-Msg-Id": "fixture", "X-Kanz-Fixture": "nats-gitops"}}
        record = {"subject": fact["subject"], "seq": 1, "data": fact["frame"], "time": "2026-11-15T00:00:00Z",
                  "hdrs": base64.b64encode(b"NATS/1.0\r\nNats-Msg-Id:fixture\r\nX-Kanz-Fixture:nats-gitops\r\n\r\n").decode()}
        return fact, record

    def test_exact_record_payload_headers_and_sequence(self):
        fact, record = self.record_fixture()
        self.assertEqual(proof.checked_record(record, fact), record)
        for key, value in (("seq", 2), ("subject", "order.order.submit"), ("data", "b3RoZXI="), ("hdrs", "")):
            with self.assertRaises(ValueError):
                proof.checked_record(record | {key: value}, fact)

    def test_cold_inventory_waits_for_missing_and_transient_responses(self):
        expected = self.canonical()
        complete = {name: {"config": value} for name, value in expected.items()}
        missing = {name: row for name, row in complete.items() if name != "RISK"}
        fixture = object.__new__(proof.Fixture)
        fact, record = self.record_fixture()
        observations, clock = {}, [10.0]
        responses = iter([missing, None, complete])
        def inventory(deadline, **kwargs):
            self.assertEqual(deadline, 10 + proof.RECOVERY_SECONDS)
            value = next(responses)
            if value is None:
                fixture.last_failure = {"category": "dependency-timeout"}
                raise RuntimeError("raw diagnostic must not be retained")
            return value
        with mock.patch.object(fixture, "inventory", side_effect=inventory), \
                mock.patch.object(fixture, "nats", return_value=json.dumps(record)) as read, \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]), \
                mock.patch.object(proof.time, "sleep", side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)):
            self.assertEqual(fixture.await_recovery(expected, fact, record, observations), record)
        read.assert_called_once()
        self.assertLessEqual(read.call_args.kwargs["timeout"], 15)
        self.assertEqual(observations["attempts"], 3)
        self.assertEqual(observations["elapsedSeconds"], 4)
        self.assertEqual(observations["observedNames"], sorted(expected))
        self.assertEqual(observations["missingNames"], [])
        self.assertEqual(observations["firstInventory"]["missingNames"], ["RISK"])
        self.assertTrue(observations["passed"])
        self.assertNotIn("raw diagnostic", json.dumps(observations))

    def test_cold_inventory_deadline_refuses_missing_streams_or_unavailable_reads(self):
        expected = self.canonical()
        complete = {name: {"config": value} for name, value in expected.items()}
        fact, record = self.record_fixture()
        for condition in ("missing", "dependency-timeout", "listener-unavailable", "command-timeout", "fact-unavailable"):
            with self.subTest(condition=condition):
                fixture = object.__new__(proof.Fixture)
                observations, clock = {}, [0.0]
                def unavailable(*args, **kwargs):
                    fixture.last_failure = {"category": "dependency-timeout" if condition == "fact-unavailable" else condition}
                    raise RuntimeError("raw broker detail")
                def inventory(**kwargs):
                    if condition == "missing":
                        return {name: row for name, row in complete.items() if name != "RISK"}
                    return complete if condition == "fact-unavailable" else unavailable()
                with mock.patch.object(fixture, "inventory", side_effect=inventory), \
                        mock.patch.object(fixture, "nats", side_effect=unavailable), \
                        mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]), \
                        mock.patch.object(proof.time, "sleep", side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)):
                    with self.assertRaises(TimeoutError):
                        fixture.await_recovery(expected, fact, record, observations)
                self.assertEqual(clock[0], proof.RECOVERY_SECONDS)
                self.assertEqual(observations["elapsedSeconds"], proof.RECOVERY_SECONDS)
                self.assertEqual(observations["state"], "deadline-expired")
                self.assertTrue(observations["deadlineExpired"])
                self.assertFalse(observations["passed"])
                self.assertEqual(observations["missingNames"], ["RISK"] if condition == "missing" else [] if condition == "fact-unavailable" else None)
                self.assertNotIn("raw broker", json.dumps(observations))

    def test_cold_inventory_refuses_configuration_fact_and_auth_drift_without_retry(self):
        expected = self.canonical()
        fact, record = self.record_fixture()
        for mutation in ("extra", "storage", "missing-field", "fact", "timestamp", "auth"):
            with self.subTest(mutation=mutation):
                actual = {name: {"config": copy.deepcopy(value)} for name, value in expected.items()}
                fixture = object.__new__(proof.Fixture)
                observations = {}
                if mutation == "extra":
                    actual["UNTRACKED"] = actual["RISK"]
                elif mutation == "storage":
                    actual["RISK"]["config"]["storage"] = "memory"
                elif mutation == "missing-field":
                    del actual["RISK"]["config"]["num_replicas"]
                def inventory(**kwargs):
                    if mutation == "auth":
                        fixture.last_failure = {"category": "broker-grant-refused"}
                        raise RuntimeError("private auth diagnostic")
                    return actual
                changed = record | ({"data": "b3RoZXI="} if mutation == "fact" else {"time": "changed"})
                with mock.patch.object(fixture, "inventory", side_effect=inventory) as probe, \
                        mock.patch.object(fixture, "nats", return_value=json.dumps(changed)), \
                        mock.patch.object(proof.time, "sleep") as pause:
                    with self.assertRaises(RuntimeError if mutation == "auth" else ValueError):
                        fixture.await_recovery(expected, fact, record, observations)
                probe.assert_called_once()
                pause.assert_not_called()
                self.assertFalse(observations["passed"])
                if mutation in ("storage", "missing-field"):
                    self.assertEqual(observations["state"], "configuration-refused")
                    self.assertEqual(observations["mismatchedFields"], {"RISK": ["storage" if mutation == "storage" else "num_replicas"]})

    def test_inventory_commands_share_one_recovery_deadline(self):
        fixture = object.__new__(proof.Fixture)
        expected, clock, budgets = self.canonical(), [0.0], []
        def read(*args, timeout):
            budgets.append(timeout)
            clock[0] += 0.2
            return json.dumps(sorted(expected) if args[1] == "ls" else {"config": expected[args[2]]})
        with mock.patch.object(fixture, "nats", side_effect=read), \
                mock.patch.object(proof.time, "monotonic", side_effect=lambda: clock[0]):
            with self.assertRaises(TimeoutError):
                fixture.inventory(deadline=0.5)
        self.assertEqual(len(budgets), 3)
        self.assertTrue(all(0 < budget <= 0.5 for budget in budgets))
        self.assertAlmostEqual(budgets[-1], 0.1)
        for invalid in (None, ["RISK", "RISK"], [{}], ["risky"], ["RISK" * 30]):
            with self.subTest(invalid=invalid), mock.patch.object(fixture, "nats", return_value=json.dumps(invalid)):
                with self.assertRaises(ValueError):
                    fixture.inventory()
        for mutation in ("extra", "storage"):
            with self.subTest(mutation=mutation):
                observations = {}
                first = sorted(expected)[0]
                names = sorted(expected) + (["UNTRACKED"] if mutation == "extra" else [])
                changed = {"config": expected[first] | {"storage": "memory"}}
                with mock.patch.object(fixture, "nats", side_effect=[json.dumps(names), json.dumps(changed)]) as probe:
                    with self.assertRaises(ValueError):
                        fixture.inventory(expected=expected, observation=observations)
                self.assertEqual(probe.call_count, 1 if mutation == "extra" else 2)
                self.assertEqual(observations["state"], "configuration-refused")
                self.assertEqual(observations["unexpectedNames"], ["UNTRACKED"] if mutation == "extra" else [])

    def receipt(self):
        operation = {"revision": SHA, "phase": "Succeeded", "hookPhase": "Succeeded", "syncPhase": "PostSync"}
        before = {f"nats-{i}": {"podUID": f"pod-{i}", "pvc": f"data-nats-{i}", "pvcUID": f"pvc-{i}", "pv": f"pv-{i}", "pvUID": f"pv-uid-{i}"} for i in range(3)}
        stopped = {"replicas": 0, "pods": [], "claims": {row["pvc"]: {key: value for key, value in row.items() if key != "podUID"} for row in before.values()}}
        after = {name: row | {"podUID": row["podUID"] + "-new"} for name, row in before.items()}
        return {"source": SHA, "tree": "b" * 40, "passed": True, "cleanupPassed": True, "cases": proof.CASES[:],
                "syncs": [operation | {"jobUID": "job-one", "operation": "first"}, operation | {"jobUID": "job-two", "operation": "second"}],
                "coldRecovery": {"before": before, "stopped": stopped, "after": after},
                "streams": self.canonical(),
                "postColdRecovery": {"passed": True, "state": "recovered", "budgetSeconds": proof.RECOVERY_SECONDS,
                    "elapsedSeconds": 2, "deadlineExpired": False, "attempts": 2, "observedNames": sorted(self.canonical()),
                    "missingNames": [], "unexpectedNames": [], "mismatchedFields": {}},
                "pubAck": {"source": SHA, "stream": "MARKET", "sequence": 1, "duplicate": False, "acknowledged": True}, "factBefore": {"record": "one"},
                "factAfter": {"record": "one"}, "canonicalValidated": True, "negativeControlPassed": True}

    def test_actual_puback_cannot_be_substituted_by_a_publish_exit(self):
        ack = self.receipt()["pubAck"]
        proof.check_puback(ack, SHA)
        for key, value in (("acknowledged", False), ("stream", "EXECUTION"), ("sequence", True),
                           ("sequence", 2), ("duplicate", True), ("source", "b" * 40)):
            with self.assertRaises(ValueError):
                proof.check_puback(ack | {key: value}, SHA)

    def test_cold_recovery_rejects_a_live_replica_or_replaced_claim(self):
        proof.check_cold_recovery(self.receipt()["coldRecovery"])
        for mutation in ("surviving-process", "observed-live-pod", "replicas-not-zero", "missing-claim", "new-pvc", "new-pv", "missing-replica"):
            with self.subTest(mutation=mutation):
                recovery = self.receipt()["coldRecovery"]
                if mutation == "surviving-process":
                    recovery["after"]["nats-1"]["podUID"] = recovery["before"]["nats-1"]["podUID"]
                elif mutation == "observed-live-pod":
                    recovery["stopped"]["pods"] = ["nats-2"]
                elif mutation == "replicas-not-zero":
                    recovery["stopped"]["replicas"] = 1
                elif mutation == "missing-claim":
                    del recovery["stopped"]["claims"]["data-nats-0"]
                elif mutation in ("new-pvc", "new-pv"):
                    recovery["after"]["nats-0"]["pvcUID" if mutation == "new-pvc" else "pvUID"] = "replacement"
                else:
                    del recovery["before"]["nats-2"]
                with self.assertRaises(ValueError):
                    proof.check_cold_recovery(recovery)

    def test_receipt_refuses_partial_skipped_or_reused_proof(self):
        proof.check_report(self.receipt())
        for key, value in (("cases", proof.CASES[:-1]), ("cleanupPassed", False), ("canonicalValidated", False),
                           ("negativeControlPassed", False), ("passed", False), ("factAfter", {"record": "other"})):
            with self.assertRaises(ValueError):
                proof.check_report(self.receipt() | {key: value})
        for group, key in (("syncs", "jobUID"), ("syncs", "operation")):
            broken = self.receipt()
            broken[group][1][key] = broken[group][0][key]
            with self.assertRaises(ValueError):
                proof.check_report(broken)
        broken = self.receipt()
        broken["coldRecovery"]["after"]["nats-0"]["pvcUID"] = "replacement-volume"
        with self.assertRaises(ValueError):
            proof.check_report(broken)
        for key, value in (("passed", False), ("state", "deadline-expired"), ("budgetSeconds", 999),
                           ("deadlineExpired", True), ("attempts", 0), ("elapsedSeconds", proof.RECOVERY_SECONDS + 1),
                           ("observedNames", []), ("missingNames", ["RISK"]), ("unexpectedNames", ["OTHER"]),
                           ("mismatchedFields", {"RISK": ["storage"]})):
            broken = self.receipt()
            broken["postColdRecovery"][key] = value
            with self.assertRaises(ValueError):
                proof.check_report(broken)


if __name__ == "__main__":
    unittest.main()

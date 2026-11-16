import copy
import json
import os
from pathlib import Path
import sys
import tempfile
import ast
import unittest
from unittest.mock import patch

import recovery_receipts as r
import recovery_plan as p
import transaction as tx
from test_transaction import policy


def fixture():
    before = policy(tx.NAMES[-1])
    before["metadata"].update(uid="11111111-1111-1111-1111-111111111111", resourceVersion="10",
        annotations={r.ANNOTATION: '{"old":"configuration"}', "custom": "preserved"})
    intended = copy.deepcopy(before)
    nonce = "a" * 32
    intended["metadata"]["annotations"][tx.TOKEN] = nonce
    intended["spec"]["ingress"][0]["ports"][0]["port"] = 81
    actual = copy.deepcopy(intended)
    serialized = copy.deepcopy(intended)
    serialized["metadata"]["annotations"].pop(r.ANNOTATION)
    actual["metadata"]["annotations"][r.ANNOTATION] = json.dumps(serialized)
    actual["metadata"]["resourceVersion"] = "11"
    entry = {"namespace": tx.NAMESPACE, "name": tx.NAMES[-1], "before": before, "intended": intended, "observed": None}
    base = tx.inventory([before])
    transaction = {"format": "kanz-gateway-policy-transaction-v1", "nonce": nonce,
        "baseline": [before], "desiredSHA256": "b" * 64, "writes": [], "rollback": "not_needed"}
    phases = []
    for i, phase in enumerate(("prepared", "dry_run_passed", "write_intent", "rollback", "rollback_refused")):
        value = copy.deepcopy(transaction)
        value.update(phase=phase, writes=[] if i < 2 else [copy.deepcopy(entry)],
                     rollback="not_needed" if i < 3 else ("pending" if i == 3 else "unresolved"))
        phases.append(value)
    probe = {"format": "kanz-gateway-policy-probes-v1", "nonce": nonce,
        "namespace": "gateway-policy-" + nonce, "namespaceUID": None,
        "phase": "namespace_create_intent", "pods": []}
    records = [*phases[:2], copy.deepcopy(probe)]
    probe.update(namespaceUID="22222222-2222-2222-2222-222222222222", phase="namespace_create_observed")
    records.append(copy.deepcopy(probe))
    def record(phase):
        probe["phase"] = phase
        records.append(copy.deepcopy(probe))
    for i in range(5):
        pod = {"namespace": tx.NAMESPACE, "name": "probe-" + str(i),
               **{key: None for key in ("uid", "address", "addresses", "addressState", "addressUncertain", "deleted")}}
        probe["pods"].append(pod)
        record("pod_create_intent")
        pod.update(uid=str(i + 3) * 8 + "-3333-3333-3333-333333333333", addresses=[], addressState="unknown")
        record("pod_address_observed")
        record("pod_create_observed")
        pod.update(addresses=["10.0.0." + str(i + 1)], addressState="observed")
        record("pod_address_observed")
        pod["address"] = "10.0.0." + str(i + 1)
        record("pod_running_unready_observed")
    records.extend(phases[2:])
    record("probe_cleanup_intent")
    for pod in reversed(probe["pods"]):
        pod["deleted"] = True
        record("pod_absence_observed")
    record("probe_cleanup_verified")
    return base, entry, nonce, tx.inventory([actual]), records


class RecoveryTests(unittest.TestCase):
    def test_inventory_transport_refuses_before_process_start(self):
        refused = [(), ("get", "secrets", "-A", "-o", "json"),
            ("get", "--raw", "/api/v1/secrets"), ("proxy",),
            ("get", "pods", "-A", "-o", "json", "--watch"),
            ("get", "pods", "-o", "json"), ("-n", "default", "get", "pods", "-o", "json"),
            ("get", "pods", "-A", "-o", "json", "--context", "other"),
            ("replace", "--raw", "/apis/networking.k8s.io/v1/namespaces/kanz-services/networkpolicies/allow-gateway-runtime-egress", "-f", "-")]
        with patch.object(p, "bounded_run", return_value="{}") as run:
            for argv in refused:
                with self.subTest(argv=argv), self.assertRaises(tx.Refusal):
                    p.inventory_command(argv, None, {}, "/private")
            with self.assertRaises(tx.Refusal):
                p.inventory_command(("get", "pods", "-A", "-o", "json"), {}, {}, "/private")
            run.assert_not_called()
            p.inventory_command(("get", "pods", "-A", "-o", "json"), None, {}, "/private")
            run.assert_called_once()

    def test_exact_probe_phase_schema_and_namespace_uid_custody(self):
        _, _, _, _, valid = fixture()
        self.assertEqual(len(valid), 39)
        r.retained(valid)
        for mode in ("foreign_format", "foreign_phase", "earlier_intent", "tx_field", "probe_field", "pod_field", "namespace_uid_drop"):
            records = copy.deepcopy(valid)
            with self.subTest(mode=mode), self.assertRaises(tx.Refusal):
                if mode == "foreign_format": records[4]["format"] = "other"
                if mode == "foreign_phase": records[4]["phase"] = "invented"
                if mode == "earlier_intent": records[4] = copy.deepcopy(records[29])
                if mode == "tx_field": records[0]["verified"] = True
                if mode == "probe_field": records[3]["verified"] = True
                if mode == "pod_field": records[4]["pods"][0]["verified"] = True
                if mode == "namespace_uid_drop": records[4]["namespaceUID"] = None
                r.retained(records)

    def test_plan_entrypoint_and_wrapper_cannot_restore(self):
        here = Path(__file__).parent
        source = (here / "recovery_plan.py").read_text()
        tree = ast.parse(source)
        calls = [node.func for node in ast.walk(tree) if isinstance(node, ast.Call)]
        self.assertFalse(any(isinstance(node, ast.Attribute) and node.attr in
            ("apply", "rollback", "replace", "create", "delete") for node in calls))
        self.assertIn("client.dry_run(proposed, True)", source)
        self.assertIn("?dryRun=All", source)
        wrapper = (here.parent / "Invoke-TokyoGatewayPolicyRecoveryPlan.ps1").read_text()
        self.assertIn("param()", wrapper)
        self.assertNotIn("--apply", wrapper)
        self.assertIn("recovery_host $repo", wrapper)
        self.assertIn("recovery_plan --tokyo-plan --source $source", wrapper)
        self.assertLess(wrapper.index("recovery_host $repo"), wrapper.index("ssm send-command"))
        for marker in ("user/kanz-operator", "KanzPlatformProvisioner/kanz-platform-cli", "-I -B -c $sourceLoader $proofRoot preflight",
                       "$encoded.Length -gt 20000", "($bytes.Length + $fixedSize) -gt 524288", "$parameterBytes -gt 30000", "len(data)>65536"):
            self.assertIn(marker, wrapper)
        self.assertNotIn("rollout_engine.py", wrapper)

    @unittest.skipUnless(sys.platform == "linux" and hasattr(os, "geteuid") and os.geteuid() == 0,
                         "actual root Linux nofollow receipt reader required")
    def test_linux_receipt_reader_rejects_links_fifo_and_inventory_changes(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(r, "DIRECTORY", directory):
            root = Path(directory)
            for i in range(1, 40):
                path = root / ("receipt-%04d.json" % i)
                path.write_bytes(b'{"bounded":true}\n')
                path.chmod(0o600)
            values, digest = r.read_receipts()
            self.assertEqual(len(values), 39)
            self.assertEqual(len(digest), 64)
            target = root / "receipt-0039.json"
            target.unlink(); target.symlink_to(root / "receipt-0001.json")
            with self.assertRaises(OSError): r.read_receipts()
            target.unlink(); os.link(root / "receipt-0001.json", target)
            with self.assertRaises(tx.Refusal): r.read_receipts()
            target.unlink(); os.mkfifo(target, 0o600)
            with self.assertRaises(tx.Refusal): r.read_receipts()
            target.unlink(); target.mkdir(mode=0o700)
            with self.assertRaises(tx.Refusal): r.read_receipts()

    def test_exact_retained_annotation_transform_only(self):
        base, entry, nonce, current, records = fixture()
        self.assertEqual(r.retained(records)[:3], (base, entry, nonce))
        self.assertEqual(r.target_matches(current, base, entry, nonce), current[r.TARGET])
        for mode in ("uid", "metadata", "spec", "annotation_uid", "annotation_rv", "annotation_nonce", "missing", "other_rv", "creation_timestamp", "other_server_metadata"):
            b, e, n, c, _ = fixture()
            with self.subTest(mode=mode), self.assertRaises(tx.Refusal):
                target = c[r.TARGET]
                if mode == "uid": target["metadata"]["uid"] = "99999999-9999-9999-9999-999999999999"
                if mode == "creation_timestamp": target["metadata"]["creationTimestamp"] = "1900-01-01T00:00:00Z"
                if mode == "metadata": target["metadata"]["annotations"]["custom"] = "changed"
                if mode == "spec": target["spec"]["ingress"] = []
                if mode.startswith("annotation_"):
                    value = json.loads(target["metadata"]["annotations"][r.ANNOTATION])
                    field = mode.removeprefix("annotation_")
                    if field == "nonce": value["metadata"]["annotations"][tx.TOKEN] = "c" * 32
                    else: value["metadata"]["resourceVersion" if field == "rv" else "uid"] = "changed"
                    target["metadata"]["annotations"][r.ANNOTATION] = json.dumps(value)
                if mode == "missing": c.clear()
                if mode in ("other_rv", "other_server_metadata"):
                    extra = policy(tx.NAMES[0]); extra["metadata"].update(uid="44444444-4444-4444-4444-444444444444", resourceVersion="2")
                    b.update(tx.inventory([extra]))
                    if mode == "other_rv": extra["metadata"]["resourceVersion"] = "3"
                    else: extra["metadata"]["managedFields"] = [{"manager": "changed"}]
                    c.update(tx.inventory([extra]))
                r.target_matches(c, b, e, n)

    def test_unresolved_receipt_history_is_closed(self):
        for mode in ("observed", "second_write", "baseline", "nonce", "verified", "missing", "phase"):
            _, _, _, _, records = fixture()
            with self.subTest(mode=mode), self.assertRaises(tx.Refusal):
                if mode == "observed": records[31]["writes"][0]["observed"] = {}
                if mode == "second_write": records[31]["writes"].append(records[31]["writes"][0])
                if mode == "baseline": records[1]["baseline"] = []
                if mode == "nonce": records[4]["nonce"] = "b" * 32
                if mode == "verified": records[31]["rollback"] = "verified"
                if mode == "missing": records.pop()
                if mode == "phase": records[29]["phase"] = "write_observed"
                r.retained(records)

    def test_plan_only_dry_runs_and_rejects_inventory_change(self):
        for mutate in (False, True):
            base, entry, nonce, current, records = fixture()
            desired = [copy.deepcopy(entry["intended"])]
            records[0]["desiredSHA256"] = tx.fingerprint(desired)
            for record in records:
                if record.get("format") == "kanz-gateway-policy-transaction-v1":
                    record["desiredSHA256"] = records[0]["desiredSHA256"]
            payload = {"source": "f" * 40, "policies": desired, "workloads": {}}
            payload["provenance"] = {"command": r.COMMAND, "instance": "i-0ee1235614219f089",
                "oldSource": r.OLD_SOURCE, "directory": r.DIRECTORY,
                "originalPoliciesSHA256": tx.fingerprint(desired), "originalWorkloadsSHA256": tx.fingerprint({}),
                **{key: "d" * 64 for key in ("commandSHA256", "sourceSHA256", "bundleSHA256")}}
            calls = []
            def k(*argv, value=None):
                calls.append(argv)
                if argv == ("get", "namespace", "kube-system", "-o", "json"):
                    return json.dumps({"metadata": {"uid": p.scope.CLUSTER_UID}})
                if argv == ("-n", tx.NAMESPACE, "get", "networkpolicies", "-o", "json"):
                    return json.dumps({"items": list(current.values())})
                if argv[0] == "get": return '{"items":[]}'
                self.assertEqual(argv, ("replace", "--raw", "/apis/networking.k8s.io/v1/namespaces/kanz-services/networkpolicies/allow-gateway-runtime-egress?dryRun=All", "-f", "-"))
                self.assertEqual(tx.semantic(value), tx.semantic(base[r.TARGET]))
                self.assertEqual(value["metadata"]["resourceVersion"], "11")
                if mutate: current[r.TARGET]["metadata"]["resourceVersion"] = "12"
                return json.dumps(value)
            # Existing rendered-baseline/lineage implementations have their own
            # real fixture; isolate this planner's no-write and stable-CAS seam.
            with patch.object(p.tx, "qualify_rendered_baseline"), patch.object(p.scope, "snapshot", return_value={}), patch.object(p.scope, "service_snapshot", return_value={}), patch.object(p.scope, "outside_policies", return_value={}):
                if mutate:
                    with self.assertRaises(tx.Refusal): p.plan(k, payload, records, "e" * 64)
                else:
                    result = p.plan(k, payload, records, "e" * 64)
                    self.assertEqual(result["legacyHistoricalSnapshots"], "UNKNOWN")
                    self.assertFalse(result["restoreAuthorized"])
                    self.assertEqual(result["fullCurrentTargetSHA256"], tx.fingerprint(current[r.TARGET]))
                    self.assertEqual(result["persistentPolicyWrites"], 0)
            self.assertEqual(sum(call[0] == "replace" for call in calls), 1)
            self.assertFalse(any(call[0] in ("create", "delete", "apply") for call in calls))

    def test_strict_json_and_platform_refusal(self):
        for raw in (b'{"x":1,"x":2}', b'{"x":NaN}', b'[]', b'{}x', b'\xff', b'x' * (2 * 1024 * 1024 + 1)):
            with self.assertRaises(tx.Refusal): r.document(raw)
        with patch.object(r.sys, "platform", "win32"), self.assertRaises(tx.Refusal): r.read_receipts()

    def test_custody_retains_all_observed_ips_and_refuses_unknown(self):
        _, _, nonce, _, records = fixture()
        probes = r.retained(records)[3]
        probes[2]["pods"][0]["addresses"] = ["2001:db8::1"]
        self.assertIn("2001:db8::1", p.cleanup_custody(probes, nonce)[3])
        for mode in ("malformed_history", "unknown_final", "uid_swap", "bad_ip", "not_deleted"):
            _, _, n, _, rs = fixture(); ps = r.retained(rs)[3]
            with self.subTest(mode=mode), self.assertRaises(tx.Refusal):
                if mode == "malformed_history": ps[2]["pods"][0]["addressState"] = "malformed"
                if mode == "unknown_final": ps[-1]["pods"][0]["addressState"] = "unknown"
                if mode == "uid_swap": ps[-1]["pods"][0]["uid"] = "99999999-9999-9999-9999-999999999999"
                if mode == "bad_ip": ps[2]["pods"][0]["addresses"] = ["not-an-address"]
                if mode == "not_deleted": ps[-1]["pods"][0]["deleted"] = False
                p.cleanup_custody(ps, n)


if __name__ == "__main__":
    unittest.main()

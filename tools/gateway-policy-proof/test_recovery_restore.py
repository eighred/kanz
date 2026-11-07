import copy
import json
import unittest
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

import recovery_restore as restore
import recovery_receipts as receipts
import policy_state as state
import restore_fixture
from test_recovery_plan import fixture


class RestoreTests(unittest.TestCase):
    @unittest.skipUnless(sys.platform == "linux", "requires real Linux directory and fsync semantics")
    def test_actual_exclusive_append_only_journal(self):
        with tempfile.TemporaryDirectory() as directory:
            path = str(Path(directory) / "owned")
            journal = restore.create_journal(path)
            try:
                journal({"phase": "intent", "synthetic": True})
                with self.assertRaises(FileExistsError): restore.create_journal(path)
                self.assertEqual(json.loads((Path(path) / "receipt-0001.json").read_bytes()),
                                 {"phase": "intent", "synthetic": True})
            finally:
                journal.close()

    def test_synthetic_legacy_schema_and_closed_evidence(self):
        baseline, entry, nonce, _, records = fixture()
        probes = [r for r in records if r["format"] == "kanz-gateway-policy-probes-v1"]
        built = restore_fixture.legacy_records(baseline, entry["intended"], nonce, [entry["intended"]], probes)
        self.assertEqual(len(built), 39)
        receipts.retained(built)
        valid = {"cases": list(restore_fixture.CASES), "syntheticLegacyReceipts": True,
                 "historicalProvenanceQualified": False, "actualAppendOnlyJournals": True,
                 "singleAttemptEnforced": True}
        restore_fixture.validate(valid)
        for key, value in (("cases", valid["cases"][:-1]), ("cases", list(reversed(valid["cases"]))),
                           ("syntheticLegacyReceipts", False), ("historicalProvenanceQualified", True),
                           ("actualAppendOnlyJournals", False), ("singleAttemptEnforced", 1)):
            with self.subTest(key=key), self.assertRaises(ValueError):
                restore_fixture.validate({**valid, key: value})

    def test_read_only_transport_and_single_use_write_capability(self):
        _, entry, _, _, _ = fixture()
        with patch.object(restore.planner, "bounded_run") as run:
            with self.assertRaises(state.Refusal):
                restore.planner.inventory_command(restore.PUT, entry["intended"], {}, "/private")
            run.assert_not_called()
        for lost in (False, True):
            with self.subTest(lost=lost), patch.object(restore, "bounded_run", return_value="{}",
                    side_effect=OSError("synthetic response loss") if lost else None) as run:
                put = restore.PutOnce({}, "/private")
                if lost:
                    with self.assertRaises(OSError): put(entry["intended"])
                else:
                    self.assertEqual(put(entry["intended"]), "{}")
                with self.assertRaises(state.Refusal): put(entry["intended"])
                self.assertEqual(run.call_count, 1)
                self.assertEqual(run.call_args.args[0], ["/usr/local/bin/k3s", "kubectl", *restore.PUT])
        with patch.object(restore, "bounded_run") as run:
            wrong = copy.deepcopy(entry["intended"])
            wrong["metadata"]["namespace"] = "default"
            with self.assertRaises(state.Refusal): restore.PutOnce({}, "/private")(wrong)
            run.assert_not_called()

    def execute(self, mode):
        baseline, entry, _, current, records = fixture()
        desired = [copy.deepcopy(entry["intended"])]
        for record in records:
            if record["format"] == "kanz-gateway-policy-transaction-v1":
                record["desiredSHA256"] = state.fingerprint(desired)
        payload = {"source": "f" * 40, "policies": desired, "workloads": {}}
        payload["provenance"] = {"command": receipts.COMMAND, "instance": "i-0ee1235614219f089",
            "oldSource": receipts.OLD_SOURCE, "directory": receipts.DIRECTORY,
            "originalPoliciesSHA256": state.fingerprint(desired), "originalWorkloadsSHA256": state.fingerprint({}),
            **{key: "d" * 64 for key in ("commandSHA256", "sourceSHA256", "bundleSHA256")}}
        journal, writes = [], []
        receipt_hash, scope_state = ["e" * 64], [{}]
        def read():
            return copy.deepcopy(records), receipt_hash[0]
        def save(value):
            journal.append(copy.deepcopy(value))
            if value["phase"] == "restore_acknowledged" and mode == "ack_fsync":
                raise OSError("synthetic acknowledgment fsync failure")
            if value["phase"] == "restore_intent":
                if mode == "target_drift": current[receipts.TARGET]["metadata"]["resourceVersion"] = "19"
                if mode == "receipt_drift": receipt_hash[0] = "0" * 64
                if mode == "scope_drift": scope_state[0] = {"changed": True}
                if mode == "intent_fsync": raise OSError("synthetic journal failure")
        def k(*argv, value=None):
            if argv == ("get", "namespace", "kube-system", "-o", "json"):
                return json.dumps({"metadata": {"uid": restore.scope.CLUSTER_UID}})
            if argv == ("-n", state.NAMESPACE, "get", "networkpolicies", "-o", "json"):
                return json.dumps({"items": list(current.values())})
            if argv[0] == "get": return '{"items":[]}'
            if argv != restore.PUT:
                self.assertEqual(argv[2], restore.PUT[2] + "?dryRun=All")
                return json.dumps(value)
            writes.append(copy.deepcopy(value))
            self.assertEqual(journal[-1]["phase"], "restore_intent")
            self.assertEqual(value["metadata"]["resourceVersion"], "11")
            if mode == "stale_cas": raise OSError("synthetic server CAS rejection")
            actual = copy.deepcopy(value)
            actual["metadata"]["resourceVersion"] = "12"
            current[receipts.TARGET] = actual
            if mode == "response_loss": raise OSError("synthetic response loss after real model mutation")
            response = copy.deepcopy(actual)
            if mode == "same_rv": response["metadata"]["resourceVersion"] = "11"
            if mode == "wrong_uid": response["metadata"]["uid"] = "9" * 36
            if mode == "annotation": response["metadata"]["annotations"]["custom"] = "changed"
            if mode == "readback": current[receipts.TARGET]["metadata"]["resourceVersion"] = "13"
            if mode == "receipt_after": receipt_hash[0] = "1" * 64
            if mode == "scope_after": scope_state[0] = {"changed": True}
            return json.dumps(response)
        with patch.object(state, "qualify_rendered_baseline"), \
                patch.object(restore.scope, "snapshot", side_effect=lambda *_: copy.deepcopy(scope_state[0])), \
                patch.object(restore.scope, "service_snapshot", return_value={}), \
                patch.object(restore.scope, "outside_policies", return_value={}):
            if mode == "success":
                result = restore.reconcile(k, lambda value: k(*restore.PUT, value=value), payload, read, save)
                self.assertEqual(result["result"], "VERIFIED")
                self.assertEqual(result["legacyHistoricalSnapshots"], "UNKNOWN")
                self.assertEqual(state.semantic(current[receipts.TARGET]), state.semantic(baseline[receipts.TARGET]))
                self.assertEqual([x["phase"] for x in journal], ["observation_recorded", "restore_intent", "restore_acknowledged", "verified"])
            else:
                with self.assertRaises(state.Refusal): restore.reconcile(k, lambda value: k(*restore.PUT, value=value), payload, read, save)
                prewrite = mode in ("target_drift", "receipt_drift", "scope_drift", "intent_fsync")
                self.assertEqual(journal[-1]["phase"], "refused" if prewrite else "unknown")
                self.assertFalse(any(x["phase"] == "verified" for x in journal))
                self.assertEqual(len(writes), 0 if prewrite else 1)
                if mode in ("readback", "receipt_after", "scope_after", "ack_fsync"):
                    self.assertIn("response", journal[-1])
                if mode == "response_loss":
                    self.assertEqual(state.semantic(current[receipts.TARGET]), state.semantic(baseline[receipts.TARGET]))
        self.assertLessEqual(len(writes), 1)

    def test_single_write_and_all_ambiguities_refuse_without_retry(self):
        for mode in ("success", "target_drift", "receipt_drift", "scope_drift", "intent_fsync",
                     "stale_cas", "response_loss", "same_rv", "wrong_uid", "annotation", "readback",
                     "receipt_after", "scope_after", "ack_fsync"):
            with self.subTest(mode=mode): self.execute(mode)


if __name__ == "__main__":
    unittest.main()

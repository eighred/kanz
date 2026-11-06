import copy
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
import uuid

import transaction as tx


def policy(name, port=80):
    return {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
            "metadata": {"name": name, "namespace": tx.NAMESPACE},
            "spec": {"podSelector": {"matchLabels": {"app": name}},
                     "policyTypes": ["Ingress"], "ingress": [{"ports": [{"port": port}]}]}}


class API:
    def __init__(self, objects):
        self.objects = {}
        self.rv = 0
        self.calls = []
        self.fail_after = None
        self.fail_before = None
        self.changed = None
        self.scope_valid = True
        for obj in objects:
            self.create(obj)
        self.calls.clear()

    def check_scope(self):
        if not self.scope_valid:
            raise tx.Refusal("scope changed")

    def list_policies(self):
        return copy.deepcopy(list(self.objects.values()))

    def dry_run(self, obj, replacing):
        self.calls.append("dry_run")

    def write(self, obj, replacing):
        count = sum(x in ("create", "replace") for x in self.calls) + 1
        if self.fail_before == count:
            self.fail_before = None
            raise RuntimeError("synthetic failure before write")
        key = (obj["metadata"]["namespace"], obj["metadata"]["name"])
        old = self.objects.get(key)
        if replacing:
            if old is None or tx.version(old) != tx.version(obj):
                raise tx.Refusal("CAS conflict")
        elif old is not None:
            raise tx.Refusal("already exists")
        self.calls.append("replace" if replacing else "create")
        result = copy.deepcopy(obj)
        self.rv += 1
        result["metadata"].update(uid=old["metadata"]["uid"] if old else str(uuid.uuid4()), resourceVersion=str(self.rv))
        self.objects[key] = result
        if self.fail_after == count:
            self.fail_after = None
            raise RuntimeError("synthetic lost response")

    def create(self, obj):
        self.write(obj, False)

    def replace(self, obj):
        self.write(obj, True)

    def delete(self, key, uid, rv):
        if tx.version(self.objects[key]) != (uid, rv):
            raise tx.Refusal("delete CAS conflict")
        self.calls.append("delete")
        del self.objects[key]


class TransactionTests(unittest.TestCase):
    @unittest.skipUnless(sys.platform == "linux", "actual no-follow receipt ownership requires Linux")
    def test_durable_receipts_refuse_nonprivate_link_and_overwrite(self):
        with tempfile.TemporaryDirectory(prefix="kanz-policy-receipt-test-") as directory:
            root = Path(directory)
            private = root / "private"
            private.mkdir(mode=0o700)
            receipts = tx.Receipts(str(private))
            try:
                receipts({"phase": "before_write"})
                self.assertEqual(json.loads((private / "receipt-0001.json").read_text()), {"phase": "before_write"})
                self.assertEqual((private / "receipt-0001.json").stat().st_mode & 0o777, 0o600)
                (private / "receipt-0002.json").write_text("retain")
                with self.assertRaises(FileExistsError):
                    receipts({"phase": "must_not_overwrite"})
                self.assertEqual((private / "receipt-0002.json").read_text(), "retain")
            finally:
                receipts.close()
            link = root / "link"
            link.symlink_to(private, target_is_directory=True)
            with self.assertRaises(OSError):
                tx.Receipts(str(link))
            os.chmod(private, 0o755)
            with self.assertRaises(tx.Refusal):
                tx.Receipts(str(private))

    def setup_transaction(self, preexisting=False):
        desired = [policy(name, 9000 + i) for i, name in enumerate(tx.NAMES)]
        base = [policy("unrelated")]
        if preexisting:
            obj = policy(tx.NAMES[0], 1234)
            obj["metadata"].update(labels={"owner": "original"}, annotations={"note": "retain"},
                                   finalizers=["fixture.example/retain"])
            base.append(obj)
        api = API(base)
        receipts = []
        transaction = tx.Transaction(api, desired, "a" * 32, receipts.append)
        return api, transaction, receipts

    def test_dry_run_has_no_policy_writes(self):
        api, transaction, receipts = self.setup_transaction()
        before = api.list_policies()
        transaction.prepare(lambda baseline, desired: None)
        self.assertEqual(api.calls, ["dry_run"] * 4)
        self.assertEqual(before, api.list_policies())
        self.assertEqual(receipts[-1]["phase"], "dry_run_passed")

    def test_success_preserves_metadata_and_removes_markers(self):
        api, transaction, receipts = self.setup_transaction(True)
        transaction.prepare(lambda baseline, desired: None)
        transaction.apply(lambda: None)
        self.assertEqual(receipts[-1]["phase"], "verified")
        first = api.objects[(tx.NAMESPACE, tx.NAMES[0])]
        self.assertEqual(first["metadata"]["annotations"], {"note": "retain"})
        self.assertEqual(first["metadata"]["finalizers"], ["fixture.example/retain"])
        self.assertTrue(all(tx.TOKEN not in x["metadata"].get("annotations", {}) for x in api.objects.values()))

    def test_each_lost_response_restores_semantic_baseline(self):
        for count in range(1, 9):
            with self.subTest(count=count):
                api, transaction, receipts = self.setup_transaction(True)
                before = tx.inventory(api.list_policies())
                transaction.prepare(lambda baseline, desired: None)
                api.fail_after = count
                with self.assertRaises(RuntimeError):
                    transaction.apply(lambda: None)
                self.assertTrue(tx.same_inventory(before, tx.inventory(api.list_policies())))
                self.assertEqual(receipts[-1]["rollback"], "verified")

    def test_each_failed_write_restores_prior_writes(self):
        for count in range(1, 5):
            with self.subTest(count=count):
                api, transaction, receipts = self.setup_transaction(True)
                before = tx.inventory(api.list_policies())
                transaction.prepare(lambda baseline, desired: None)
                api.fail_before = count
                with self.assertRaises(RuntimeError):
                    transaction.apply(lambda: None)
                self.assertTrue(tx.same_inventory(before, tx.inventory(api.list_policies())))

    def test_failed_probe_rolls_back_all_four(self):
        api, transaction, receipts = self.setup_transaction(True)
        before = tx.inventory(api.list_policies())
        transaction.prepare(lambda baseline, desired: None)
        def fail():
            raise RuntimeError("probe failed")
        with self.assertRaises(RuntimeError):
            transaction.apply(fail)
        self.assertTrue(tx.same_inventory(before, tx.inventory(api.list_policies())))

    def test_concurrent_change_refuses_rollback_without_deleting(self):
        for change in ("foreign_uid", "resource_version", "unrelated_policy", "scope"):
            with self.subTest(change=change):
                api, transaction, receipts = self.setup_transaction()
                transaction.prepare(lambda baseline, desired: None)
                def conflict():
                    if change == "scope":
                        api.scope_valid = False
                    elif change == "unrelated_policy":
                        api.create(policy("foreign"))
                    else:
                        meta = api.objects[(tx.NAMESPACE, tx.NAMES[0])]["metadata"]
                        meta["uid" if change == "foreign_uid" else "resourceVersion"] = str(uuid.uuid4()) if change == "foreign_uid" else "999999"
                with self.assertRaisesRegex(tx.Refusal, "retain receipt"):
                    transaction.apply(conflict)
                self.assertEqual(receipts[-1]["rollback"], "unresolved")
                self.assertNotIn("delete", api.calls)

    def test_missing_dryrun_invalid_scope_and_unresolved_marker_refuse(self):
        api, transaction, _ = self.setup_transaction()
        with self.assertRaises(tx.Refusal):
            transaction.apply(lambda: None)
        api.scope_valid = False
        with self.assertRaises(tx.Refusal):
            transaction.prepare(lambda baseline, desired: None)
        api.scope_valid = True
        next(iter(api.objects.values()))["metadata"]["annotations"] = {tx.TOKEN: "b" * 32}
        with self.assertRaisesRegex(tx.Refusal, "unresolved transaction"):
            transaction.prepare(lambda baseline, desired: None)

    def test_receipt_failure_prevents_first_mutation(self):
        api, transaction, _ = self.setup_transaction()
        transaction.prepare(lambda baseline, desired: None)
        def refuse(_):
            raise OSError("fixture receipt storage unavailable")
        transaction.save = refuse
        with self.assertRaises(OSError):
            transaction.apply(lambda: None)
        self.assertEqual(api.calls, ["dry_run"] * 4)

    def test_normalization_does_not_remove_user_metadata(self):
        obj = policy("test")
        obj["metadata"]["annotations"] = {"example": "one"}
        altered = copy.deepcopy(obj)
        altered["metadata"]["annotations"]["example"] = "two"
        self.assertNotEqual(tx.semantic(obj), tx.semantic(altered))
        altered = copy.deepcopy(obj)
        altered["metadata"].update(uid=str(uuid.uuid4()), resourceVersion="10", generation=2)
        self.assertEqual(tx.semantic(obj), tx.semantic(altered))

    def test_rendered_union_only_allows_exact_two_egress_additions(self):
        rendered = [policy(name) for name in tx.NAMES] + [policy("unrelated")]
        rendered[3]["spec"] = {"podSelector": {"matchLabels": {"app": "api-gateway"}},
                               "policyTypes": ["Egress"], "egress": [
                                   {"to": [{"podSelector": {"matchLabels": {"app": app}}}],
                                    "ports": [{"protocol": "TCP", "port": port}]}
                                   for app, port in (("compliance", 8095), ("accounting", 8101), ("audit", 8102))]}
        desired = tx.inventory(rendered[:4])
        old = copy.deepcopy(rendered[3:])
        old[0]["spec"]["egress"] = old[0]["spec"]["egress"][:1]
        tx.qualify_rendered_baseline(tx.inventory(old), desired, rendered)
        tx.qualify_rendered_baseline(tx.inventory(rendered), desired, rendered)
        for mutation in ("extra", "missing", "broad", "partial", "unrelated"):
            changed = copy.deepcopy(old)
            if mutation == "extra":
                changed.append(policy("unknown"))
            elif mutation == "missing":
                changed.pop(0)
            elif mutation == "broad":
                changed[0]["spec"]["egress"].append({})
            elif mutation == "partial":
                changed[0]["spec"]["egress"].append(copy.deepcopy(rendered[3]["spec"]["egress"][1]))
            else:
                changed[1]["spec"]["podSelector"] = {}
            with self.subTest(mutation=mutation), self.assertRaises(tx.Refusal):
                tx.qualify_rendered_baseline(tx.inventory(changed), desired, rendered)


if __name__ == "__main__":
    unittest.main()

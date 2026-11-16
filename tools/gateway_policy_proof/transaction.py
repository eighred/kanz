"""Internal four-policy transaction; no context-selecting command-line entrypoint.

The live wrapper and disposable fixture provide independently verified scopes.
Receipts are written before side effects and retained when ownership is uncertain.
"""
import copy
import re

import policy_state as state

NAMESPACE = state.NAMESPACE
NAMES = state.NAMES
TOKEN = state.TOKEN
SERVER_METADATA = state.SERVER_METADATA
Refusal = state.Refusal
Receipts = state.Receipts
encoded = state.encoded
fingerprint = state.fingerprint
semantic = state.semantic
inventory = state.inventory
version = state.version
same_inventory = state.same_inventory
qualify_rendered_baseline = state.qualify_rendered_baseline


class Transaction:
    """Client must supply list/create/replace/delete/dry_run and scope check.

    replace uses the object's captured UID/resourceVersion; delete takes both
    preconditions. Client transport failures never establish write absence.
    """

    def __init__(self, client, desired, nonce, save_receipt):
        if not re.fullmatch(r"[0-9a-f]{32}", nonce):
            raise Refusal("invalid transaction nonce")
        self.client, self.nonce, self.save = client, nonce, save_receipt
        self.desired = inventory(desired)
        if set(self.desired) != {(NAMESPACE, name) for name in NAMES}:
            raise Refusal("transaction must contain exactly four named policies")
        for obj in self.desired.values():
            if TOKEN in obj["metadata"].get("annotations", {}):
                raise Refusal("desired policy contains transaction marker")
        self.baseline = None
        self.expected = None
        self.writes = []
        self.receipt = {"format": "kanz-gateway-policy-transaction-v1", "nonce": nonce,
                        "phase": "new", "writes": [], "rollback": "not_needed"}

    def record(self, phase):
        self.receipt["phase"] = phase
        self.save(copy.deepcopy(self.receipt))

    def current(self):
        self.client.check_scope()
        return inventory(self.client.list_policies())

    def assert_expected(self):
        current = self.current()
        if not same_inventory(current, self.expected, exact_version=True):
            raise Refusal("policy inventory changed concurrently")
        return current

    def prepare(self, qualify_baseline):
        if self.baseline is not None:
            raise Refusal("transaction already prepared")
        baseline = self.current()
        for obj in baseline.values():
            version(obj)
            if TOKEN in obj["metadata"].get("annotations", {}):
                raise Refusal("unresolved transaction marker exists")
        # The caller compares the exact rendered union and live workload identity.
        qualify_baseline(copy.deepcopy(baseline), copy.deepcopy(self.desired))
        self.baseline = baseline
        self.expected = copy.deepcopy(baseline)
        self.receipt["baseline"] = list(baseline.values())
        self.receipt["desiredSHA256"] = fingerprint(list(self.desired.values()))
        self.record("prepared")
        for key in self.desired:
            self.client.dry_run(self.write_object(key), key in baseline)
        self.assert_expected()
        self.record("dry_run_passed")

    def write_object(self, key):
        existing = self.expected.get(key)
        obj = copy.deepcopy(existing if existing is not None else self.desired[key])
        obj["spec"] = copy.deepcopy(self.desired[key]["spec"])
        meta = obj["metadata"]
        # Preserve existing user metadata, including annotations and ownership.
        meta.setdefault("annotations", {})[TOKEN] = self.nonce
        for field in SERVER_METADATA - {"uid", "resourceVersion"}:
            meta.pop(field, None)
        if existing is None:
            meta.pop("uid", None)
            meta.pop("resourceVersion", None)
        return obj

    def apply(self, verify):
        if self.receipt["phase"] != "dry_run_passed":
            raise Refusal("successful preparation required")
        try:
            for key in self.desired:
                self.assert_expected()
                obj = self.write_object(key)
                before = self.expected.get(key)
                entry = {"namespace": key[0], "name": key[1], "before": before,
                         "intended": obj, "observed": None}
                self.writes.append(entry)
                self.receipt["writes"].append(entry)
                self.record("write_intent")
                try:
                    if before is None:
                        self.client.create(obj)
                    else:
                        self.client.replace(obj)
                finally:
                    # Reconcile a lost response against the real API. Unknown
                    # outcomes are retained, never treated as successful absence.
                    current = self.current()
                    observed = current.get(key)
                    if observed is not None and semantic(observed) == semantic(obj):
                        version(observed)
                        if before is not None and version(observed)[0] != version(before)[0]:
                            raise Refusal("policy replaced by another owner")
                        entry["observed"] = observed
                        self.expected[key] = observed
                    elif observed != before:
                        raise Refusal("ambiguous policy write ownership")
                    self.record("write_observed")
                if entry["observed"] is None:
                    raise Refusal("policy write not observed")
            self.assert_expected()
            verify()
            self.assert_expected()
            # Remove our ownership annotations only after successful controls.
            # Every finalization is itself CAS-bound and reconciled on readback.
            for entry in self.writes:
                key = (entry["namespace"], entry["name"])
                self.assert_expected()
                obj = copy.deepcopy(self.expected[key])
                annotations = obj["metadata"].get("annotations", {})
                if annotations.pop(TOKEN, None) != self.nonce:
                    raise Refusal("transaction marker changed")
                if not annotations:
                    obj["metadata"].pop("annotations", None)
                self.record("finalize_intent")
                try:
                    self.client.replace(obj)
                finally:
                    actual = self.current().get(key)
                    if actual is not None and semantic(actual) == semantic(obj) and version(actual)[0] == version(obj)[0]:
                        entry["observed"] = actual
                        self.expected[key] = actual
                    self.record("finalize_observed")
                self.assert_expected()
            self.record("verified")
        except Exception:
            self.rollback()
            raise

    def rollback(self):
        self.receipt["rollback"] = "pending"
        self.record("rollback")
        try:
            self.assert_expected()
            for entry in reversed(self.writes):
                key = (entry["namespace"], entry["name"])
                observed = entry["observed"]
                if observed is None:
                    continue
                self.assert_expected()
                uid, rv = version(observed)
                before = entry["before"]
                if before is None:
                    self.client.delete(key, uid, rv)
                    self.expected.pop(key)
                else:
                    restored = copy.deepcopy(before)
                    restored["metadata"]["resourceVersion"] = rv
                    for field in SERVER_METADATA - {"uid", "resourceVersion"}:
                        restored["metadata"].pop(field, None)
                    self.client.replace(restored)
                    current = self.current()
                    actual = current.get(key)
                    if actual is None or version(actual)[0] != uid or semantic(actual) != semantic(before):
                        raise Refusal("policy restoration differs")
                    self.expected[key] = actual
                self.assert_expected()
            if not same_inventory(self.current(), self.baseline):
                raise Refusal("rollback baseline differs")
            self.receipt["rollback"] = "verified"
            self.record("rolled_back")
        except Exception:
            self.receipt["rollback"] = "unresolved"
            self.record("rollback_refused")
            raise Refusal("rollback ownership uncertain; retain receipt") from None

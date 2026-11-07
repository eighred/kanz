"""Internal four-policy transaction; no context-selecting command-line entrypoint.

The live wrapper and disposable fixture provide independently verified scopes.
Receipts are written before side effects and retained when ownership is uncertain.
"""
import copy
import hashlib
import json
import os
import re
import stat
import sys

NAMESPACE = "kanz-services"
NAMES = (
    "allow-gateway-to-accounting",
    "allow-gateway-to-audit",
    "allow-gateway-to-compliance-mandate",
    "allow-gateway-runtime-egress",
)
TOKEN = "kanz.eighred.com/gateway-policy-transaction"
SERVER_METADATA = {"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields"}


class Refusal(RuntimeError):
    pass


class Receipts:
    """Append-only private evidence, fsynced before each external side effect.

    The owner creates the private directory. This never deletes receipts; an
    unresolved transaction therefore survives wrapper/network interruption.
    """
    def __init__(self, directory):
        if sys.platform != "linux":
            raise Refusal("durable rollout receipts require Linux")
        self.fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
        info = os.fstat(self.fd)
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            os.close(self.fd)
            self.fd = -1
            raise Refusal("receipt directory is not private and owned")
        self.count = 0

    def __call__(self, value):
        if self.fd < 0 or self.count >= 64:
            raise Refusal("receipt lifetime bound exceeded")
        raw = encoded(value) + b"\n"
        self.count += 1
        fd = os.open("receipt-%04d.json" % self.count,
                     os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC,
                     0o600, dir_fd=self.fd)
        try:
            with os.fdopen(fd, "wb", closefd=False) as out:
                out.write(raw)
                out.flush()
                os.fsync(fd)
        finally:
            os.close(fd)
        os.fsync(self.fd)

    def close(self):
        if self.fd >= 0:
            os.close(self.fd)
            self.fd = -1


def encoded(value):
    raw = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    if len(raw) > 2 * 1024 * 1024:
        raise Refusal("policy inventory exceeds bound")
    return raw


def fingerprint(value):
    return hashlib.sha256(encoded(value)).hexdigest()


def semantic(obj):
    result = copy.deepcopy(obj)
    meta = result.get("metadata", {})
    for field in SERVER_METADATA:
        meta.pop(field, None)
    # Kubernetes omits these empty slices and defaults omitted port protocol.
    spec = result.get("spec", {})
    for direction in ("ingress", "egress"):
        if spec.get(direction) == []:
            spec.pop(direction)
        for rule in spec.get(direction, []):
            for port in rule.get("ports", []):
                port.setdefault("protocol", "TCP")
    return result


def inventory(items):
    if not isinstance(items, list) or len(items) > 256:
        raise Refusal("invalid policy inventory")
    out = {}
    for obj in items:
        if not isinstance(obj, dict) or set(obj) - {"apiVersion", "kind", "metadata", "spec"}:
            raise Refusal("invalid policy object")
        meta = obj.get("metadata", {})
        name, namespace = meta.get("name"), meta.get("namespace")
        if (obj.get("apiVersion") != "networking.k8s.io/v1" or obj.get("kind") != "NetworkPolicy"
                or not isinstance(name, str) or not re.fullmatch(r"[a-z0-9][a-z0-9.-]{0,252}", name)
                or not isinstance(namespace, str) or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,62}", namespace)
                or not isinstance(obj.get("spec"), dict) or meta.get("deletionTimestamp") is not None):
            raise Refusal("invalid policy identity")
        key = (namespace, name)
        if key in out:
            raise Refusal("duplicate policy identity")
        out[key] = copy.deepcopy(obj)
    encoded(items)
    return out


def version(obj):
    meta = obj["metadata"]
    uid, rv = meta.get("uid"), meta.get("resourceVersion")
    if not isinstance(uid, str) or not re.fullmatch(r"[0-9a-f-]{36}", uid):
        raise Refusal("policy UID missing")
    if not isinstance(rv, str) or not re.fullmatch(r"[0-9]{1,30}", rv):
        raise Refusal("policy resource version missing")
    return uid, rv


def same_inventory(left, right, exact_version=False):
    if left.keys() != right.keys():
        return False
    return all(semantic(left[key]) == semantic(right[key]) and
               (not exact_version or version(left[key]) == version(right[key])) for key in left)


def qualify_rendered_baseline(actual, desired, rendered):
    """Accept only the reviewed old or already-installed four-policy shape.

    Other policy names/specs must match the complete rendered services union.
    Existing metadata is captured, not overwritten by rendered metadata.
    """
    target = inventory(rendered)
    keys = {(NAMESPACE, name) for name in NAMES}
    if set(desired) != keys or not keys.issubset(target):
        raise Refusal("rendered ownership mismatch")
    if any(key[0] != NAMESPACE for key in target):
        raise Refusal("rendered union outside services namespace")
    for key in keys:
        if semantic(desired[key])["spec"] != semantic(target[key])["spec"]:
            raise Refusal("desired policy differs from render")
    if set(actual) - set(target) or set(target) - keys - set(actual):
        raise Refusal("unexpected policy union")
    for key, obj in actual.items():
        actual_spec = semantic(obj)["spec"]
        expected_spec = semantic(target[key])["spec"]
        if key != (NAMESPACE, NAMES[-1]):
            if actual_spec != expected_spec:
                raise Refusal("policy differs from rendered baseline")
            continue
        old_spec = copy.deepcopy(expected_spec)
        for app, port in (("accounting", 8101), ("audit", 8102)):
            rule = {"to": [{"podSelector": {"matchLabels": {"app": app}}}],
                    "ports": [{"protocol": "TCP", "port": port}]}
            if old_spec.get("egress", []).count(rule) != 1:
                raise Refusal("rendered egress addition is not exact")
            old_spec["egress"].remove(rule)
        if actual_spec not in (old_spec, expected_spec):
            raise Refusal("gateway egress differs beyond two reviewed peers")
    if (NAMESPACE, NAMES[-1]) not in actual:
        raise Refusal("existing gateway egress policy missing")


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

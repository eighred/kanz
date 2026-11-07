"""Shared bounded policy state and durable receipts; no mutation engine."""
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



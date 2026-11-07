"""Read-only validation of the one retained Tokyo transaction, never restoration."""
import copy
import hashlib
import json
import os
import re
import stat
import sys

import transaction as tx

DIRECTORY = "/var/tmp/kanz-gateway-policy-nu1ezqwo"
OLD_SOURCE = "0769d36cf857993284109d03868393a0c7e0d7c8"
COMMAND = "21d730ab-18c9-43ba-9427-522d557b6f2c"
ANNOTATION = "kubectl.kubernetes.io/last-applied-configuration"
TARGET = (tx.NAMESPACE, "allow-gateway-runtime-egress")
UUID = r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"


def document(raw):
    if not isinstance(raw, bytes) or not raw or len(raw) > 2 * 1024 * 1024:
        raise tx.Refusal("receipt JSON exceeds bound")
    def pairs(values):
        result = {}
        for key, value in values:
            if key in result:
                raise tx.Refusal("duplicate receipt field")
            result[key] = value
        return result
    def invalid(value):
        raise tx.Refusal("nonfinite receipt value")
    try:
        result = json.loads(raw, object_pairs_hook=pairs, parse_constant=invalid)
    except (ValueError, RecursionError, UnicodeError):
        raise tx.Refusal("invalid receipt JSON") from None
    if not isinstance(result, dict):
        raise tx.Refusal("receipt object required")
    return result


def stat_identity(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_nlink,
            info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def read_receipts():
    # Exact old directory; no caller-selected paths and no writes/cache cleanup.
    if sys.platform != "linux" or os.geteuid() != 0:
        raise tx.Refusal("Linux root receipt observation required")
    flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC
    directory = os.open(DIRECTORY, flags | os.O_DIRECTORY)
    try:
        before_dir = os.fstat(directory)
        if before_dir.st_uid != 0 or before_dir.st_mode & 0o077:
            raise tx.Refusal("receipt directory ownership differs")
        names = sorted(os.listdir(directory))
        # kubectl's private cache may be present, but is never opened or trusted.
        receipts = [name for name in names if name != ".kube"]
        if receipts != ["receipt-%04d.json" % i for i in range(1, 40)]:
            raise tx.Refusal("retained receipt sequence differs")
        result, hashes, total = [], [], 0
        identities = []
        for name in receipts:
            fd = os.open(name, flags, dir_fd=directory)
            try:
                before = os.fstat(fd)
                if (not stat.S_ISREG(before.st_mode) or before.st_uid != 0 or before.st_nlink != 1
                        or before.st_mode & 0o077 or not 0 < before.st_size <= 2 * 1024 * 1024):
                    raise tx.Refusal("receipt file custody differs")
                total += before.st_size
                if total > 16 * 1024 * 1024:
                    raise tx.Refusal("aggregate receipt bound exceeded")
                chunks, size = [], 0
                while True:
                    block = os.read(fd, min(65536, before.st_size + 1 - size))
                    if not block:
                        break
                    chunks.append(block)
                    size += len(block)
                    if size > before.st_size:
                        raise tx.Refusal("receipt grew during read")
                raw = b"".join(chunks)
                if len(raw) != before.st_size or stat_identity(before) != stat_identity(os.fstat(fd)):
                    raise tx.Refusal("receipt changed during read")
                result.append(document(raw))
                hashes.append(hashlib.sha256(raw).hexdigest())
                identities.append((name, stat_identity(before)))
            finally:
                os.close(fd)
        if names != sorted(os.listdir(directory)) or stat_identity(before_dir) != stat_identity(os.fstat(directory)):
            raise tx.Refusal("receipt directory changed during read")
        for name, identity in identities:
            if stat_identity(os.stat(name, dir_fd=directory, follow_symlinks=False)) != identity:
                raise tx.Refusal("receipt replaced during read")
        return result, tx.fingerprint(hashes)
    finally:
        os.close(directory)


def retained(records):
    if len(records) != 39:
        raise tx.Refusal("retained receipt count differs")
    transaction_format = "kanz-gateway-policy-transaction-v1"
    probe_format = "kanz-gateway-policy-probes-v1"
    expected = [(transaction_format, "prepared"), (transaction_format, "dry_run_passed"),
                (probe_format, "namespace_create_intent"), (probe_format, "namespace_create_observed")]
    for _ in range(5):
        expected.extend((probe_format, phase) for phase in ("pod_create_intent", "pod_address_observed",
            "pod_create_observed", "pod_address_observed", "pod_running_unready_observed"))
    expected.extend((transaction_format, phase) for phase in ("write_intent", "rollback", "rollback_refused"))
    expected.extend([(probe_format, "probe_cleanup_intent"),
                     *[(probe_format, "pod_absence_observed")] * 5, (probe_format, "probe_cleanup_verified")])
    transaction_keys = {"format", "nonce", "phase", "writes", "rollback", "baseline", "desiredSHA256"}
    probe_keys = {"format", "nonce", "phase", "namespace", "namespaceUID", "pods"}
    pod_keys = {"name", "namespace", "uid", "address", "addresses", "addressState", "addressUncertain", "deleted"}
    namespace_uid = None
    for record, (format_name, phase) in zip(records, expected):
        keys = transaction_keys if format_name == transaction_format else probe_keys
        if not isinstance(record, dict) or set(record) != keys or (record.get("format"), record.get("phase")) != (format_name, phase):
            raise tx.Refusal("retained receipt schema/phase sequence differs")
        if format_name == probe_format:
            pods = record["pods"]
            if not isinstance(pods, list) or len(pods) > 5 or any(not isinstance(pod, dict) or set(pod) != pod_keys for pod in pods):
                raise tx.Refusal("retained probe entry schema differs")
            observed_uid = record["namespaceUID"]
            if (phase == "namespace_create_intent") != (observed_uid is None):
                raise tx.Refusal("retained namespace creation custody differs")
            if namespace_uid is not None and observed_uid != namespace_uid:
                raise tx.Refusal("retained namespace UID custody regressed")
            if observed_uid is not None:
                if not isinstance(observed_uid, str) or not re.fullmatch(UUID, observed_uid):
                    raise tx.Refusal("retained namespace UID malformed")
                namespace_uid = observed_uid
    transactions = [r for r in records if r.get("format") == "kanz-gateway-policy-transaction-v1"]
    probes = [r for r in records if r.get("format") == "kanz-gateway-policy-probes-v1"]
    if len(transactions) + len(probes) != len(records) or not transactions or not probes:
        raise tx.Refusal("unknown receipt format")
    nonce = transactions[0].get("nonce")
    if not isinstance(nonce, str) or not re.fullmatch(r"[0-9a-f]{32}", nonce):
        raise tx.Refusal("receipt nonce missing")
    if any(r.get("nonce") != nonce for r in records):
        raise tx.Refusal("receipt nonce differs")
    if [r.get("phase") for r in transactions] != ["prepared", "dry_run_passed", "write_intent", "rollback", "rollback_refused"]:
        raise tx.Refusal("transaction history is not the retained unresolved intent")
    base = tx.inventory(transactions[0].get("baseline"))
    for obj in base.values():
        tx.version(obj)
        if tx.TOKEN in obj["metadata"].get("annotations", {}):
            raise tx.Refusal("baseline contains transaction marker")
    final = transactions[-1]
    if final.get("rollback") != "unresolved" or len(final.get("writes", [])) != 1:
        raise tx.Refusal("retained rollback is not unresolved")
    entry = final["writes"][0]
    if set(entry) != {"namespace", "name", "before", "intended", "observed"} or (entry["namespace"], entry["name"]) != TARGET or entry["observed"] is not None:
        raise tx.Refusal("unresolved intent identity differs")
    if TARGET not in base or entry["before"] != base[TARGET]:
        raise tx.Refusal("intent baseline differs")
    tx.inventory([entry["intended"]])
    if tx.version(entry["intended"]) != tx.version(base[TARGET]) or entry["intended"]["metadata"].get("annotations", {}).get(tx.TOKEN) != nonce:
        raise tx.Refusal("intent ownership differs")
    for index, record in enumerate(transactions):
        if record["rollback"] != ("not_needed" if index < 3 else ("pending" if index == 3 else "unresolved")):
            raise tx.Refusal("retained rollback state sequence differs")
        if tx.inventory(record.get("baseline")) != base or record.get("desiredSHA256") != final.get("desiredSHA256"):
            raise tx.Refusal("transaction baseline changed")
        if record.get("writes") != ([] if index < 2 else [entry]):
            raise tx.Refusal("transaction intent changed")
    if records[29] != transactions[2] or records[31] != final or records[-1].get("phase") != "probe_cleanup_verified":
        raise tx.Refusal("retained receipt positions differ")
    return base, entry, nonce, probes


def target_matches(current, baseline, entry, nonce):
    if current.keys() != baseline.keys():
        raise tx.Refusal("policy inventory names changed")
    for key in baseline.keys() - {TARGET}:
        if current[key] != baseline[key] or not tx.same_inventory({key: current[key]}, {key: baseline[key]}, exact_version=True):
            raise tx.Refusal("unowned policy changed")
    actual = current[TARGET]
    if tx.version(actual)[0] != tx.version(baseline[TARGET])[0]:
        raise tx.Refusal("target UID changed")
    old_meta, live_meta = baseline[TARGET]["metadata"], actual["metadata"]
    if (("creationTimestamp" in old_meta, old_meta.get("creationTimestamp")) !=
            ("creationTimestamp" in live_meta, live_meta.get("creationTimestamp"))):
        raise tx.Refusal("target immutable creation timestamp changed")
    intended = entry["intended"]
    expected = copy.deepcopy(intended)
    annotation = actual["metadata"].get("annotations", {}).get(ANNOTATION)
    old_annotation = intended["metadata"].get("annotations", {}).get(ANNOTATION)
    if not isinstance(annotation, str) or not isinstance(old_annotation, str) or not old_annotation:
        raise tx.Refusal("known kubectl annotation transformation absent")
    serialized = copy.deepcopy(intended)
    serialized["metadata"]["annotations"].pop(ANNOTATION)
    if document(annotation.encode()) != serialized:
        raise tx.Refusal("annotation is not the exact originally submitted object")
    expected["metadata"]["annotations"][ANNOTATION] = annotation
    if tx.semantic(actual) != tx.semantic(expected) or actual["metadata"]["annotations"].get(tx.TOKEN) != nonce:
        raise tx.Refusal("target differs beyond known annotation transformation")
    return actual

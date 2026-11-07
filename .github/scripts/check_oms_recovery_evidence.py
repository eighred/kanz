"""Exact-source recovery proof; generic verifier owns no-skip/package checks."""
import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import stat
import sys
from check_migration_evidence import verify, PREFIX

PACKAGE = "services/oms/tools/recovery-conformance"
GROUPS = {
 "TestRustRecoveryObservation": "complete-terminal-history complete-raw-observation-unknown complete-large-history-small-facts complete-single-round-weighted-average complete-identical-redelivery-history missing-order-blocked conflicting-observation-blocked incomplete-total-blocked missing-total-blocked unknown-view-blocked duplicate-execution-conflict-blocked".split(),
 "TestRustRecoveryPersistedFeeApproval": "approved stale-approved-baseline missing-approval same-actor-approval expired-approval exact-expiry-approval immediately-before-expiry proposal-unknown-bound proposal-unknown-unbound invalid-change-before-valid duplicate-valid-change proposal-wrong-history proposal-wrong-mapping".split(),
 "TestRustRecoveryAdmissionRefusals": "foreign-tenant missing-tenant command-not-fact wrong-subject empty-payload malformed-payload oversized-observation".split(),
 "TestRustRecoveryDurableTailRollback": ["outbox-insert", "deferred-commit"],
 "TestRustRecoveryObservationPayloadBoundary": ["above-standard-message-bound", "exact-four-mib", "one-byte-over-four-mib"],
 **{name: ["go", "rust"] for name in ("TestRustRecoveryOversizedSingleFactRefusal", "TestRustRecoveryComplete8000History", "TestRustRecovery8001HistoryRefusal", "TestRustRecoveryDefaultLockCapacityRefusal", "TestRustRecoveryMalformedPostedJournal", "TestRustRecoveryTenantIsolation", "TestRustRecoveryExplicitZeroHistory", "TestRustRecoveryFrozenMappingAttribution")},
}
NAMES = set(GROUPS) | {f"{name}/{child}" for name, children in GROUPS.items() for child in children} | {"TestRustRecoveryConcurrentGoWriter", "TestRustRecoveryCrashDuringHistoryQuery", "TestRecoveryObservationPaddingOracle"}
REQUIRED = {PACKAGE: NAMES}
STREAMS = ("recovery.jsonl", "large-go.jsonl", "large-rust.jsonl")
MAX_STREAM_BYTES = 64 * 1024 * 1024
MAX_PROOF_BYTES = 64 * 1024
HEX = re.compile(r"[0-9a-f]{40}\Z")

def strict(pairs):
    out = {}
    for key, value in pairs:
        if key in out: raise ValueError("duplicate evidence field")
        out[key] = value
    return out

def read_bounded(path, limit):
    # Bound before parsing or hashing; growth after fstat is also refused.
    if sys.platform != "linux" or not hasattr(os, "O_NOFOLLOW") or not hasattr(os, "O_NONBLOCK"):
        raise ValueError("Linux no-follow/nonblocking evidence reader required")
    if path.is_symlink(): raise ValueError("evidence symlink refused")
    flags = os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW
    with os.fdopen(os.open(path, flags), "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise ValueError("evidence is not a bounded regular file")
        raw = stream.read(limit + 1)
    if len(raw) != info.st_size or len(raw) > limit: raise ValueError("evidence changed during read")
    return raw

def decode_events(raw):
    return [json.loads(line, object_pairs_hook=strict) for line in raw.decode("utf-8").splitlines() if line]

def events(path):
    return decode_events(read_bounded(path, MAX_STREAM_BYTES))

def checked_streams(streams):
    all_events = []
    large = "TestRustRecoveryComplete8000History"
    for filename in STREAMS:
        batch = decode_events(streams[filename])
        required = NAMES - {large, large + "/go", large + "/rust"} if filename == "recovery.jsonl" else {large, large + ("/go" if filename == "large-go.jsonl" else "/rust")}
        verify(batch, {PACKAGE: required})
        all_events += batch
    verify(all_events, REQUIRED)
    counts = Counter(e.get("Test") for e in all_events if e.get("Action") == "pass" and e.get("Package") == PREFIX + PACKAGE and e.get("Test"))
    for name in NAMES:
        if counts[name] != (2 if name == large else 1): raise ValueError("missing/duplicate recovery outcome: " + name)
    def output(test):
        return "".join(e.get("Output", "") for e in all_events if e.get("Test") == test and e.get("Action") == "output")
    for owner in ("go", "rust"):
        if "max_locks_per_transaction=1024 max_connections=100 max_prepared_transactions=0" not in output(large + "/" + owner): raise ValueError("missing explicit large lock capacity")
        if "max_locks_per_transaction=64 max_connections=100 max_prepared_transactions=0" not in output("TestRustRecoveryDefaultLockCapacityRefusal/" + owner): raise ValueError("missing default lock capacity")
    match = re.search(r"peak_rss_kib=([0-9]+) current_rss_kib=([0-9]+)", output(large + "/rust"))
    if not match or not 0 < int(match[2]) <= int(match[1]): raise ValueError("missing measured process memory")
    for owner in ("go", "rust"):
        if "actual PostgreSQL role: nosuperuser=true nobypassrls=true" not in output(large + "/" + owner): raise ValueError("missing runtime role proof")
        if "actual PostgreSQL recovery owner tables: forced_rls=12 expected=12" not in output(large + "/" + owner): raise ValueError("missing exact runtime RLS proof")
    if "canonical Go default-capacity SQLSTATE=53200" not in output("TestRustRecoveryDefaultLockCapacityRefusal/go"): raise ValueError("missing exact default-capacity cause")
    broker = "".join(e.get("Output", "") for e in all_events)
    limits = re.search(r"actual recovery broker limits: server_max_payload=([0-9]+) stream_max_msg_size=(-?[0-9]+)", broker)
    if not limits or int(limits[1]) <= 0: raise ValueError("missing actual broker limit")
    return {"required_passes": len(NAMES), "peak_rss_kib": int(match[1]), "current_rss_kib": int(match[2])}

def checked(directory):
    return checked_streams({name: read_bounded(directory / name, MAX_STREAM_BYTES) for name in STREAMS})

def metadata(source, tree, run, attempt):
    if not HEX.fullmatch(source) or not HEX.fullmatch(tree) or not run.isdecimal() or not attempt.isdecimal() or int(run) <= 0 or int(attempt) <= 0: raise ValueError("invalid source/run binding")
    return {"version": 1, "source_sha": source, "tree_sha": tree, "run_id": run, "run_attempt": attempt}

def qualify(directory, expected, create=False):
    streams = {name: read_bounded(directory / name, MAX_STREAM_BYTES) for name in STREAMS}
    result = checked_streams(streams)
    bound = {**expected, "streams": {name: hashlib.sha256(raw).hexdigest() for name, raw in streams.items()}, "result": result}
    path = directory / "proof.json"
    if create:
        path.write_text(json.dumps(bound, sort_keys=True) + "\n", encoding="utf-8")
    else:
        saved = json.loads(read_bounded(path, MAX_PROOF_BYTES).decode("utf-8"), object_pairs_hook=strict)
        if not isinstance(saved, dict) or type(saved.get("version")) is not int or not isinstance(saved.get("result"), dict) or any(type(value) is not int for value in saved["result"].values()) or saved != bound:
            raise ValueError("recovery proof source/run/content mismatch")
    return result

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("directory", type=Path)
    parser.add_argument("--create", action="store_true")
    for name in ("source", "tree", "run", "attempt"): parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    if args.create:
        head = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
        tree = subprocess.check_output(["git", "rev-parse", "HEAD^{tree}"], text=True).strip()
        if (head, tree, os.environ.get("GITHUB_RUN_ID"), os.environ.get("GITHUB_RUN_ATTEMPT")) != (args.source, args.tree, args.run, args.attempt): raise ValueError("runner identity differs")
    print(json.dumps(qualify(args.directory, metadata(args.source, args.tree, args.run, args.attempt), args.create), sort_keys=True))

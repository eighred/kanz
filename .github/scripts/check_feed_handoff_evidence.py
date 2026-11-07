"""Source-bound controlled feed rehearsal, not distributed publisher fencing."""
import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import re
import subprocess

from check_migration_evidence import verify, PREFIX
from check_oms_recovery_evidence import read_bounded, decode_events, strict

PACKAGE = "tools/depth-conformance"
TOP = "TestFeedRuntimeControlledHandoff"
NAMES = {TOP, "TestFeedHandoffExactRepeatControl"} | {f"{TOP}/{v}" for v in ("binance", "okx")} | {
    f"{TOP}/{v}/{case}" for v in ("binance", "okx")
    for case in ("authenticated-publish-denied", "duplicate-external-assignment-refused")
}
BINARIES = ("market-ingest", "kanz-binance-depth", "kanz-okx-depth", "handoff.test")
HEX = re.compile(r"[0-9a-f]{40}\Z")


def checked(raw):
    batch = decode_events(raw)
    if not batch or any(e.get("Package") != PREFIX + PACKAGE for e in batch):
        raise ValueError("unexpected handoff package")
    verify(batch, {PACKAGE: NAMES})
    passes = Counter(e.get("Test", "") for e in batch if e.get("Action") == "pass")
    if passes != Counter({name: 1 for name in NAMES | {""}}):
        raise ValueError("handoff outcome inventory differs")
    if any(e.get("Test") and e["Test"] not in NAMES for e in batch):
        raise ValueError("unexpected handoff test")
    return {"required": len(NAMES), "skipped": 0, "packages": 1}


def identity(sha, tree, run, attempt):
    if not HEX.fullmatch(sha) or not HEX.fullmatch(tree):
        raise ValueError("source identity invalid")
    if not re.fullmatch(r"[1-9][0-9]*", run) or not re.fullmatch(r"[1-9][0-9]*", attempt):
        raise ValueError("run identity invalid")
    return {"source": sha, "tree": tree, "run": run, "attempt": attempt}


def inventory(directory):
    if {p.name for p in directory.iterdir()} != set(BINARIES):
        raise ValueError("binary inventory differs")
    result = {}
    for name in BINARIES:
        raw = read_bounded(directory / name, 512 * 1024 * 1024)
        if not raw.startswith(b"\x7fELF"):
            raise ValueError("Linux executable required")
        result[name] = hashlib.sha256(raw).hexdigest()
    return result


def qualify(directory, binaries, expected):
    manifest = json.loads(read_bounded(directory / "manifest.json", 16 * 1024), object_pairs_hook=strict)
    wanted = {**expected, "binaries": inventory(binaries), "kind": "controlled-feed-rehearsal"}
    if manifest != wanted:
        raise ValueError("handoff build/source binding differs")
    raw = read_bounded(directory / "tests.jsonl", 16 * 1024 * 1024)
    return {**wanted, "result": checked(raw), "tests_sha256": hashlib.sha256(raw).hexdigest()}


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--prepare", action="store_true")
    p.add_argument("--root", type=Path, required=True)
    p.add_argument("--proof", type=Path, required=True)
    p.add_argument("--binaries", type=Path, required=True)
    for key in ("sha", "tree", "run", "attempt"):
        p.add_argument("--" + key, required=True)
    a = p.parse_args()
    expected = identity(a.sha, a.tree, a.run, a.attempt)
    observed = subprocess.check_output(["git", "-C", str(a.root), "rev-parse", "HEAD", "HEAD^{tree}"], timeout=10, text=True).splitlines()
    if observed != [a.sha, a.tree]:
        raise ValueError("checkout identity differs")
    subprocess.run(["git", "-C", str(a.root), "diff", "--quiet", "HEAD", "--", "."], timeout=10, check=True)
    if a.prepare:
        body = {**expected, "binaries": inventory(a.binaries), "kind": "controlled-feed-rehearsal"}
        path = a.proof / "manifest.json"
    else:
        body = qualify(a.proof, a.binaries, expected)
        path = a.proof / "proof.json"
    with path.open("x", encoding="utf-8") as out:
        json.dump(body, out, sort_keys=True)
        out.write("\n")


if __name__ == "__main__":
    main()

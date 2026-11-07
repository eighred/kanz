"""Deterministic serial Go shards and exact-source, complete-union evidence."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

from go_test_evidence import check, events

COUNT = 2
# Distribute these integration workloads across the serial race runners.
# Routing changes ownership only: the same complete inventory and fresh
# serial tests remain mandatory.
SHARD_OWNERS = {
    "github.com/eighred/kanz/pkg/bus": 1,
    "github.com/eighred/kanz/services/risk-engine/internal/app": 0,
}
MAX_PACKAGES = 10000
MAX_INVENTORY = 4 * 1024 * 1024
MAX_EVENTS = 256 * 1024 * 1024


def bounded_bytes(path, limit):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > limit:
        raise ValueError("missing, linked or oversized evidence file")
    with path.open("rb") as source:
        data = source.read(limit + 1)
    if len(data) > limit:
        raise ValueError("evidence file grew beyond its bound")
    return data


def inventory(text):
    rows = text.splitlines()
    if not rows or len(rows) > MAX_PACKAGES or len(rows) != len(set(rows)) or any(
        not re.fullmatch(r"github\.com/eighred/kanz(?:/[A-Za-z0-9_.-]+)*", p) for p in rows
    ):
        raise ValueError("invalid or duplicate package inventory")
    return sorted(rows)


def partition(packages, shard):
    if type(shard) is not int or shard not in range(COUNT):
        raise ValueError("invalid shard")
    return [p for p in packages if SHARD_OWNERS.get(p, int(hashlib.sha256(p.encode()).hexdigest(), 16) % COUNT) == shard]


def require_routed_owners(packages):
    if set(SHARD_OWNERS) - set(packages) or any(type(s) is not int or s not in range(COUNT) for s in SHARD_OWNERS.values()):
        raise ValueError("routed package disappeared or has an invalid shard")


def identity():
    sha = os.environ["GITHUB_SHA"]
    run, attempt = os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"]
    if not re.fullmatch(r"[0-9a-f]{40}", sha) or not all(re.fullmatch(r"[1-9][0-9]*", x) for x in (run, attempt)):
        raise ValueError("invalid source/run identity")
    actual = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    tree = subprocess.check_output(["git", "rev-parse", "HEAD^{tree}"], text=True).strip()
    if actual != sha:
        raise ValueError("checkout does not match workflow SHA")
    return {"sha": sha, "tree": tree, "run": run, "attempt": attempt}


def validate_events(text, expected):
    started, completed, running_tests = set(), set(), set()
    no_tests = set()
    for e in events(text.splitlines()):
        p, test, action = e.get("Package"), e.get("Test", ""), e["Action"]
        if p not in expected:
            raise ValueError("unexpected event package")
        if action == "fail" or (not test and re.match(r"^ok\s+\S+\s+\(cached\)", e.get("Output", ""))):
            raise ValueError("failed or cached result")
        if "[no test files]" in e.get("Output", ""):
            no_tests.add(p)
        if not test and action == "start":
            if p in started:
                raise ValueError("duplicate package start")
            started.add(p)
        if test and action == "run":
            if (p, test) in running_tests:
                raise ValueError("duplicate test start")
            running_tests.add((p, test))
        if test and action in ("pass", "skip"):
            if (p, test) not in running_tests:
                raise ValueError("test result without start")
            running_tests.remove((p, test))
        if not test and action in ("pass", "skip"):
            if p not in started or p in completed or (action == "skip" and p not in no_tests):
                raise ValueError("invalid or duplicate package completion")
            completed.add(p)
    if completed != set(expected) or running_tests:
        raise ValueError("missing/truncated package or test results")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def seal(directory, shard, source):
    package_bytes = bounded_bytes(directory / "packages.txt", MAX_INVENTORY)
    event_bytes = bounded_bytes(directory / "events.jsonl", MAX_EVENTS)
    packages = inventory(package_bytes.decode())
    validate_events(event_bytes.decode(), packages)
    manifest = dict(source, shard=shard, count=COUNT,
                    packages_sha256=digest(package_bytes), events_sha256=digest(event_bytes))
    (directory / "manifest.json").write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")


def combine(root, packages, source):
    require_routed_owners(packages)
    combined, seen = [], set()
    names = {f"go-race-{source['run']}-{source['attempt']}-{s}" for s in range(COUNT)}
    if set(p.name for p in root.iterdir()) != names:
        raise ValueError("missing or unexpected shard artifacts")
    for shard in range(COUNT):
        directory = root / f"go-race-{source['run']}-{source['attempt']}-{shard}"
        if directory.is_symlink() or not directory.is_dir():
            raise ValueError("invalid shard directory")
        if set(p.name for p in directory.iterdir()) != {"manifest.json", "packages.txt", "events.jsonl"}:
            raise ValueError("unexpected shard artifact files")
        meta = json.loads(bounded_bytes(directory / "manifest.json", 4096))
        raw_packages = bounded_bytes(directory / "packages.txt", MAX_INVENTORY)
        raw_events = bounded_bytes(directory / "events.jsonl", MAX_EVENTS)
        expected_meta = dict(source, shard=shard, count=COUNT,
                             packages_sha256=digest(raw_packages), events_sha256=digest(raw_events))
        if meta != expected_meta:
            raise ValueError("artifact source/run/attempt/shard/content mismatch")
        selected = inventory(raw_packages.decode())
        if selected != partition(packages, shard) or seen.intersection(selected):
            raise ValueError("incorrect or overlapping shard inventory")
        validate_events(raw_events.decode(), selected)
        seen.update(selected)
        combined.extend(raw_events.decode().splitlines())
    if seen != set(packages):
        raise ValueError("incomplete shard union")
    problems, skipped, count = check(combined, packages)
    if problems:
        raise ValueError("; ".join(problems))
    return count, skipped


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("select", "seal", "aggregate"))
    parser.add_argument("--packages", type=Path)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--shard", type=int)
    args = parser.parse_args()
    source = identity()
    if args.mode == "select":
        packages = inventory(bounded_bytes(args.packages, MAX_INVENTORY).decode())
        require_routed_owners(packages)
        selected = partition(packages, args.shard)
        if not selected:
            raise ValueError("empty shard")
        args.directory.mkdir(parents=True, exist_ok=True)
        (args.directory / "packages.txt").write_text("\n".join(selected) + "\n", encoding="utf-8")
    elif args.mode == "seal":
        if type(args.shard) is not int or args.shard not in range(COUNT):
            raise ValueError("invalid shard")
        seal(args.directory, args.shard, source)
    else:
        count, skipped = combine(args.directory, inventory(bounded_bytes(args.packages, MAX_INVENTORY).decode()), source)
        print(f"{count} fresh package results in two disjoint shards match go list; {skipped} skips, none citing supplied backing services; browser proof present")


if __name__ == "__main__":
    main()

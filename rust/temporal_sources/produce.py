"""Bind the normal Linux release build's Cargo artifacts to the prepared source set and executable."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import tomllib

from prepare import HERE, digest, verify

ROOT = Path("/src/rust")
OUT = Path("/kanz-source-evidence")
REGISTRY = "registry+https://github.com/rust-lang/crates.io-index"
FORK = "sad-chrono"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source_revision")
    revision = parser.parse_args().source_revision
    if not re.fullmatch("[0-9a-f]{40}", revision) or HERE != ROOT / "temporal_sources":
        raise ValueError("expected the source-bound production Rust builder")
    host = subprocess.check_output(["rustc", "-vV"], text=True).splitlines()
    target = "x86_64-unknown-linux-gnu"
    if "host: " + target not in host or "release: 1.90.0" not in host:
        raise ValueError("unreviewed builder host or toolchain")
    manifest = HERE / "manifest.json"
    rows = json.loads(manifest.read_bytes())["packages"]
    verify(HERE / "generated", rows)
    lock = ROOT / "Cargo.lock"
    locked = tomllib.loads(lock.read_text(encoding="utf-8"))["package"]
    if any(row["name"] in {"chrono", "chrono-tz", "humantime"} for row in locked):
        raise ValueError("workspace selected an unqualified temporal dependency")
    selected, = (row for row in locked if row["name"] == FORK)
    if selected["source"] != REGISTRY:
        raise ValueError("the temporal bridge must be the real published archive")
    registry_id = f"{REGISTRY}#{FORK}@{selected['version']}"
    expected = {}
    for row in rows:
        name, version = row["name"], row["version"]
        folder = (Path("/src") / row["manifestPath"]).parent
        identity = f"path+{folder.as_uri()}#{name}@{version}"
        locked_row, = (p for p in locked if (p["name"], p["version"]) == (name, version))
        if "source" in locked_row or "checksum" in locked_row:
            raise ValueError(f"workspace source differs for {name} {version}")
        expected[identity] = row | {"packageID": identity}
    expected[registry_id] = {"packageID": registry_id, "name": FORK, "version": selected["version"],
                             "upstream": {"kind": "registry-archive", "sha256": selected["checksum"],
                                          "url": f"https://static.crates.io/crates/{FORK}/{FORK}-{selected['version']}.crate"}}
    root_id = f"path+file:///src/rust/services/lake_snapshot#kanz-lake-snapshot@0.1.0"
    binary = ROOT / "target-provenance/release/kanz-lake-snapshot"
    found, binaries, finished = set(), [], []
    for line in (OUT / "compiler.jsonl").read_bytes().splitlines():
        message = json.loads(line)
        if message["reason"] == "build-finished":
            finished.append(message["success"])
        if message["reason"] != "compiler-artifact":
            continue
        identity, kind = message["package_id"], message["target"]["kind"]
        if identity in expected and kind == ["lib"]:
            row = expected[identity]
            if identity in found or message["fresh"] or message["profile"]["test"] or message["profile"]["opt_level"] != "3":
                raise ValueError(f"source was not freshly compiled once for release: {identity}")
            if "manifestPath" in row and message["manifest_path"] != "/src/" + row["manifestPath"]:
                raise ValueError(f"compiler manifest and prepared source disagree: {identity}")
            found.add(identity)
        if identity == root_id and kind == ["bin"]:
            if message["fresh"] or message["profile"]["test"] or message["profile"]["opt_level"] != "3":
                raise ValueError("final production executable must be freshly built")
            binaries.append(message["executable"])
    if found != set(expected) or binaries != [str(binary)] or finished != [True]:
        raise ValueError("release build omitted a required source or executable")
    receipt = {
        "schema": "kanz.rust-source-provenance.v1", "sourceRevision": revision, "target": target,
        "service": "lake-sink", "binarySHA256": digest(binary), "lockSHA256": digest(lock),
        "patchSetSHA256": digest(manifest), "advisorySnapshotSHA256": digest(OUT / "chrono-advisories.json"),
        "packages": [expected[key] for key in sorted(expected)],
    }
    (OUT / "provenance.json").write_text(json.dumps(receipt, sort_keys=True, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()

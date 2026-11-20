"""Select the registry's sad-chrono package in the Apache crates that share its Rust types."""
import json
from pathlib import Path
import tomllib

from chrono_advisories import CHECKSUM, VERSION, evaluate

CONSUMERS = {
    "54.1.0": (
        "datafusion", "datafusion-common", "datafusion-expr", "datafusion-execution",
        "datafusion-functions", "datafusion-physical-expr-common", "datafusion-datasource",
        "datafusion-datasource-arrow", "datafusion-catalog-listing", "datafusion-optimizer",
    ),
    "58.4.1": ("arrow", "arrow-array", "arrow-arith", "arrow-cast", "arrow-json", "arrow-csv"),
    "59.3.2": ("arrow-array", "parquet"),
}


def select_registry_fork(folder):
    manifest = folder / "Cargo.toml"
    text = manifest.read_text()
    expected = tomllib.loads(text)
    changes = 0
    for group in ("dependencies", "dev-dependencies", "build-dependencies"):
        dep = expected.get(group, {}).get("chrono")
        if dep is None:
            continue
        if "package" in dep or dep["version"] not in {"0.4.40", "0.4.44"}:
            raise ValueError(f"unreviewed source requirement in {manifest} [{group}.chrono]")
        old = f'[{group}.chrono]\nversion = "{dep["version"]}"'
        if text.count(old) != 1:
            raise ValueError(f"ambiguous dependency location in {manifest}")
        text = text.replace(old, f'[{group}.chrono]\npackage = "sad-chrono"\nversion = "={VERSION}"')
        dep["package"], dep["version"] = "sad-chrono", f"={VERSION}"
        changes += 1
    if not changes or tomllib.loads(text) != expected:
        raise ValueError(f"unexpected Apache manifest change in {manifest}")
    manifest.write_text(text)


def prepare(work, owner, patches, pins, unpack):
    for version, names in CONSUMERS.items():
        for name in names:
            folder = work / f"{name}-{version}"
            if not folder.is_dir():
                folder = unpack({"name": name, "version": version}, pins)
            select_registry_fork(folder)
            # Cargo requires separate patch keys for the two versions of arrow-array.
            key = "arrow-array-writer" if (name, version) == ("arrow-array", "59.3.2") else name
            patches[key] = folder
    manifest = owner / "services/lake_snapshot/Cargo.toml"
    with manifest.open("a") as out:
        out.write(f'\n[dev-dependencies.sad-chrono]\nversion = "={VERSION}"\ndefault-features = false\n')


def verify(lock, artifacts):
    row, = (p for p in lock if p["name"] == "sad-chrono")
    source = "registry+https://github.com/rust-lang/crates.io-index"
    if row["source"] != source:
        raise ValueError("sad-chrono must retain its authentic registry identity")
    evaluate(json.loads(Path("/inputs/chrono-advisories.json").read_bytes()), row["version"], row["checksum"])
    expected = source + f"#sad-chrono@{VERSION}"
    selected = {r["package_id"] for r in artifacts if r["target"]["name"] == "sad_chrono"}
    if selected != {expected} or any(p["name"] == "chrono" for p in lock):
        raise ValueError("legacy and maintained package identities differ from the qualified graph")
    if any(r["target"]["name"] == "chrono" for r in artifacts):
        raise ValueError("the worker compiled the unmaintained package")
    print(f"Qualified published {expected}, archive {CHECKSUM}.")

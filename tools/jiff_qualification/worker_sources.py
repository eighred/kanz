"""Check the debug worker's actual Cargo records against the same source recipe as release."""
import json
from pathlib import Path
import tomllib

from chrono_advisories import evaluate


def verify(lines):
    root = Path("/work/kanz")
    rust = root / "rust"
    rows = json.loads((rust / "temporal_sources/manifest.json").read_bytes())["packages"]
    compiled = {record["package_id"]: record["manifest_path"] for line in lines if line.startswith("{")
                and (record := json.loads(line))["reason"] == "compiler-artifact" and record["target"]["kind"] == ["lib"]}
    lock = tomllib.loads((rust / "Cargo.lock").read_text())["package"]
    if {p["name"] for p in lock} & {"humantime", "chrono", "chrono-tz"}:
        raise ValueError("legacy temporal package in the changed production workspace")
    for row in rows:
        manifest = root / row["manifestPath"]
        package = f"path+{manifest.parent.as_uri()}#{row['name']}@{row['version']}"
        if compiled.get(package) != str(manifest):
            raise ValueError(f"compiled source differs for {package}")
    published, = (p for p in lock if p["name"] == "sad-chrono")
    registry = "registry+https://github.com/rust-lang/crates.io-index"
    if published["source"] != registry:
        raise ValueError("the bridge is not the real registry package")
    evaluate(json.loads(Path("/inputs/chrono-advisories.json").read_bytes()), published["version"], published["checksum"])
    for coordinate in (f"sad-chrono@{published['version']}", "jiff@1.0.1", "jiff-tzdb@1.0.0"):
        if f"{registry}#{coordinate}" not in compiled:
            raise ValueError(f"missing compiler record for {coordinate}")
    print(f"Verified {len(rows)} prepared Apache sources and the published temporal packages in the real workspace.")

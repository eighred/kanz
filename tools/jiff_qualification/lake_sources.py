"""Prepare and verify the disposable Arrow/DataFusion timezone backport for the lake worker."""
import hashlib
import json
import re
from pathlib import Path
import subprocess
import tomllib


WORK = Path("/work")
OWNER = WORK / "kanz/rust"
CACHE = WORK / "cargo/registry"


def packages(lock):
    return tomllib.loads(lock.read_text())["package"]


def replace(path, old, new):
    text = path.read_text()
    if text.count(old) != 1:
        raise ValueError(f"unexpected adaptation point in {path}")
    path.write_text(text.replace(old, new))


def archive(row, pins):
    name, version = row["name"], row["version"]
    file, = (CACHE / "cache").glob(f"*/{name}-{version}.crate")
    if hashlib.sha256(file.read_bytes()).hexdigest() != pins[name, version]:
        raise ValueError(f"published package differs from the Kanz pin: {name} {version}")
    return file


def unpack(row, pins):
    file = archive(row, pins)
    # Cargo already accepted and extracted this archive in the same fresh volume.
    subprocess.run(["tar", "--no-same-owner", "--no-same-permissions", "-xzf", file, "-C", WORK], check=True)
    return WORK / file.stem


def prepare_arrow(row, pins):
    folder = unpack(row, pins)
    manifest = folder / "Cargo.toml"
    if row["name"] == "arrow":
        replace(manifest, '[features]', '[features]\njiff = ["arrow-array/jiff"]')
    else:
        (folder / "src/timezone.rs").write_bytes(Path("/inputs/timezone.rs").read_bytes())
        replace(manifest, '[features]', '[features]\njiff = ["dep:jiff", "dep:jiff-tzdb"]')
        with manifest.open("a") as out:
            out.write('\n[dependencies.jiff]\nversion = "=1.0.1"\ndefault-features = false\n'
                      'features = ["std", "tz-fat", "perf-inline"]\noptional = true\n'
                      '\n[dependencies.jiff-tzdb]\nversion = "=1.0.0"\noptional = true\n')
    return folder


def prepare_datafusion(row, pins):
    name = row["name"]
    # Cargo expands Apache's one workspace Arrow feature into each published manifest.
    manifest, = (CACHE / "src").glob(f"*/{name}-54.1.0/Cargo.toml")
    expected = tomllib.loads(manifest.read_text())
    arrow = expected.get("dependencies", {}).get("arrow", {})
    if "chrono-tz" not in arrow.get("features", []):
        return None
    folder = unpack(row, pins)
    manifest = folder / "Cargo.toml"
    text = manifest.read_text()
    header = re.search(r"^\[dependencies\.arrow\]\n(?P<table>.*?)(?=^\[|\Z)", text, re.MULTILINE | re.DOTALL)
    if not header or header["table"].count('"chrono-tz"') != 1:
        raise ValueError(f"unexpected published Arrow dependency in {name}")
    table = header["table"].replace('"chrono-tz"', '"jiff"')
    manifest.write_text(text[:header.start("table")] + table + text[header.end("table"):])
    arrow["features"][arrow["features"].index("chrono-tz")] = "jiff"
    if name == "datafusion-functions":
        del expected["dependencies"]["chrono-tz"]
        expected["features"]["datetime_expressions"] = []
        replace(manifest, 'datetime_expressions = ["chrono-tz"]', 'datetime_expressions = []')
        replace(manifest, '[dependencies.chrono-tz]\nversion = "0.10.4"\noptional = true\n', '')
        parser = folder / "src/datetime/common.rs"
        replace(parser, 'Option<chrono_tz::Tz>', 'Option<Tz>')
        replace(parser, """            let result: Result<chrono_tz::Tz, chrono_tz::ParseError> =
                timezone_name.parse();
            let Ok(tz) = result else {
                return Err(err(&result.unwrap_err().to_string()));
            };""", """            if timezone_name.starts_with(['+', '-']) {
                return Err(err("failed to parse timezone"));
            }
            let tz = timezone_name.parse::<Tz>().map_err(|_| err("failed to parse timezone"))?;""")
    if tomllib.loads(manifest.read_text()) != expected:
        raise ValueError(f"unrelated TOML change in {name}")
    return folder


def prepare():
    pins = {(p["name"], p["version"]): p["checksum"] for p in packages(WORK / "kanz.lock") if "checksum" in p}
    patches = {}
    for row in packages(OWNER / "Cargo.lock"):
        name, version = row["name"], row["version"]
        if (name, version) == ("arrow-array", "59.3.2"):
            archive(row, pins)  # The writer must stay on the registry release.
        elif name in {"arrow", "arrow-array"} and version == "58.4.1":
            patches[name] = prepare_arrow(row, pins)
        elif (name == "datafusion" or name.startswith("datafusion-")) and version == "54.1.0":
            folder = prepare_datafusion(row, pins)
            if folder:
                patches[name] = folder
    with (OWNER / "Cargo.toml").open("a") as out:
        for name, folder in sorted(patches.items()):
            out.write(f'{name} = {{ path = "{folder}" }}\n')


def verify(lines):
    artifacts = [r for line in lines if line.startswith("{") and (r := json.loads(line))["reason"] == "compiler-artifact"]
    lock = packages(OWNER / "Cargo.lock")
    names = {p["name"] for p in lock}
    targets = {r["target"]["name"] for r in artifacts}
    if names & {"humantime", "chrono-tz"} or targets & {"humantime", "chrono_tz"}:
        raise ValueError("the selected worker still depends on humantime or chrono-tz")
    registry = "registry+https://github.com/rust-lang/crates.io-index#"
    for name, ids in {
        "arrow_array": {"path+file:///work/arrow-array-58.4.1#arrow-array@58.4.1", registry + "arrow-array@59.3.2"},
        "datafusion": {"path+file:///work/datafusion-54.1.0#datafusion@54.1.0"},
        "datafusion_functions": {"path+file:///work/datafusion-functions-54.1.0#datafusion-functions@54.1.0"},
        "object_store": {"path+file:///work/source#object_store@0.13.2"},
        "jiff": {registry + "jiff@1.0.1"}, "jiff_tzdb": {registry + "jiff-tzdb@1.0.0"},
        "chrono": {registry + "chrono@0.4.45"},
    }.items():
        selected = {r["package_id"] for r in artifacts if r["target"]["name"] == name}
        if selected != ids:
            raise ValueError(f"unexpected {name} compiler sources: {selected}")
    print(f"Verified combined worker sources in {len(artifacts)} compiler artifacts; chrono 0.4.45 remains.")


if __name__ == "__main__":
    prepare()

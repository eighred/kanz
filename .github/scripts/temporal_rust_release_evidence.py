"""Check the temporal source receipt from the protected lake worker image build."""
import hashlib
import json
from pathlib import PurePosixPath
import re
from urllib.parse import unquote


SCHEMA = "kanz.rust-source-provenance.v1"
REGISTRY = "registry+https://github.com/rust-lang/crates.io-index"
TEMPORAL = "sad-chrono"
RUST_ROOT = "/src/rust"
WORKER = "kanz-lake-snapshot"
MAX_COMPILER_LOG = 16 * 1024 * 1024


def unique(pairs):
    result = dict(pairs)
    if len(result) != len(pairs):
        raise ValueError("duplicate source provenance field")
    return result


def read_input(path, limit):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > limit:
        raise ValueError("missing or oversized source provenance input")
    return path.read_bytes()


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def compiler_artifacts(raw, expected, *, library_build):
    artifacts = {}
    coordinates = {i.rsplit("#", 1)[1] for i in expected}
    finished = False
    worker = []
    expected_fresh = not library_build
    for line in raw.splitlines():
        if not line.startswith(b"{") or finished:
            raise ValueError("unexpected output from release Cargo build")
        event = json.loads(line, object_pairs_hook=unique)
        if event.get("reason") == "build-finished":
            finished = event["success"] is True
            if not finished:
                raise ValueError("failed release Cargo build")
            continue
        if event.get("reason") != "compiler-artifact":
            continue
        coordinate = event["package_id"]
        kinds = event["target"]["kind"]
        if (coordinate.rsplit("#", 1)[-1] in coordinates
                or coordinate.startswith("path+file:///src/rust/temporal_sources/generated/")) and coordinate not in expected:
            raise ValueError("unreviewed compiler package source")
        if coordinate in expected and kinds == ["lib"]:
            if (coordinate in artifacts or event["fresh"] is not expected_fresh
                    or event["profile"]["test"] is not False or event["profile"]["opt_level"] != "3"):
                raise ValueError("ambiguous, cached or test-only selected compilation")
            artifacts[coordinate] = event
        if event["target"]["name"] == WORKER and kinds == ["bin"] and not event["profile"]["test"]:
            worker.append(event)
    if not finished or set(artifacts) != set(expected) or len(worker) != (0 if library_build else 1):
        raise ValueError("incomplete release compiler records")
    if library_build:
        return artifacts
    binary = worker[0]
    manifest = f"{RUST_ROOT}/services/lake_snapshot/Cargo.toml"
    executable = f"{RUST_ROOT}/target-provenance/release/{WORKER}"
    if (binary["manifest_path"] != manifest or binary["fresh"] is not False
            or binary["profile"]["opt_level"] != "3" or binary["executable"] != executable or binary["executable"] not in binary["filenames"]
            or not re.fullmatch(r"path\+file:///src/rust/services/lake_snapshot#kanz-lake-snapshot@[0-9.]+",
                                binary["package_id"])):
        raise ValueError("release worker compiler output differs")
    return artifacts


def spdx_identity(sbom, name, version):
    coordinate = "pkg:cargo/" + name + "@" + version
    matches = []
    for row in sbom["packages"]:
        refs = [ref["referenceLocator"] for ref in row.get("externalRefs", []) if ref["referenceType"] == "purl"]
        # Do not silently strip a PURL qualifier that could select a different source.
        if ((row.get("name"), row.get("versionInfo")) == (name, version)
                or any(re.split(r"[?#]", unquote(value), maxsplit=1)[0] == coordinate for value in refs)):
            matches.append((row.get("name"), row.get("versionInfo"), refs))
    if matches != [(name, version, [coordinate])]:
        raise ValueError("ambiguous or missing SPDX Cargo package")


def verify_compilation(packages, lock, embedded, sbom, raw, libraries, archives):
    if {"chrono", "chrono-tz", "humantime"} & {p["name"] for p in lock["package"]}:
        raise ValueError("superseded temporal package remains in the changed workspace")
    ids = [row["packageID"] for row in packages]
    if not ids or ids != sorted(set(ids)):
        raise ValueError("reviewed Cargo identities must be unique and ordered")
    prepared = compiler_artifacts(libraries, ids, library_build=True)
    compiled = compiler_artifacts(raw, ids, library_build=False)
    if set(archives) != set(ids):
        raise ValueError("sealed release archives differ from the reviewed Cargo identities")
    selected = set()
    for row in packages:
        name, version = row["name"], row["version"]
        pair = (name, version)
        if pair in selected:
            raise ValueError("ambiguous reviewed Cargo coordinate")
        selected.add(pair)
        lock_rows = [p for p in lock["package"] if (p["name"], p["version"]) == pair]
        binary_rows = [p for p in embedded["packages"] if (p["name"], p["version"]) == pair]
        if len(lock_rows) != 1 or len(binary_rows) != 1 or binary_rows[0].get("kind", "runtime") != "runtime":
            raise ValueError("reviewed dependency absent or ambiguous in lock or runtime ELF")
        locked, actual = lock_rows[0], compiled[row["packageID"]]
        identity = row["packageID"]
        if actual != (prepared[identity] | {"fresh": True}):
            raise ValueError("release library inputs or outputs changed before the final build")
        archive = archives[identity]
        target = name.replace("-", "_")
        rlibs = [f for f in actual["filenames"] if f.endswith(".rlib")]
        if (set(archive) != {"path", "sha256"} or rlibs != [archive["path"]]
                or not re.fullmatch(rf"{RUST_ROOT}/target-provenance/release/deps/lib{re.escape(target)}-[0-9a-f]+\.rlib", archive["path"])
                or not re.fullmatch("[0-9a-f]{64}", archive["sha256"])):
            raise ValueError("release archive identity differs from the actual Cargo target")
        if actual["target"]["name"] != target:
            raise ValueError("compiled crate target differs from reviewed package")
        if name == TEMPORAL:
            if (row["packageID"] != f"{REGISTRY}#{name}@{version}" or locked.get("source") != REGISTRY
                    or binary_rows[0].get("source") != "crates.io"
                    or locked.get("checksum") != row["upstream"]["sha256"]
                    or not re.fullmatch("[0-9a-f]{64}", locked["checksum"])
                    or not actual["manifest_path"].endswith(f"/{name}-{version}/Cargo.toml")):
                raise ValueError("temporal registry dependency identity differs")
        else:
            relative = f"rust/temporal_sources/generated/{name}-{version}/Cargo.toml"
            manifest = "/src/" + relative
            package_id = f"path+file://{PurePosixPath(manifest).parent}#{name}@{version}"
            if (row["manifestPath"] != relative or row["packageID"] != package_id
                    or actual["manifest_path"] != manifest or "source" in locked or "checksum" in locked
                    or binary_rows[0].get("source") != "local"):
                raise ValueError("patched Apache origin differs across Cargo, lock and ELF")
        spdx_identity(sbom, name, version)
    if not any(name == TEMPORAL for name, _ in selected):
        raise ValueError("temporal registry package omitted from reviewed build")
    return len(selected)


def reviewed_packages(recipe, lock):
    if recipe.get("schema") != "kanz.apache-temporal-sources.v1" or not recipe["packages"]:
        raise ValueError("missing reviewed Apache source manifest")
    packages = []
    for row in recipe["packages"]:
        path = PurePosixPath("/src") / row["manifestPath"]
        package_id = f"path+file://{path.parent}#{row['name']}@{row['version']}"
        packages.append({**row, "packageID": package_id})
    registry, = [p for p in lock["package"] if p["name"] == TEMPORAL]
    version = registry["version"]
    upstream = {"kind": "registry-archive", "url": f"https://static.crates.io/crates/{TEMPORAL}/{TEMPORAL}-{version}.crate",
                "sha256": registry["checksum"]}
    packages.append({"packageID": f"{REGISTRY}#{TEMPORAL}@{version}", "name": TEMPORAL, "version": version,
                     "upstream": upstream})
    return sorted(packages, key=lambda row: row["packageID"])


def verify(directory, manifest_bytes, source, lock_sha, binary_sha, lock, embedded, sbom, collect_advisories, evaluate_advisories):
    """Consume the protected builder's sidecar. The extracted image supplies its own ELF digest."""
    if directory is None:
        raise ValueError("temporal sources require same-build compiler and source provenance")
    # BuildKit writes its SLSA attestation at the export root under provenance.json.
    directory = directory / "kanz-source-evidence"
    if directory.is_symlink() or not directory.is_dir():
        raise ValueError("missing protected builder evidence directory")
    recipe = json.loads(manifest_bytes, object_pairs_hook=unique)
    receipt_bytes = read_input(directory / "provenance.json", 1024 * 1024)
    receipt = json.loads(receipt_bytes, object_pairs_hook=unique)
    raw = read_input(directory / "compiler.jsonl", MAX_COMPILER_LOG)
    libraries = read_input(directory / "library-compiler.jsonl", MAX_COMPILER_LOG)
    archive_bytes = read_input(directory / "library-archives.json", 1024 * 1024)
    archives = json.loads(archive_bytes, object_pairs_hook=unique)
    advisory = read_input(directory / "chrono-advisories.json", 4 * 1024 * 1024)
    target = "x86_64-unknown-linux-gnu"
    expected = {"schema": SCHEMA, "service": "lake-sink", "target": target, "sourceRevision": source,
                "binarySHA256": binary_sha, "lockSHA256": lock_sha, "patchSetSHA256": sha256(manifest_bytes),
                "advisorySnapshotSHA256": sha256(advisory), "libraryCompilerSHA256": sha256(libraries)}
    if any(receipt.get(k) != v for k, v in expected.items()):
        raise ValueError("build provenance does not match this source, binary, lock or snapshot")
    snapshot = json.loads(advisory, object_pairs_hook=unique)
    registry, = [p for p in lock["package"] if p["name"] == TEMPORAL]
    evaluate_advisories(snapshot, registry["version"], registry["checksum"])
    if receipt["packages"] != reviewed_packages(recipe, lock):
        raise ValueError("compiled sources differ from reviewed source manifest")
    if receipt.get("libraryArchives") != archives:
        raise ValueError("release link reused different archive bytes")
    count = verify_compilation(receipt["packages"], lock, embedded, sbom, raw, libraries, archives)
    # Image build receipts can be cached. This independent current observation can
    # refuse an old image for a newly published finding under either package identity.
    current = collect_advisories()
    evaluate_advisories(current, registry["version"], registry["checksum"])
    current_bytes = json.dumps(current, sort_keys=True, separators=(",", ":")).encode()
    return ({**expected, "reviewedPackages": count, "compilerSHA256": sha256(raw),
             "receiptSHA256": sha256(receipt_bytes), "libraryArchivesSHA256": sha256(archive_bytes), "buildAdvisoryDBCommit": snapshot["rustsec_commit"],
             "releaseAdvisoryDBCommit": current["rustsec_commit"], "releaseAdvisorySnapshotSHA256": sha256(current_bytes)},
            current_bytes)

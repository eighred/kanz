"""Inspect the actual image, then audit its embedded Cargo inventory (#1413).

Never execute the image. This is inventory/advisory evidence, not a proof that
dependencies are nonmalicious or that native libraries are completely cataloged.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import tomllib
from urllib.parse import unquote

SERVICES = ("binance-depth", "okx-depth")
LIMIT = 8 * 1024 * 1024


def run(args, **kwargs):
    return subprocess.run(args, check=True, timeout=900, **kwargs)


def output(args, **kwargs):
    return run(args, stdout=subprocess.PIPE, text=True, **kwargs).stdout.strip()


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def read_json(path, limit=LIMIT):
    if not path.is_file() or path.stat().st_size > limit:
        raise ValueError("missing or oversized evidence")
    return json.loads(path.read_text(encoding="utf-8"))


def validate_reference(service, ref, local):
    if service not in SERVICES:
        raise ValueError("unsupported service")
    expected = r"sha256:[0-9a-f]{64}" if local else (
        r"ghcr\.io/eighred/" + re.escape(service) + r"@sha256:[0-9a-f]{64}")
    if not re.fullmatch(expected, ref):
        raise ValueError("image must be the exact service digest (or explicit local image ID)")


def validate_inventory(data, lock, service, sbom):
    packages = data.get("packages", [])
    if not isinstance(packages, list) or not packages or len(packages) > 10000:
        raise ValueError("empty or invalid embedded inventory")
    locked = {(p["name"], p["version"]) for p in lock["package"]}
    roots, runtime, seen = [], set(), set()
    for p in packages:
        pair = (p["name"], p["version"])
        if pair not in locked or pair in seen:
            raise ValueError("embedded dependency absent from lock or duplicated")
        seen.add(pair)
        if p.get("root", False):
            roots.append(p["name"])
        if p.get("kind", "runtime") == "runtime":
            runtime.add(pair)
        elif p["kind"] != "build":
            raise ValueError("unknown dependency kind")
    if roots != ["kanz-" + service]:
        raise ValueError("wrong or ambiguous root package")
    # Witnesses catch an empty/partial catalog, without pretending the lockfile
    # (which also includes other binaries and build/dev dependencies) is an SBOM.
    if not {"kanz-market-feed", "async-nats", "tokio", "prost"} <= {n for n, _ in runtime}:
        raise ValueError("missing expected runtime dependencies")
    catalog = set()
    for p in sbom.get("packages", []):
        pair = (p.get("name"), p.get("versionInfo"))
        for ref in p.get("externalRefs", []):
            locator = ref.get("referenceLocator", "")
            if ref.get("referenceType") == "purl" and locator.startswith("pkg:cargo/"):
                # Name/version must agree with the Cargo PURL, not an OS entry.
                cargo = unquote(locator[len("pkg:cargo/"):].split("?", 1)[0])
                if cargo == f"{pair[0]}@{pair[1]}":
                    catalog.add(pair)
    if not runtime <= catalog:
        raise ValueError("SPDX omits embedded runtime Cargo packages")
    return len(packages), len(runtime)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--service", required=True, choices=SERVICES)
    parser.add_argument("--image", required=True)
    parser.add_argument("--local", action="store_true", help="nonpublishing CI only: immutable Docker image ID")
    parser.add_argument("--sbom", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    validate_reference(args.service, args.image, args.local)
    root = Path(__file__).resolve().parents[2]
    lock_path = root / "kanz-rs/Cargo.lock"
    lock = tomllib.loads(lock_path.read_text())
    sbom = read_json(args.sbom, 64 * 1024 * 1024)
    destination = args.output.resolve()
    destination.mkdir(parents=True, exist_ok=False)
    if not args.local:
        run(["docker", "pull", args.image])
    image = json.loads(output(["docker", "image", "inspect", args.image]))[0]
    if not args.local and args.image not in image.get("RepoDigests", []):
        raise ValueError("pulled image digest identity mismatch")
    if args.local and image["Id"] != args.image:
        raise ValueError("local image identity mismatch")
    evidence = {"passed": False, "image": args.image, "imageId": image["Id"], "localOnly": args.local,
                "sourceSHA": output(["git", "rev-parse", "HEAD"], cwd=root),
                "lockSHA256": digest(lock_path), "sbomSHA256": digest(args.sbom),
                "tools": {"cargo-audit": "0.22.2", "rust-audit-info": "0.5.4", "cargo-auditable": "0.7.7", "syft": "1.56.0"}}
    def save_evidence():
        (destination / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
    save_evidence()
    with tempfile.TemporaryDirectory(prefix="kanz-rust-evidence-") as directory:
        temp = Path(directory)
        binary = temp / ("kanz-" + args.service)
        container = output(["docker", "create", args.image])
        try:
            run(["docker", "cp", f"{container}:/kanz-{args.service}", str(binary)])
        finally:
            run(["docker", "rm", container])
        if not binary.is_file() or binary.is_symlink():
            raise ValueError("image binary is not a regular file")
        evidence["binarySHA256"] = digest(binary)
        with (destination / "dependencies.json").open("w", encoding="utf-8") as result:
            run(["rust-audit-info", str(binary), "104857600", str(LIMIT)], stdout=result)
        counts = validate_inventory(read_json(destination / "dependencies.json"), lock, args.service, sbom)
        evidence["embeddedPackages"], evidence["runtimePackages"] = counts
        save_evidence()
        # No project/user audit.toml, Cargo configuration or credential inheritance.
        # Absolute executable paths avoid rustup lookup under the clean HOME.
        audit = shutil.which("cargo-audit")
        if not audit:
            raise ValueError("pinned cargo-audit missing")
        env = {k: os.environ[k] for k in ("PATH", "SYSTEMROOT", "SSL_CERT_FILE", "SSL_CERT_DIR") if k in os.environ}
        env.update(HOME=str(temp), CARGO_HOME=str(temp / "cargo"))
        db = temp / "advisory-db"
        common = [audit, "audit", "--db", str(db), "--deny", "warnings", "--json"]
        # Default fetch stays enabled for EACH audit; fetch/parse errors refuse.
        for name, tail in (("lock", ["--file", str(lock_path)]), ("binary", ["bin", str(binary)])):
            with (destination / f"audit-{name}.json").open("w", encoding="utf-8") as result:
                run(common + tail, cwd=temp, env=env, stdout=result)
            evidence[f"{name}AdvisoryDBCommit"] = output(["git", "-C", str(db), "rev-parse", "HEAD"], env=env)
            save_evidence()
        evidence["passed"] = True
    save_evidence()


if __name__ == "__main__":
    main()

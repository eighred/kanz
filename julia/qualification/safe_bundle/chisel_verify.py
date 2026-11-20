"""Verify the separately packaged Chisel profile against the real OCI, scanner and process oracles."""
import argparse
import base64
import gzip
import io
import json
from pathlib import Path, PurePosixPath
import re
import tarfile

from blas_owners import advisory, blob, exact_file, prior_receipt, retained_packages, scan_results
from chisel_image import DEFINITIONS, reference, sha
from qualify import required, run_corpus


PACKAGES = {"base-files", "ca-certificates", "libc6", "libssl3t64", "openssl", "tzdata", "tzdata-legacy"}
LIBC = "d821b41fc59cb4978ecd53bc83e5c964ac15d305c8d2ac4c8a35aebe55010fb9"
MODULES = {"UHC.so", "ISO-2022-CN-EXT.so", "IBM1390.so", "IBM1399.so"}
REMOVED_ROOTS = {"86a1ecba089c4a8d3bbe2734c612ba341d813e043cf9e8a862cd5c57a36bbe6b",
                 "d7a7a0fb5d7e2731d771e9484ebcdef71d5f0c3e0a2948782bc83ee0ea699ef4"}
WALL = "var/lib/chisel/manifest.wall"
STATUS = "var/lib/dpkg/status"


def names(archive):
    result = {}
    for member in archive:
        path = PurePosixPath(member.name)
        if path.is_absolute() or ".." in path.parts or path.name.startswith(".wh.") or path.as_posix() in result:
            raise ValueError("unsafe or duplicate layer member")
        result[path.as_posix()] = member
    return result


def wall(archive, members):
    from compression import zstd  # Python 3.14 standard library, only needed for the native Chisel profile.

    raw = archive.extractfile(members[WALL]).read()
    rows = [json.loads(line) for line in zstd.decompress(raw).splitlines()]
    header = rows.pop(0)
    if header != {"jsonwall": "1.0", "schema": "1.0", "count": len(rows) + 1}:
        raise ValueError("incomplete Chisel manifest")
    return rows


def records(rows, kind, key):
    selected = [r for r in rows if r.get("kind") == kind]
    result = {r[key]: r for r in selected}
    if len(result) != len(selected):
        raise ValueError("duplicate Chisel " + kind)
    return result


def control_modules(control):
    with tarfile.open(control) as archive:
        members = names(archive)
        rows = wall(archive, members)
        package = records(rows, "package", "name")["libc6"]
        if package["sha256"] != LIBC or "libc6_gconv" not in records(rows, "slice", "name"):
            raise ValueError("positive control uses another libc6 or slice")
        paths = records(rows, "path", "path")
        hashes = {}
        for name in MODULES:
            path = "usr/lib/x86_64-linux-gnu/gconv/" + name
            actual = archive.extractfile(members[path]).read()
            if not actual.startswith(b"\x7fELF") or sha(actual) != paths["/" + path]["sha256"]:
                raise ValueError("positive control converter differs from its manifest")
            hashes[name] = sha(actual)
        return package, hashes


def base_inventory(data, control):
    control_package, modules = control_modules(control)
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        members = names(archive)
        rows = wall(archive, members)
        packages = records(rows, "package", "name")
        if set(packages) != PACKAGES or packages["libc6"] != control_package:
            raise ValueError("candidate and control package identity mismatch")
        slices = records(rows, "slice", "name")
        if "libc6_libs" not in slices or any(s.startswith("libc6_gconv") for s in slices):
            raise ValueError("unexpected libc6 slices")
        paths = {p.strip("/"): r for p, r in records(rows, "path", "path").items()}
        if paths.keys() - members.keys():
            raise ValueError("Chisel manifest path missing from filesystem")
        for path, member in members.items():
            if (member.uid, member.gid) != (0, 0) or "gconv" in PurePosixPath(path).parts:
                raise ValueError("unexpected base path or ownership")
            if member.isdir():
                if path in paths and not paths[path]["path"].endswith("/"):
                    raise ValueError("manifest file replaced by directory")
                continue
            if path == STATUS and member.isfile():
                continue  # The publisher's wrapper creates this file after Chisel.
            row = paths.get(path)
            if row is None or int(row["mode"], 8) != member.mode:
                raise ValueError("undeclared base file or unexpected mode")
            if member.issym():
                if row["link"] != member.linkname:
                    raise ValueError("unexpected base symlink")
            elif path != WALL:
                if not member.isfile() and not member.islnk():
                    raise ValueError("unexpected base member type")
                digest = sha(archive.extractfile(member).read())
                if digest != row.get("final_sha256", row.get("sha256")) or digest in modules.values():
                    raise ValueError("unexpected base file bytes")
        status = archive.extractfile(members[STATUS]).read().decode()
        stanzas = [dict(line.split(": ", 1) for line in entry.splitlines()
                        if not line.startswith((" ", "\t")) and ": " in line)
                   for entry in status.strip().split("\n\n")]
        installed = {p["Package"]: (p["Version"], p["Architecture"]) for p in stanzas}
        if len(stanzas) != len(packages) or installed != {
                name: (row["version"], row["arch"]) for name, row in packages.items()}:
            raise ValueError("dpkg status differs from Chisel package identities")
        ca = archive.extractfile(members["etc/ssl/certs/ca-certificates.crt"]).read()
        anchors = {sha(base64.b64decode(x)) for x in re.findall(
            rb"-----BEGIN CERTIFICATE-----\s*(.*?)\s*-----END CERTIFICATE-----", ca, re.S)}
        if not anchors or anchors & REMOVED_ROOTS:
            raise ValueError("missing CA bundle or removed trust anchor installed")
        tz = archive.extractfile(members["usr/share/zoneinfo/tzdata.zi"]).readline().decode().strip()
        if tz != "# version " + packages["tzdata"]["version"].split("-")[0]:
            raise ValueError("timezone bytes differ from package version")
        return {"packages": packages, "controlModules": modules, "basePaths": len(members),
                "trustAnchors": len(anchors), "timezoneData": tz}


def scanner_packages(scans, packages):
    expected = {p: r["version"] for p, r in packages.items()}
    report = json.loads((scans / "image.json").read_bytes())
    os_result, = [r for r in report["Results"] if r.get("Type") == "ubuntu" and r.get("Class") == "os-pkgs"]
    trivy = {p["Name"]: p["ID"] for p in os_result["Packages"]}
    sbom = json.loads((scans / "syft.json").read_bytes())
    rows = [a for a in sbom["artifacts"] if a["type"] == "deb"]
    syft = {p["name"]: p["version"] for p in rows}
    if (len(rows) != len(expected) or len(os_result["Packages"]) != len(expected)
            or syft != expected or trivy != {name: name + "@" + version for name, version in expected.items()}):
        raise ValueError("OS scanner coverage differs from the Chisel manifest")


def verify(args):
    original_config, application = reference(args.original)
    with tarfile.open(args.image) as oci:
        selected, = json.loads(exact_file(oci, "index.json"))["manifests"]
        manifest = json.loads(blob(oci, selected))
        config = json.loads(blob(oci, manifest["config"]))
        base, *rest = manifest["layers"]
        if rest != [desc for desc, _ in application]:
            raise ValueError("application layers changed")
        for desc in rest:
            blob(oci, desc)
        raw = gzip.decompress(blob(oci, base))
    if sha(raw) != advisory.digest(args.base):
        raise ValueError("OCI base differs from the independently retained Chisel cut")
    labels = original_config["config"]["Labels"]
    source = labels["org.opencontainers.image.revision"]
    labels["org.eighred.calibration.test-chisel"] = DEFINITIONS
    expected_rootfs = ["sha256:" + sha(raw)] + original_config["rootfs"]["diff_ids"][-2:]
    if (config["config"] != original_config["config"] or config["rootfs"]["diff_ids"] != expected_rootfs
            or config["os"] != "linux" or config["architecture"] != "amd64"):
        raise ValueError("OCI runtime configuration differs from the reference")
    physical = base_inventory(raw, args.control)
    scanner_packages(args.scans, physical["packages"])
    inputs = json.loads((args.scans / "scan-inputs.json").read_bytes())
    if inputs["imageArchiveSHA256"] != advisory.digest(args.image):
        raise ValueError("the scanner did not inspect this archive")
    findings = scan_results(args.scans, selected["digest"], manifest["config"]["digest"],
                            retained_packages(prior_receipt(args.previous_receipt)), distribution="ubuntu")
    info, = json.loads(required(["docker", "image", "inspect", args.local_image]))
    if (args.local_image != info["Id"] or info["Id"] not in (selected["digest"], manifest["config"]["digest"])
            or info["RootFS"]["Layers"] != expected_rootfs):
        raise ValueError("local worker is not the inspected OCI")
    launcher = "a1ee4394ea020ab0af4f8d35aff918fb7dcbb47e16665606c33a725dec4a6535"
    runtime = run_corpus(args.local_image, source, launcher)
    return {**physical, **findings, **inputs, "manifest": selected["digest"], "runtime": runtime,
            "applicationLayer": rest[0]["digest"], "releaseQualified": False}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("original", "image", "base", "control", "previous_receipt", "scans"):
        parser.add_argument(name, type=Path)
    parser.add_argument("local_image")
    print(json.dumps(verify(parser.parse_args()), indent=2, sort_keys=True))

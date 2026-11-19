"""Compare the real application tar and exported candidate filesystem on Linux."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import posixpath
import re
import subprocess
import tarfile
import tempfile


FORBIDDEN = ("libllvm", "libjulia-codegen")
LAUNCHER = "bin/curve-worker"
GCC_LIBRARY = "lib/julia/libstdc++.so.6.0.34"
GCC_SHA256 = "18c8d1e340b784387ca1fdda0d1b90c5fe94842517a6b5151dc71387cbf409b1"
REMOVED = {"bin/julia", "lib/julia/sys.so", "lib/julia/libLLVM.so.20.1jl",
           "lib/julia/libjulia-codegen.so.1.13.3"}
ADDED = {"lib/julia/libblastrampoline.so.5", "lib/julia/libopenblas64_.0.3.30.so"}


def require_runtime_member(value):
    if any(name in value.lower() for name in FORBIDDEN):
        raise ValueError("compiler library in runtime: " + value)


def inspect_tar(archive, temporary, rootfs=False):
    application, native = {}, {}
    count = total = 0
    with tarfile.open(archive, "r:*") as stream:
        for member in stream:
            count += 1
            total += member.size
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts or count > 50000 or total > 2 << 30:
                raise ValueError("invalid filesystem inventory")
            key = path.as_posix()
            require_runtime_member(key)
            app = key.removeprefix("opt/kanz/") if rootfs else key
            in_app = not rootfs or key.startswith("opt/kanz/")
            if member.isdir():
                continue
            if in_app and app in application:
                raise ValueError("duplicate application path")
            if member.issym() or member.islnk():
                require_runtime_member(member.linkname)
                if in_app:
                    application[app] = ["symlink" if member.issym() else "hardlink", member.linkname]
                continue
            if not member.isfile():
                if in_app:
                    raise ValueError("unsupported application entry")
                continue
            with stream.extractfile(member) as raw:
                first = raw.read(4)
                elf = first == b"\x7fELF"
                if not (in_app or elf):
                    continue
                digest = hashlib.sha256(first)
                if elf:
                    with temporary.open("wb") as out:
                        out.write(first)
                        while chunk := raw.read(1 << 20):
                            digest.update(chunk)
                            out.write(chunk)
                    dynamic = subprocess.check_output(["readelf", "-dW", temporary], text=True, timeout=10)
                    entries = re.findall(r"\((NEEDED|SONAME|RUNPATH|RPATH)\)[^\n]*?\[([^\]\n]*)\]", dynamic)
                    for _, value in entries:
                        require_runtime_member(value)
                    native[key] = {"sha256": digest.hexdigest(), "bytes": member.size, "dynamic": entries}
                else:
                    while chunk := raw.read(1 << 20):
                        digest.update(chunk)
                if in_app:
                    application[app] = ["file", digest.hexdigest(), member.size]
    if LAUNCHER not in application:
        raise ValueError("compiled executable missing")
    for key, record in application.items():
        if record[0] == "file":
            continue
        seen = set()
        while record[0] != "file":
            if key in seen or record[0] != "symlink":
                raise ValueError("unsupported application link chain")
            seen.add(key)
            key = posixpath.normpath(posixpath.join(posixpath.dirname(key), record[1]))
            if key not in application:
                raise ValueError("application link does not resolve within bundle")
            record = application[key]
    return application, native


def compare(bundle, filesystem, legacy):
    with tempfile.TemporaryDirectory() as scratch:
        tmp = Path(scratch) / "native"
        expected, before = inspect_tar(bundle, tmp)
        actual, after = inspect_tar(filesystem, tmp, rootfs=True)
    if expected != actual:
        raise ValueError("final application differs from the build bundle")
    app_native = {k.removeprefix("opt/kanz/"): v for k, v in after.items() if k.startswith("opt/kanz/")}
    if app_native != before or before.get(GCC_LIBRARY, {}).get("sha256") != GCC_SHA256:
        raise ValueError("final native inventory or selected GCC runtime differs")
    previous = legacy["native"]
    added = sorted(before.keys() - previous.keys())
    removed = sorted(previous.keys() - before.keys())
    changed = sorted(k for k in before.keys() & previous.keys() if before[k]["sha256"] != previous[k])
    if set(added) != ADDED or set(removed) != REMOVED or set(changed) != {LAUNCHER, GCC_LIBRARY}:
        raise ValueError("unreviewed change from the recorded native profile")
    return {"application": app_native, "baseELFs": {k: v for k, v in after.items() if not k.startswith("opt/kanz/")},
            "previousImage": legacy["image"], "added": added, "removed": removed, "changed": changed,
            "bundleSHA256": file_digest(bundle), "filesystemSHA256": file_digest(filesystem)}


def file_digest(path):
    with path.open("rb") as src:
        return hashlib.file_digest(src, "sha256").hexdigest()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "filesystem", "output"):
        parser.add_argument(name, type=Path)
    args = parser.parse_args()
    previous = Path(__file__).with_name("previous_application.json")
    report = compare(args.bundle, args.filesystem, json.loads(previous.read_bytes()))
    with args.output.open("x", encoding="utf-8") as out:
        json.dump(report, out, sort_keys=True, indent=2)
        out.write("\n")

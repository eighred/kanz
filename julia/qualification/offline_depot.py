"""Verify and stage pinned package sources; no network or Julia execution.

This closes only the external source-package input. Runtime, native libraries,
precompilation, final image and consumer acceptance require their own evidence.
Run preparation inside the reviewed sandbox; its output stays in owned scratch.
"""

import argparse
import hashlib
import io
import json
import os
from pathlib import PurePosixPath
import re
import stat
import sys
import tarfile
import tomllib
import uuid


MAX_ARCHIVE = 8 << 20
MAX_EXPANDED = 32 << 20
MAX_MEMBER = 4 << 20
MAX_ENTRIES = 4096
SLUG_CHARS = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def crc32c(raw, seed=0):
    """CRC-32C convention used by Julia Base._crc32c, including chained input."""
    value = seed ^ 0xFFFFFFFF
    for byte in raw:
        value ^= byte
        for _ in range(8):
            value = (value >> 1) ^ (0x82F63B78 if value & 1 else 0)
    return value ^ 0xFFFFFFFF


def version_slug(package_uuid, tree):
    # Julia 1.13.3 base/loading.jl:184-205 and base/uuid.jl:39. The
    # qualification target is little-endian amd64; no host-native packing.
    value = crc32c(uuid.UUID(package_uuid).int.to_bytes(16, "little"))
    value = crc32c(bytes.fromhex(tree), value)
    result = []
    for _ in range(5):
        value, digit = divmod(value, len(SLUG_CHARS))
        result.append(SLUG_CHARS[digit])
    return "".join(result)


def package_plan(manifest, pins):
    if (set(pins) != {"schema", "juliaVersion", "manifestSHA256", "packages"}
            or pins["schema"] != "kanz-julia-package-input-v1"
            or sha256(manifest) != pins["manifestSHA256"]):
        raise ValueError("package input contract or locked manifest differs")
    doc = tomllib.loads(manifest.decode("utf-8"))
    if (doc.get("julia_version") != pins["juliaVersion"]
            or doc.get("manifest_format") != "2.1"):
        raise ValueError("Julia runtime version differs from locked input")
    selected = {}
    for name, entries in doc["deps"].items():
        if len(entries) != 1:
            raise ValueError("ambiguous manifest package entry")
        entry = entries[0]
        if any(key in entry for key in ("path", "repo-url", "repo-rev")):
            raise ValueError("unreviewed external package source")
        if "git-tree-sha1" in entry:
            selected[name] = entry
    if not 0 < len(selected) <= 8 or set(selected) != set(pins["packages"]):
        raise ValueError("external source-package closure differs")
    plan = {}
    for name, entry in selected.items():
        pin = pins["packages"][name]
        if (not re.fullmatch(r"[A-Za-z][A-Za-z0-9_]*", name)
                or set(pin) != {"uuid", "version", "treeSHA1", "archiveSHA256",
                                "archiveBytes", "entries", "expandedBytes"}
                or str(uuid.UUID(pin["uuid"])) != pin["uuid"]
                or not re.fullmatch(r"[0-9a-f]{40}", pin["treeSHA1"])
                or not re.fullmatch(r"[0-9a-f]{64}", pin["archiveSHA256"])
                or not all(type(pin[key]) is int for key in
                           ("archiveBytes", "entries", "expandedBytes"))
                or not 0 < pin["archiveBytes"] <= MAX_ARCHIVE
                or not 0 < pin["entries"] <= MAX_ENTRIES
                or not 0 < pin["expandedBytes"] <= MAX_EXPANDED
                or (entry["uuid"], entry["version"], entry["git-tree-sha1"])
                != (pin["uuid"], pin["version"], pin["treeSHA1"])):
            raise ValueError("package identity or resource bound differs")
        plan[name] = dict(pin, slug=version_slug(pin["uuid"], pin["treeSHA1"]))
    if (sum(pin["archiveBytes"] for pin in plan.values()) > MAX_ARCHIVE
            or sum(pin["expandedBytes"] for pin in plan.values()) > MAX_EXPANDED):
        raise ValueError("combined package input exceeds its bound")
    return plan


def git_tree(files):
    """Compute Git's original file modes and recursive tree identity as data."""
    root = {}
    for name, (mode, raw) in files.items():
        parts = name.split("/")
        node = root
        for part in parts[:-1]:
            child = node.setdefault(part, {})
            if not isinstance(child, dict):
                raise ValueError("package file is also a directory")
            node = child
        if parts[-1] in node:
            raise ValueError("duplicate package tree path")
        node[parts[-1]] = (mode, raw)

    def object_hash(kind, raw):
        header = kind + b" " + str(len(raw)).encode() + b"\0"
        # Outer SHA256 binds the complete compressed bytes. SHA1 here is the
        # upstream Git tree contract, not an alternative security checksum.
        return hashlib.sha1(header + raw, usedforsecurity=False).digest()

    def tree(node):
        items = []
        for name, value in node.items():
            encoded = name.encode("utf-8")
            if isinstance(value, dict):
                items.append((encoded + b"/", b"40000", encoded, tree(value)))
            else:
                mode, raw = value
                items.append((encoded, mode.encode(), encoded, object_hash(b"blob", raw)))
        payload = b"".join(mode + b" " + name + b"\0" + digest
                           for _, mode, name, digest in sorted(items))
        return object_hash(b"tree", payload)

    return tree(root).hex()


def package_files(raw, name, pin):
    if len(raw) != pin["archiveBytes"] or sha256(raw) != pin["archiveSHA256"]:
        raise ValueError("package archive size or SHA256 differs")
    files, seen, directories, total = {}, set(), set(), 0
    with tarfile.open(fileobj=io.BytesIO(raw), mode="r:gz") as archive:
        for member in archive:
            path = PurePosixPath(member.name)
            normalized = "/".join(path.parts)
            if (not path.parts or path.is_absolute() or ".." in path.parts
                    or "." in path.parts or "\\" in member.name or "\0" in member.name
                    or normalized != member.name.rstrip("/")
                    or member.name.endswith("/") and not member.isdir()
                    or normalized in seen or len(seen) >= MAX_ENTRIES
                    or not (member.isfile() or member.isdir())
                    or member.mode & 0o7000
                    or not 0 <= member.size <= MAX_MEMBER):
                raise ValueError("unsafe or ambiguous package archive member")
            seen.add(normalized)
            for part in path.parts:
                component(part)
            total += member.size
            if total > min(MAX_EXPANDED, pin["expandedBytes"]):
                raise ValueError("expanded package exceeds input bound")
            if member.isdir():
                if member.size:
                    raise ValueError("package directory carries a payload")
                directories.add(normalized)
            else:
                data = archive.extractfile(member).read(member.size + 1)
                if len(data) != member.size:
                    raise ValueError("package member size differs")
                files[normalized] = ("100755" if member.mode & 0o111 else "100644", data)
    if len(seen) != pin["entries"] or total != pin["expandedBytes"]:
        raise ValueError("package archive census differs")
    if any(path in files for path in directories):
        raise ValueError("package file/directory collision")
    for path in files:
        if any(parent.as_posix() in files for parent in PurePosixPath(path).parents
               if parent.as_posix() != "."):
            raise ValueError("package file is also an ancestor")
    if git_tree(files) != pin["treeSHA1"]:
        raise ValueError("package Git tree differs from the locked manifest")
    if "Artifacts.toml" in files or "deps/build.jl" in files:
        raise ValueError("package requires a separately qualified artifact/build input")
    if "Project.toml" not in files:
        raise ValueError("package project metadata is absent")
    project = tomllib.loads(files["Project.toml"][1].decode("utf-8"))
    if (project.get("name"), project.get("uuid"), project.get("version")) != (
            name, pin["uuid"], pin["version"]):
        raise ValueError("package project metadata contradicts the manifest")
    return files


def new_directory(parent, name):
    component(name)
    os.mkdir(name, 0o755, dir_fd=parent)
    fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
    try:
        actual = os.fstat(fd)
        named = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if (actual.st_dev, actual.st_ino) != (named.st_dev, named.st_ino):
            raise ValueError("new package input directory changed identity")
        return fd
    except BaseException:
        os.close(fd)
        raise


def component(name):
    if (not isinstance(name, str) or name in ("", ".", "..")
            or any(ord(char) < 32 or ord(char) == 127 or char in "/\\" for char in name)
            or len(name.encode("utf-8")) > 255):
        raise ValueError("unsafe package input path component")


def write_file(parent, name, raw, executable):
    component(name)
    fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                 0o600, dir_fd=parent)
    try:
        before = os.fstat(fd)
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
            raise ValueError("new package input is not a unique regular file")
        view = memoryview(raw)
        while view:
            count = os.write(fd, view)
            if count <= 0:
                raise ValueError("package input write made no progress")
            view = view[count:]
        os.fsync(fd)
        os.fchmod(fd, 0o555 if executable else 0o444)
        after = os.fstat(fd)
        named = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if (after.st_size != len(raw) or after.st_nlink != 1
                or (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino)
                or (after.st_dev, after.st_ino) != (named.st_dev, named.st_ino)):
            raise ValueError("package input changed during materialization")
    finally:
        os.close(fd)


def stage_packages(plan, archives, depot_fd):
    """The caller retains a private, empty scratch-directory descriptor."""
    root = os.fstat(depot_fd)
    if (not stat.S_ISDIR(root.st_mode) or root.st_uid != os.geteuid()
            or stat.S_IMODE(root.st_mode) & 0o077 or os.listdir(depot_fd)
            or set(archives) != set(plan)):
        raise ValueError("package scratch or archive set differs")
    # Complete data/identity validation precedes the first output mutation.
    validated = {name: package_files(archives[name], name, pin)
                 for name, pin in plan.items()}
    for name, pin in plan.items():
        if not re.fullmatch(r"[A-Za-z][A-Za-z0-9_]*", name) or not re.fullmatch(
                r"[A-Za-z0-9]{5}", pin["slug"]):
            raise ValueError("package output name or slug differs")
    packages_fd = new_directory(depot_fd, "packages")
    try:
        for name, pin in sorted(plan.items()):
            name_fd = new_directory(packages_fd, name)
            descriptors = {}
            try:
                slug_fd = new_directory(name_fd, pin["slug"])
                descriptors[""] = slug_fd
                for path, (mode, raw) in sorted(validated[name].items()):
                    parts = path.split("/")
                    prefix = ""
                    for part in parts[:-1]:
                        child = part if not prefix else prefix + "/" + part
                        if child not in descriptors:
                            descriptors[child] = new_directory(descriptors[prefix], part)
                        prefix = child
                    write_file(descriptors[prefix], parts[-1], raw, mode == "100755")
                for fd in descriptors.values():
                    os.fchmod(fd, 0o555)
                os.fchmod(name_fd, 0o555)
            finally:
                for fd in descriptors.values():
                    os.close(fd)
                os.close(name_fd)
        os.fchmod(packages_fd, 0o555)
    finally:
        os.close(packages_fd)
    return {"scope": "external-source-packages-only", "consumerAccepted": False,
            "packages": {name: {key: pin[key] for key in
                                  ("uuid", "version", "treeSHA1", "archiveSHA256", "slug")}
                         for name, pin in sorted(plan.items())}}


def read_input(parent, name, limit):
    component(name)
    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
    try:
        before = os.fstat(fd)
        if (not stat.S_ISREG(before.st_mode) or before.st_nlink != 1
                or not 0 < before.st_size <= limit):
            raise ValueError("package input is not a bounded unique regular file")
        parts, total = [], 0
        while True:
            raw = os.read(fd, min(65536, limit + 1 - total))
            if not raw:
                break
            parts.append(raw)
            total += len(raw)
            if total > limit:
                raise ValueError("package input grew beyond its bound")
        after = os.fstat(fd)
        named = os.stat(name, dir_fd=parent, follow_symlinks=False)
        if (total != before.st_size or after.st_size != total or after.st_nlink != 1
                or (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino)
                or (after.st_dev, after.st_ino) != (named.st_dev, named.st_ino)):
            raise ValueError("package input changed while reading")
        return b"".join(parts)
    finally:
        os.close(fd)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate package input JSON key")
        result[key] = value
    return result


def prepare(input_fd, depot_fd):
    """Both retained directory descriptors are opened by the trusted caller."""
    pins = json.loads(read_input(input_fd, "offline_packages.json", 65536),
                      object_pairs_hook=unique_object)
    manifest = read_input(input_fd, "Manifest.toml", 65536)
    plan = package_plan(manifest, pins)
    names = {name: name + ".tar.gz" for name in plan}
    if set(os.listdir(input_fd)) != {"Manifest.toml", "offline_packages.json", *names.values()}:
        raise ValueError("package input file inventory differs")
    archives = {name: read_input(input_fd, file, plan[name]["archiveBytes"])
                for name, file in names.items()}
    return stage_packages(plan, archives, depot_fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True, help="reviewed read-only input directory")
    parser.add_argument("--depot", required=True, help="private empty owned scratch directory")
    args = parser.parse_args()
    if sys.platform != "linux":
        raise RuntimeError("descriptor-relative preparation requires the Linux sandbox")
    input_fd = os.open(args.input, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        depot_fd = os.open(args.depot, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            print(json.dumps(prepare(input_fd, depot_fd), sort_keys=True))
        finally:
            os.close(depot_fd)
    finally:
        os.close(input_fd)


if __name__ == "__main__":
    main()

"""Synthetic joint source/VERSION cache control; never a canonical image."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
import time

from rust_release_evidence import bounded_command

COOK = "cargo auditable chef cook --locked --release --recipe-path /recipe.json -p kanz-lake-snapshot"
BUILD = 'test -n "$VERSION" && KANZ_BUILD_VERSION="$VERSION" cargo auditable build --locked --release --manifest-path Cargo.toml -p kanz-lake-snapshot'
SOURCE = "kanz-rs/services/lake-snapshot/src/main.rs"
COMMENT = b"\n// Synthetic joint source and VERSION cache qualification only.\n"
DOCKERFILE = "kanz/services/lake-sink/Dockerfile"
LIMIT = 16 << 20
PATHS = ("kanz-rs", "kanz-schemas/proto", DOCKERFILE, ".dockerignore")
SOURCE_HASHES = ("contextSHA256", "dockerfileSHA256", "sourceBeforeSHA256", "sourceAfterSHA256")


def strict(pairs):
    value = {}
    for key, item in pairs:
        if key in value: raise ValueError("duplicate JSON key")
        value[key] = item
    return value


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def progress(raw, source):
    if not raw or len(raw) > LIMIT or not raw.endswith(b"\n"):
        raise ValueError("progress size or termination differs")
    version = digest(("kanz-lake-joint-cache-probe:" + source).encode())[:40]
    commands = (("cook", COOK, True), ("build", BUILD.replace("$VERSION", version), False))
    return target_progress(raw, "rust-build", commands)


def command_name_matches(name, stage, command, canonical_counter=False):
    if not canonical_counter:
        # Preserve the original lake predicate, including its exact spacing.
        return re.fullmatch(r"\[" + re.escape(stage) + r" [1-9][0-9]*/[1-9][0-9]*\] RUN " + re.escape(command), name) is not None
    # BuildKit v0.33 prefixCommand pads the index to the decimal width of total.
    match = re.fullmatch(r"\[" + re.escape(stage) + r" ( *[1-9][0-9]*)/([1-9][0-9]*)\] RUN " + re.escape(command), name)
    if match is None:
        return False
    index, total = int(match[1]), int(match[2])
    return 1 <= index <= total and match[1] == str(index).rjust(len(match[2]))


def target_progress(raw, stage, commands, canonical_counter=False):
    """Shared closed vertex parser; each caller supplies exact owned commands."""
    if not raw or len(raw) > LIMIT or not raw.endswith(b"\n"):
        raise ValueError("progress size or termination differs")
    def owned(name):
        return any(command_name_matches(name, stage, command, canonical_counter)
                   for _, command, _ in commands)
    vertices = {}
    lines = raw.splitlines()
    if len(lines) > 20000: raise ValueError("progress event count exceeds bound")
    for line in lines:
        if not line or len(line) > 1 << 20: raise ValueError("progress line exceeds bound")
        event = json.loads(line, object_pairs_hook=strict)
        if not isinstance(event, dict) or set(event) - {"vertexes", "statuses", "logs", "warnings"}:
            raise ValueError("unexpected progress event")
        for values in event.values():
            if not isinstance(values, list) or len(values) > 10000: raise ValueError("progress collection differs")
        for vertex in event.get("vertexes", []):
            if (not isinstance(vertex, dict) or set(vertex) - {"digest", "inputs", "name", "started", "completed", "cached", "error", "progressGroup"}
                    or not re.fullmatch(r"sha256:[0-9a-f]{64}", vertex.get("digest", ""))
                    or not isinstance(vertex.get("name"), str) or len(vertex["name"]) > 4096
                    or vertex.get("error") or type(vertex.get("cached", False)) is not bool):
                raise ValueError("invalid or failed build vertex")
            key = vertex["digest"]
            old = vertices.get(key)
            if old and old["name"] != vertex["name"]: raise ValueError("vertex identity changed")
            # BuildKit v0.33 vertexStream.append forwards successive same-digest
            # observations, including restarted frontend/context operations. Only
            # our exact cook/build witnesses require immutable completed state.
            if old and owned(vertex["name"]) and old.get("completed") and old != vertex:
                raise ValueError("completed owned vertex changed")
            vertices[key] = vertex
            if len(vertices) > 4096: raise ValueError("vertex count exceeds bound")
    result = {}
    for label, command, cached in commands:
        matches = [v for v in vertices.values() if command_name_matches(v["name"], stage, command, canonical_counter)]
        if len(matches) != 1: raise ValueError("exact build vertex missing or duplicated")
        vertex = matches[0]
        if (vertex.get("cached", False) is not cached or not isinstance(vertex.get("started"), str)
                or not isinstance(vertex.get("completed"), str) or not vertex["started"] or not vertex["completed"]):
            raise ValueError("build vertex did not prove required execution/cache state")
        result[label] = vertex["digest"]
    if len(set(result.values())) != len(commands): raise ValueError("ambiguous build vertices")
    return result


def validate(report, raw, source, tree, run, attempt, image, expected_hashes):
    expected = {"schema", "source", "tree", "run", "attempt", "image", "builder", "contextSHA256", "dockerfileSHA256",
                "sourceBeforeSHA256", "sourceAfterSHA256", "versionSHA256", "progressSHA256", "cook", "build",
                "jointSourceAndVersion", "cookCached", "finalBuildExecuted", "syntheticOnly"}
    if not isinstance(report, dict) or set(report) != expected: raise ValueError("cache report shape differs")
    if (type(report["schema"]) is not int or report["schema"] != 1 or report["source"] != source or report["tree"] != tree
            or report["run"] != run or report["attempt"] != attempt or report["image"] != image
            or not re.fullmatch(r"[0-9a-f]{40}", source) or not re.fullmatch(r"[0-9a-f]{40}", tree)
            or not re.fullmatch(r"[1-9][0-9]*", run) or not re.fullmatch(r"[1-9][0-9]*", attempt)
            or not re.fullmatch(r"sha256:[0-9a-f]{64}", image)
            or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", report["builder"])):
        raise ValueError("cache report source/run/image identity differs")
    for key in ("jointSourceAndVersion", "cookCached", "finalBuildExecuted", "syntheticOnly"):
        if report[key] is not True: raise ValueError("cache control not observed")
    for key in ("contextSHA256", "dockerfileSHA256", "sourceBeforeSHA256", "sourceAfterSHA256", "versionSHA256", "progressSHA256"):
        if not re.fullmatch(r"[0-9a-f]{64}", report[key]): raise ValueError("cache hash missing")
    if set(expected_hashes) != set(SOURCE_HASHES) or any(report[key] != expected_hashes[key] for key in SOURCE_HASHES):
        raise ValueError("cache context differs from exact committed source")
    probe_version = digest(("kanz-lake-joint-cache-probe:" + source).encode())[:40]
    if report["versionSHA256"] != digest(probe_version.encode()) or probe_version == source:
        raise ValueError("synthetic VERSION differs")
    if report["sourceBeforeSHA256"] == report["sourceAfterSHA256"] or report["progressSHA256"] != digest(raw):
        raise ValueError("cache source/progress binding differs")
    if progress(raw, source) != {key: report[key] for key in ("cook", "build")}:
        raise ValueError("cache vertex binding differs")


def source_projection(hashes, original):
    if not {SOURCE, DOCKERFILE, ".dockerignore"} <= hashes.keys(): raise ValueError("incomplete source context")
    return {"contextSHA256":digest(json.dumps(hashes,sort_keys=True,separators=(",",":")).encode()),
            "dockerfileSHA256":hashes[DOCKERFILE]["sha256"],
            "sourceBeforeSHA256":digest(original), "sourceAfterSHA256":digest(original+COMMENT)}


def checked_path(name, mode, blob):
    path = Path(name)
    if mode not in ("100644", "100755") or not re.fullmatch(r"[0-9a-f]{40}", blob) or path.is_absolute() or ".." in path.parts:
        raise ValueError("unsupported tracked file")
    if not (name.startswith("kanz-rs/") or name.startswith("kanz-schemas/proto/") or name in (DOCKERFILE, ".dockerignore")):
        raise ValueError("unapproved source path")
    return path


def source_hashes(root, source, tree, deadline=None):
    """Independently read exact committed blobs; never trust artifact hash claims."""
    if not re.fullmatch(r"[0-9a-f]{40}", source) or not re.fullmatch(r"[0-9a-f]{40}", tree):
        raise ValueError("invalid source identity")
    deadline = time.monotonic()+30 if deadline is None else deadline
    def read(args, destination, limit):
        remaining = deadline-time.monotonic()
        if remaining <= 0: raise ValueError("committed source observation budget expired")
        bounded_command(args,destination,limit,timeout=min(10,remaining))
    with tempfile.TemporaryDirectory(prefix="kanz-lake-cache-source-") as owned:
        directory = Path(owned)
        bound = directory / "tree"
        read(["git","-C",str(root),"rev-parse",source+"^{tree}"],bound,128)
        if bound.read_text().strip() != tree: raise ValueError("source tree differs")
        inventory = directory / "inventory"
        read(["git","-C",str(root),"ls-tree","-r","-l","-z",source,"--",*PATHS],inventory,2<<20)
        raw = inventory.read_bytes()
        if not raw.endswith(b"\0"): raise ValueError("truncated committed inventory")
        entries = raw.split(b"\0")[:-1]
        if not entries or len(entries)>10000: raise ValueError("committed file count exceeds bound")
        hashes, total, original = {}, 0, None
        for index, entry in enumerate(entries):
            header, name = entry.decode("utf-8").split("\t",1)
            mode, kind, blob, length = header.split()
            checked_path(name,mode,blob)
            if kind != "blob" or not length.isdecimal() or int(length)>4<<20 or name in hashes:
                raise ValueError("unsupported committed blob")
            total += int(length)
            if total>128<<20: raise ValueError("committed context exceeds bound")
            target = directory / ("blob-"+str(index))
            read(["git","-C",str(root),"cat-file","blob",blob],target,4<<20)
            data = target.read_bytes()
            target.unlink()
            if len(data)!=int(length) or hashlib.sha1(b"blob "+str(len(data)).encode()+b"\0"+data).hexdigest()!=blob:
                raise ValueError("committed blob identity differs")
            hashes[name] = {"mode":mode,"blob":blob,"sha256":digest(data)}
            if name == SOURCE: original = data
        if original is None: raise ValueError("committed source missing")
        return source_projection(hashes,original)


def copy_context(root, destination):
    subprocess.run(["git", "-C", str(root), "diff", "--quiet", "HEAD", "--", *PATHS], check=True, timeout=10)
    inventory = destination / ".probe-git-inventory"
    bounded_command(["git", "-C", str(root), "ls-files", "--stage", "-z", "--", *PATHS], inventory, 2 << 20, timeout=10)
    raw = inventory.read_bytes()
    inventory.unlink()
    if not raw.endswith(b"\0"): raise ValueError("truncated tracked inventory")
    entries = raw.split(b"\0")[:-1]
    if not entries or len(entries) > 10000: raise ValueError("tracked file count exceeds bound")
    hashes, total = {}, 0
    for entry in entries:
        header, name = entry.decode("utf-8").split("\t", 1)
        mode, blob, stage = header.split()
        path = checked_path(name,mode,blob)
        if stage != "0" or name in hashes: raise ValueError("ambiguous tracked file")
        for parent in path.parents:
            if (root / parent).is_symlink(): raise ValueError("source parent is a link")
        fd = os.open(root / path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as stream:
            before = os.fstat(stream.fileno())
            if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size > 4 << 20:
                raise ValueError("source file is not bounded regular")
            data = stream.read((4 << 20) + 1)
            after = os.fstat(stream.fileno())
            identity = lambda s: (s.st_mode, s.st_nlink, s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns)
            if identity(before) != identity(after) or len(data) != before.st_size: raise ValueError("source changed during copy")
        if hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest() != blob:
            raise ValueError("source bytes differ from approved Git blob")
        total += len(data)
        if total > 128 << 20: raise ValueError("source context exceeds bound")
        target = destination / path
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open("xb") as stream: stream.write(data)
        target.chmod(int(mode[-3:], 8))
        hashes[name] = {"mode":mode,"blob":blob,"sha256":digest(data)}
    if not {SOURCE, DOCKERFILE, ".dockerignore"} <= hashes.keys(): raise ValueError("incomplete source context")
    return hashes


def main():
    if sys.platform != "linux" or not all(hasattr(os, flag) for flag in ("O_NOFOLLOW", "O_NONBLOCK")):
        raise ValueError("cache probe requires Linux file and process ownership")
    parser = argparse.ArgumentParser()
    for name in ("source", "tree", "run", "attempt", "image", "builder", "output"): parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", args.image): raise ValueError("canonical image must be immutable")
    root = Path.cwd()
    if subprocess.check_output(["git", "rev-parse", "HEAD", "HEAD^{tree}"], text=True, timeout=10).splitlines() != [args.source, args.tree]:
        raise ValueError("probe source differs")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", args.builder): raise ValueError("builder identity differs")
    if subprocess.check_output(["docker", "image", "inspect", "--format", "{{.Id}}", args.image], text=True, timeout=10).strip() != args.image:
        raise ValueError("canonical image differs")
    output = Path(args.output)
    deadline = time.monotonic()+300
    expected_hashes = source_hashes(root,args.source,args.tree,deadline)
    with tempfile.TemporaryDirectory(prefix="kanz-lake-cache-probe-") as owned:
        context = Path(owned)
        hashes = copy_context(root, context)
        original = (context / SOURCE).read_bytes()
        (context / SOURCE).write_bytes(original + COMMENT)
        version = digest(("kanz-lake-joint-cache-probe:" + args.source).encode())[:40]
        log = output / "cache-probe-progress.jsonl"
        remaining = deadline-time.monotonic()
        if remaining <= 0: raise ValueError("synthetic probe observation budget expired")
        bounded_command(["docker", "buildx", "build", "--builder", args.builder, "--target", "rust-build", "--progress=rawjson", "--output=type=cacheonly",
                         "--build-arg", "VERSION=" + version, "--file", str(context / DOCKERFILE), str(context)],
                        log, LIMIT, timeout=remaining, capture_stderr=True)
        raw = log.read_bytes()
        report = {"schema":1, "source":args.source, "tree":args.tree, "run":args.run, "attempt":args.attempt,
                  "image":args.image, "builder":args.builder, **source_projection(hashes,original),
                  "versionSHA256":digest(version.encode()), "progressSHA256":digest(raw), **progress(raw, args.source),
                  "jointSourceAndVersion":True, "cookCached":True, "finalBuildExecuted":True, "syntheticOnly":True}
        validate(report, raw, args.source, args.tree, args.run, args.attempt, args.image, expected_hashes)
    with (output / "cache-probe.json").open("x") as stream: json.dump(report, stream, sort_keys=True)


if __name__ == "__main__":
    main()

"""Inspect the actual image, then audit its embedded Cargo inventory (#1413).

Never execute the image. This is inventory/advisory evidence, not a proof that
dependencies are nonmalicious or that native libraries are completely cataloged.
"""
import argparse
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import tarfile
import threading
import signal
import sys
from pathlib import PurePosixPath
import tomllib
from urllib.parse import unquote

# A recipe is an exact image contract; hybrid acceptance additionally requires
# Go binary/source/SBOM/audit and pinned native-base evidence.
SERVICES = ("binance-depth", "okx-depth", "lake-sink")
CC_RUNTIME = "ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f"


@dataclass(frozen=True)
class ImageRecipe:
    rust_path: str
    rust_root: str
    witnesses: frozenset[str]
    go_path: str | None = None
    go_main: str | None = None
    native_base: str | None = None


def image_recipe(service):
    if service in ("binance-depth", "okx-depth"):
        root = "kanz-" + service
        return ImageRecipe("/" + root, root,
                           frozenset(("kanz-market-feed", "async-nats", "tokio", "prost")))
    if service == "lake-sink":
        return ImageRecipe("/kanz-lake-snapshot", "kanz-lake-snapshot",
                           frozenset(("datafusion", "parquet", "arrow", "tokio", "prost")),
                           "/lake-sink", "github.com/eighred/kanz/services/lake-sink/cmd/lake-sink",
                           CC_RUNTIME)
    raise ValueError("unsupported image recipe")
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
    recipe = image_recipe(service)
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
    if roots != [recipe.rust_root]:
        raise ValueError("wrong or ambiguous root package")
    # Witnesses catch an empty/partial catalog, without pretending the lockfile
    # (which also includes other binaries and build/dev dependencies) is an SBOM.
    if not recipe.witnesses <= {n for n, _ in runtime}:
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






def bounded_command(args, destination, limit, *, cwd=None, env=None, timeout=900):
    """Hybrid Linux tools have bounded output and an owned kill/join path."""
    if sys.platform != "linux" or not hasattr(os, "killpg"):
        raise ValueError("hybrid image verification requires Linux process ownership")
    failure = threading.Event()
    errors = []
    process = None
    readers = []
    def kill():
        if process is not None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
    def drain(stream, target, bound):
        total = 0
        try:
            with stream:
                while chunk := stream.read(65536):
                    total += len(chunk)
                    if total > bound:
                        failure.set()
                        kill()
                        break
                    if target is not None:
                        target.write(chunk)
        except (OSError, ValueError) as exc:
            errors.append(type(exc).__name__)
            failure.set()
            kill()
    # Refuse an unavailable destination before acquiring process ownership.
    with destination.open("xb") as target:
        try:
            process = subprocess.Popen(args, cwd=cwd, env=env, stdout=subprocess.PIPE,
                                       stderr=subprocess.PIPE, start_new_session=True)
            for stream, sink, bound in ((process.stdout, target, limit), (process.stderr, None, 65536)):
                reader = threading.Thread(target=drain, args=(stream, sink, bound), daemon=True)
                readers.append(reader)
                reader.start()
            try:
                code = process.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                raise ValueError("image tool exceeded work budget") from None
            for reader in readers:
                reader.join(timeout=5)
            if any(reader.is_alive() for reader in readers):
                raise ValueError("tool pipe cleanup exceeded observation budget")
            if failure.is_set() or errors or code:
                raise ValueError("bounded image tool refused or exceeded its budget")
        finally:
            # Covers Popen success followed by any setup, reader or wait error.
            # Nothing after this function may mark evidence passed on a failure.
            if process is not None:
                if process.poll() is None or any(reader.is_alive() for reader in readers):
                    kill()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    raise ValueError("tool termination unconfirmed") from None
                for reader in readers:
                    if reader.ident is not None:
                        reader.join(timeout=5)
                if any(reader.is_alive() for reader in readers):
                    raise ValueError("tool pipe cleanup unconfirmed")
                process.stdout.close()
                process.stderr.close()


def export_inventory(image, temp, label):
    archive = temp / (label + ".tar")
    container = output(["docker", "create", image])
    if not re.fullmatch(r"[0-9a-f]{64}", container):
        raise ValueError("invalid owned inventory container identity")
    try:
        bounded_command(["docker", "export", container], archive, 2 * 1024**3)
    finally:
        run(["docker", "rm", "-f", container])
    inventory = elf_inventory(archive)
    archive.unlink()
    return inventory


def validate_native_base_identity(base):
    fixed = image_recipe("lake-sink").native_base
    repository, digest_ref = fixed.split("@")
    # Docker RepoDigests omits the tag even when pull uses tag@digest.
    canonical = repository.removesuffix(":nonroot") + "@" + digest_ref
    if (canonical not in base.get("RepoDigests", []) or base.get("Os") != "linux" or
            base.get("Architecture") != "amd64" or not re.fullmatch(r"sha256:[0-9a-f]{64}", base.get("Id", ""))):
        raise ValueError("pinned native base identity mismatch")
    return canonical

def validate_go_buildinfo(info, expected, sums, toolchain, sbom):
    """Match binary modules to the source-selected dependency graph and SPDX."""
    recipe = image_recipe("lake-sink")
    if (info.get("Path") != recipe.go_main or info.get("GoVersion") != toolchain or
            info.get("Main") != {"Path": "github.com/eighred/kanz", "Version": "(devel)"}):
        raise ValueError("wrong Go executable identity")
    settings = {}
    for item in info.get("Settings", []):
        if set(item) != {"Key", "Value"} or item["Key"] in settings:
            raise ValueError("ambiguous Go build setting")
        settings[item["Key"]] = item["Value"]
    for key, value in {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0", "-buildmode": "exe", "-trimpath": "true"}.items():
        if settings.get(key) != value:
            raise ValueError("unexpected Go build configuration")
    if settings.get("-tags", ""):
        raise ValueError("unreviewed hybrid Go build tags")
    dependencies = info.get("Deps")
    if not isinstance(dependencies, list) or not dependencies or len(dependencies) > 4096:
        raise ValueError("missing or oversized Go module inventory")
    catalog = set()
    for package in sbom.get("packages", []):
        name, version = package.get("name"), package.get("versionInfo", "")
        if not isinstance(name, str) or not isinstance(version, str):
            continue
        locator = "pkg:golang/" + name + ("@" + version if version else "")
        if any(ref.get("referenceType") == "purl" and
               unquote(ref.get("referenceLocator", "").split("?", 1)[0]) == locator
               for ref in package.get("externalRefs", [])):
            catalog.add((name, version))
    actual = {}
    for dep in dependencies:
        name, version = dep.get("Path"), dep.get("Version")
        if not isinstance(name, str) or name in actual or not isinstance(version, str):
            raise ValueError("ambiguous Go module")
        actual[name] = dep
        if dep != expected.get(name):
            raise ValueError("binary dependency differs from source-selected module")
        replacement = dep.get("Replace")
        if replacement is not None:
            if (name != "github.com/eighred/kanz/kanz-schemas-go" or
                    version != "v0.0.0-00010101000000-000000000000" or
                    replacement != {"Path": "../kanz-schemas/gen/go", "Version": "(devel)"}):
                raise ValueError("unreviewed binary module replacement")
            # Syft1.56 keeps the original module name for a relative replacement,
            # removes (devel), and emits a versionless PURL (golang/package.go).
            pair = (name, "")
        else:
            if sums.get((name, version)) != dep.get("Sum") or not dep.get("Sum"):
                raise ValueError("Go module checksum not bound to source go.sum")
            pair = (name, version)
        if pair not in catalog:
            raise ValueError("SPDX omits actual Go module")
    if set(actual) != set(expected):
        raise ValueError("incomplete source-selected Go module inventory")
    # Main module version is deliberately not interpreted as a source commit;
    # image/source provenance and the unchanged application stamp bind those.
    return len(actual)


def json_stream(text, limit=LIMIT):
    if len(text.encode("utf-8")) > limit:
        raise ValueError("JSON stream capacity")
    decoder, offset, records = json.JSONDecoder(), 0, []
    while offset < len(text):
        while offset < len(text) and text[offset].isspace():
            offset += 1
        if offset == len(text):
            break
        item, offset = decoder.raw_decode(text, offset)
        if not isinstance(item, dict) or len(records) >= 20000:
            raise ValueError("invalid JSON stream record")
        records.append(item)
    if not records:
        raise ValueError("empty JSON stream")
    return records


def validate_go_audit(text, go_version, expected_modules):
    records = json_stream(text)
    if set(records[0]) != {"config"}:
        raise ValueError("missing first scanner configuration")
    config = records[0]["config"]
    required = {"protocol_version": "v1.0.0", "scanner_name": "govulncheck",
                "scanner_version": "v1.6.0", "scan_mode": "binary", "scan_level": "symbol",
                "db": "https://vuln.go.dev"}
    if not isinstance(config, dict) or any(config.get(k) != v for k, v in required.items()):
        raise ValueError("wrong Go vulnerability scan provenance")
    if not isinstance(config.get("db_last_modified"), str) or not config["db_last_modified"]:
        raise ValueError("missing vulnerability database observation")
    sboms, findings = [], []
    for index, record in enumerate(records):
        if len(record) != 1 or not set(record) <= {"config", "progress", "SBOM", "osv", "finding"}:
            raise ValueError("unknown scanner message")
        if index and "config" in record:
            raise ValueError("duplicate scanner configuration")
        if "SBOM" in record:
            sboms.append(record["SBOM"])
        if "finding" in record:
            finding = record["finding"]
            if not isinstance(finding, dict) or not re.fullmatch(r"GO-\d{4}-\d+", finding.get("osv", "")):
                raise ValueError("invalid scanner finding")
            findings.append(finding["osv"])
    if len(sboms) != 1 or not isinstance(sboms[0], dict):
        raise ValueError("missing or ambiguous scanner SBOM")
    sbom = sboms[0]
    if sbom.get("go_version") != go_version or sbom.get("roots") != ["github.com/eighred/kanz"]:
        raise ValueError("scanner examined another binary")
    modules = sbom.get("modules", [])
    pairs = [(m.get("path"), m.get("version", "")) for m in modules if isinstance(m, dict)]
    if len(pairs) != len(modules) or len(set(pairs)) != len(pairs) or set(pairs) != expected_modules:
        raise ValueError("scanner module inventory mismatch")
    # The separate plain-mode exit gate is decisive. JSON can include module-
    # level findings that are not reachable; preserve IDs instead of hiding them.
    return {"databaseModified": config["db_last_modified"], "findingIDs": sorted(set(findings)),
            "modules": len(pairs)}

def preflight_tar(archive):
    """Check physical headers before tarfile can allocate extended metadata."""
    total = archive.stat().st_size
    if total > 2 * 1024**3 or total % 512:
        raise ValueError("filesystem archive capacity or alignment")
    with archive.open("rb") as source:
        count, metadata_bytes = 0, 0
        while source.tell() < total:
            header = source.read(512)
            if header == bytes(512):
                if source.read(512) != bytes(512):
                    raise ValueError("missing second archive end marker")
                # No concatenated archive or hidden suffix after end-of-archive.
                while tail := source.read(1024 * 1024):
                    if any(tail):
                        raise ValueError("nonzero archive suffix")
                return
            count += 1
            if count > 100000 or len(header) != 512:
                raise ValueError("archive header capacity")
            def octal(raw):
                raw = raw.strip(b" \0")
                if not raw or any(c not in b"01234567" for c in raw):
                    raise ValueError("unsupported archive number")
                return int(raw, 8)
            size = octal(header[124:136])
            if octal(header[148:156]) != sum(header[:148]) + 8 * 32 + sum(header[156:]):
                raise ValueError("archive checksum")
            kind = header[156:157]
            if kind not in (b"\0", b"0", b"2", b"5", b"x", b"g", b"L", b"K"):
                raise ValueError("unsupported archive type")
            metadata = kind in (b"x", b"g", b"L", b"K")
            if size > (65536 if metadata else 512 * 1024**2):
                raise ValueError("archive member capacity")
            if kind in (b"2", b"5") and size:
                raise ValueError("nonregular archive body")
            padded = (size + 511) // 512 * 512
            if source.tell() + padded > total:
                raise ValueError("truncated archive body")
            if metadata:
                metadata_bytes += size
                if metadata_bytes > LIMIT:
                    raise ValueError("aggregate archive metadata capacity")
                body = source.read(size)
                if kind in (b"x", b"g"):
                    offset = 0
                    while offset < len(body):
                        space = body.find(b" ", offset)
                        length_bytes = body[offset:space] if space >= 0 else b""
                        if not length_bytes.isdigit() or len(length_bytes) > 6:
                            raise ValueError("malformed PAX length")
                        length = int(length_bytes)
                        end = offset + length
                        if end > len(body) or end <= space + 2 or body[end - 1:end] != b"\n":
                            raise ValueError("malformed PAX record")
                        pair = body[space + 1:end - 1]
                        key, separator, value = pair.partition(b"=")
                        if not separator or not key or key == b"size" or b"sparse" in key.lower():
                            raise ValueError("unsupported PAX override")
                        if key in (b"path", b"linkpath") and len(value) > 4096:
                            raise ValueError("PAX path capacity")
                        offset = end
                elif len(body) > 4096:
                    raise ValueError("GNU path capacity")
                source.seek(padded - size, 1)
            else:
                source.seek(padded, 1)
    raise ValueError("missing archive end marker")

def elf_inventory(archive):
    """Bounded, nonextracting Docker export inventory. Never follow image links."""
    if not archive.is_file() or archive.stat().st_size > 2 * 1024**3:
        raise ValueError("missing or oversized filesystem archive")
    preflight_tar(archive)
    entries, elf, links, total = set(), {}, {}, 0
    with tarfile.open(archive, mode="r:") as source:
        for member in source:
            name = member.name.removeprefix("./").rstrip("/")
            path = PurePosixPath(name)
            if (not name or path.is_absolute() or ".." in path.parts or
                    str(path) != name or name in entries or len(entries) >= 100000):
                raise ValueError("ambiguous filesystem member")
            entries.add(name)
            if member.isdir():
                continue
            if member.issym():
                if not member.linkname or len(member.linkname) > 4096:
                    raise ValueError("invalid image link")
                links["/" + name] = member.linkname
                continue
            if not member.isfile() or member.size < 0 or member.size > 512 * 1024**2:
                raise ValueError("unsupported filesystem member")
            total += member.size
            if total > 2 * 1024**3:
                raise ValueError("expanded filesystem capacity")
            stream = source.extractfile(member)
            if stream is None:
                raise ValueError("missing member body")
            with stream:
                prefix = stream.read(4)
                if prefix != b"\x7fELF":
                    continue
                hashed = hashlib.sha256(prefix)
                count = len(prefix)
                while chunk := stream.read(1024 * 1024):
                    count += len(chunk)
                    if count > member.size:
                        raise ValueError("member body exceeds declared size")
                    hashed.update(chunk)
                if count != member.size:
                    raise ValueError("truncated ELF")
            elf["/" + name] = {"sha256": hashed.hexdigest(), "size": member.size,
                               "uid": member.uid, "gid": member.gid,
                               "mode": member.mode & 0o7777,
                               "paxMetadata": {key: hashlib.sha256(value.encode("utf-8", "surrogateescape")).hexdigest()
                                   for key, value in member.pax_headers.items()
                                   if key not in ("mtime", "atime", "ctime", "path", "linkpath")}}
    if not elf:
        raise ValueError("empty ELF inventory")
    return {"elf": elf, "links": links}


def validate_hybrid_native(base, actual, go_sha, worker_sha):
    recipe = image_recipe("lake-sink")
    expected = {recipe.go_path: go_sha, recipe.rust_path: worker_sha}
    if set(base["elf"]) & set(expected):
        raise ValueError("base collides with owned application")
    if set(actual["elf"]) != set(base["elf"]) | set(expected):
        raise ValueError("unmapped or missing runtime ELF")
    for name, record in base["elf"].items():
        if actual["elf"][name] != record:
            raise ValueError("runtime ELF differs from pinned base")
    # No new loader aliases can redirect a reviewed library or application.
    if actual["links"] != base["links"]:
        raise ValueError("runtime link inventory differs from pinned base")
    for name, sha in expected.items():
        record = actual["elf"][name]
        if (not re.fullmatch(r"[0-9a-f]{64}", sha) or record["sha256"] != sha or
                record["uid"] != 0 or record["gid"] != 0 or record.get("paxMetadata") != {}):
            raise ValueError("application ELF identity or ownership mismatch")
    if actual["elf"][recipe.go_path]["mode"] != 0o755:
        raise ValueError("Go executable mode must retain 0755")
    if actual["elf"][recipe.rust_path]["mode"] != 0o555:
        raise ValueError("worker executable mode must be exactly 0555")
    return len(actual["elf"])



def generated_sdk_digest(root):
    directory = root / "kanz-schemas/gen/go"
    files = []
    total = 0
    for base, directories, names in os.walk(directory, followlinks=False):
        for name in directories:
            if (Path(base) / name).is_symlink():
                raise ValueError("generated SDK directory link")
        for name in names:
            path = Path(base) / name
            if path.suffix != ".go" and name not in ("go.mod", "go.sum"):
                continue
            stat = path.lstat()
            if path.is_symlink() or not path.is_file() or stat.st_nlink != 1:
                raise ValueError("generated SDK is not singly owned regular source")
            total += stat.st_size
            if total > 64 * 1024**2 or len(files) >= 10000:
                raise ValueError("generated SDK capacity")
            files.append((path.relative_to(directory).as_posix(), digest(path)))
    if not files or not any(name == "go.mod" for name, _ in files):
        raise ValueError("generated SDK missing")
    return hashlib.sha256(json.dumps(sorted(files), separators=(",", ":")).encode()).hexdigest()

def verify_hybrid(root, image, image_inspect, binary, temp, destination, sbom):
    recipe = image_recipe("lake-sink")
    sdk_sha = generated_sdk_digest(root)
    if (image_inspect.get("Os") != "linux" or image_inspect.get("Architecture") != "amd64" or
            image_inspect.get("Config", {}).get("Entrypoint") != ["/lake-sink"] or
            image_inspect.get("Config", {}).get("User") != "nonroot:nonroot"):
        raise ValueError("wrong hybrid runtime configuration")
    go_binary = temp / "lake-sink"
    container = output(["docker", "create", image])
    try:
        run(["docker", "cp", f"{container}:/lake-sink", str(go_binary)])
    finally:
        run(["docker", "rm", container])
    if not go_binary.is_file() or go_binary.is_symlink() or go_binary.stat().st_size > 100 * 1024**2:
        raise ValueError("invalid Go image executable")
    buildinfo = destination / "go-buildinfo.json"
    bounded_command(["go", "version", "-m", "-json", str(go_binary)], buildinfo, LIMIT)
    info = read_json(buildinfo)
    module = root / "kanz"
    toolchain_match = re.search(r"(?m)^toolchain (go[0-9.]+)$", (module / "go.mod").read_text())
    if not toolchain_match:
        raise ValueError("source Go toolchain missing")
    env = {k: os.environ[k] for k in ("PATH", "HOME", "SSL_CERT_FILE", "SSL_CERT_DIR") if k in os.environ}
    env.update(GOTOOLCHAIN="local", GOWORK="off", GOOS="linux", GOARCH="amd64", CGO_ENABLED="0", GOFLAGS="-mod=readonly")
    selected_path, graph_path = temp / "go-selected.txt", temp / "go-graph.json"
    bounded_command(["go", "list", "-deps", "-f", "{{if .Module}}{{.Module.Path}}{{end}}", recipe.go_main],
                    selected_path, LIMIT, cwd=module, env=env)
    bounded_command(["go", "list", "-m", "-json", "all"], graph_path, LIMIT, cwd=module, env=env)
    selected = set(selected_path.read_text(encoding="utf-8").split()) - {"github.com/eighred/kanz"}
    expected = {}
    for item in json_stream(graph_path.read_text(encoding="utf-8")):
        name = item.get("Path")
        if name not in selected:
            continue
        if name in expected:
            raise ValueError("duplicate source graph module")
        dep = {k: item[k] for k in ("Path", "Version", "Sum") if k in item}
        if "Replace" in item:
            dep["Replace"] = {"Path": item["Replace"]["Path"], "Version": item["Replace"].get("Version", "(devel)")}
        expected[name] = dep
    if set(expected) != selected:
        raise ValueError("source-selected module absent from graph")
    sums = {}
    for line in (module / "go.sum").read_text().splitlines():
        name, version, checksum = line.split()
        if version.endswith("/go.mod"):
            continue
        if (name, version) in sums and sums[(name, version)] != checksum:
            raise ValueError("ambiguous module checksum")
        sums[(name, version)] = checksum
    count = validate_go_buildinfo(info, expected, sums, toolchain_match[1], sbom)
    go_sha, worker_sha = digest(go_binary), digest(binary)
    run(["docker", "pull", recipe.native_base])
    base = json.loads(output(["docker", "image", "inspect", recipe.native_base]))[0]
    canonical_base = validate_native_base_identity(base)
    base_inventory = export_inventory(recipe.native_base, temp, "base")
    actual_inventory = export_inventory(image, temp, "image")
    elf_count = validate_hybrid_native(base_inventory, actual_inventory, go_sha, worker_sha)
    (destination / "native-inventory.json").write_text(json.dumps({"base": base_inventory,
        "image": actual_inventory}, sort_keys=True) + "\n", encoding="utf-8")
    scanner = shutil.which("govulncheck")
    if not scanner:
        raise ValueError("pinned Go vulnerability scanner missing")
    audit_env = {k: os.environ[k] for k in ("PATH", "SSL_CERT_FILE", "SSL_CERT_DIR") if k in os.environ}
    audit_env.update(HOME=str(temp), GOVULNDB="https://vuln.go.dev", GOTOOLCHAIN="local")
    # JSON exits zero even on vulnerabilities. Never replace this plain verdict.
    bounded_command([scanner, "-mode=binary", str(go_binary)], destination / "go-audit.txt", LIMIT,
                    cwd=temp, env=audit_env)
    audit_path = destination / "go-audit.json"
    bounded_command([scanner, "-json", "-mode=binary", str(go_binary)], audit_path, LIMIT,
                    cwd=temp, env=audit_env)
    audit_modules = {("github.com/eighred/kanz", "(devel)"), ("stdlib", info["GoVersion"])}
    for dep in info["Deps"]:
        effective = dep.get("Replace", dep)
        audit_modules.add((effective["Path"], effective["Version"]))
    audit = validate_go_audit(audit_path.read_text(encoding="utf-8"), info["GoVersion"], audit_modules)
    if digest(go_binary) != go_sha or digest(binary) != worker_sha or generated_sdk_digest(root) != sdk_sha:
        raise ValueError("extracted application changed during inspection")
    return {"goBinarySHA256": go_sha, "goModules": count, "goVersion": info["GoVersion"],
            "goModSHA256": digest(module / "go.mod"), "goSumSHA256": digest(module / "go.sum"),
            "generatedSDKSourceSHA256": sdk_sha,
            "goBuildinfoSHA256": digest(buildinfo), "goAudit": audit,
            "nativeBase": canonical_base, "nativeBaseImageId": base["Id"], "nativeELFs": elf_count,
            "nativeInventorySHA256": digest(destination / "native-inventory.json"),
            "goPlainAuditSHA256": digest(destination / "go-audit.txt"), "goJSONAuditSHA256": digest(audit_path)}

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--service", required=True, choices=SERVICES)
    parser.add_argument("--image", required=True)
    parser.add_argument("--local", action="store_true", help="nonpublishing CI only: immutable Docker image ID")
    parser.add_argument("--sbom", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    validate_reference(args.service, args.image, args.local)
    recipe = image_recipe(args.service)
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
        binary = temp / recipe.rust_root
        container = output(["docker", "create", args.image])
        try:
            run(["docker", "cp", f"{container}:{recipe.rust_path}", str(binary)])
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
        if recipe.go_path is not None:
            evidence["hybrid"] = verify_hybrid(root, args.image, image, binary, temp, destination, sbom)
            save_evidence()
        evidence["passed"] = True
    save_evidence()


if __name__ == "__main__":
    main()

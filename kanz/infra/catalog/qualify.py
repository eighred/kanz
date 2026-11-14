"""Disposable Linux catalog proof. Pull exact inputs before internal networking.

No production endpoint/configuration is accepted. Containers receive no host
credentials/socket; all listeners stay inside one internal network. Only the
CLI sees read-only catalog source and its assigned scratch directory.
"""
import argparse
import hashlib
import json
import os
import re
import resource
import secrets
import selectors
import signal
import stat
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent.parent
PIN = re.compile(r"[a-z0-9./_-]+:[A-Za-z0-9_.-]+@sha256:[0-9a-f]{64}\Z")
ROLES = ("gms", "upgrade", "mysql", "search", "broker", "cli", "tests")
CASES = ("distinct-stewards", "distinct-sensitivity", "schema-references", "lineage-edges", "cyclic-lineage-retained", "catalog-authentication", "altered-catalog-metadata", "lineage-authorization-after-alteration")
REQUIRED_TESTS = frozenset({"TestCatalogMetadataCannotGrantLineageAccess/" + name for name in ("anonymous-catalog", "anonymous-lineage", "catalog-owner-is-not-a-grant", "steward-still-authorized", "upstream-pii-still-redacted")} | {"TestCatalogMetadataCannotGrantLineageAccess", "TestCatalogFixtureExport"})
OUTPUT_LIMIT = 8 << 20
DEFAULT_FILE_LIMIT = 8 << 20
SCANNER_FILE_LIMIT = 8 << 30
SCANNER_DISK_LIMIT = 12 << 30
REFERENCE_DB_LAYERS = [
    {"repository": "mirror.gcr.io/aquasec/trivy-db:2", "compressedBytes": 140857902, "digest": "sha256:d1962eb370fc230dd1d8d31261144eaaab765192d88f4ca4e8156c803d59e8ac"},
    {"repository": "mirror.gcr.io/aquasec/trivy-java-db:1", "compressedBytes": 999527167, "digest": "sha256:7fd2ba5c26dc70e021bc57f9bd478227549e5e566c6515477a9f8e9dd9e4ef51"},
]


class CommandFailure(ValueError):
    def __init__(self, stage, category, code=None):
        self.stage, self.category, self.code = stage, category, code
        super().__init__("catalog operation refused at " + stage + ":" + category)


def command_limits(args, scanner):
    if scanner and Path(args[0]).name != "trivy":
        raise ValueError("scanner resources require the scanner command")
    return SCANNER_FILE_LIMIT if scanner else DEFAULT_FILE_LIMIT


def append_output(output, chunk):
    if len(output) + len(chunk) > OUTPUT_LIMIT:
        raise CommandFailure("capture", "output-budget")
    output.extend(chunk)


def capture_process(process, timeout):
    deadline, output = time.monotonic() + timeout, bytearray()
    with selectors.DefaultSelector() as ready:
        ready.register(process.stdout, selectors.EVENT_READ)
        while ready.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise CommandFailure("capture", "timeout")
            for key, _ in ready.select(min(remaining, 1)):
                chunk = os.read(key.fd, 64 << 10)
                if chunk:
                    append_output(output, chunk)
                else:
                    ready.unregister(key.fileobj)
    try:
        code = process.wait(timeout=max(0.01, deadline - time.monotonic()))
    except subprocess.TimeoutExpired:
        raise CommandFailure("capture", "timeout") from None
    return code, bytes(output)


def stop_process(process):
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    except OSError:
        raise CommandFailure("reap", "reap-unavailable") from None
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        raise CommandFailure("reap", "reap-timeout") from None


def failure_category(output, code, scanner):
    if not scanner:
        return "command-exit"
    diagnostic = output.decode("utf-8", errors="replace").lower()
    if code == -signal.SIGXFSZ or "file too large" in diagnostic:
        return "scanner-file-budget"
    if "no space left on device" in diagnostic or "disk quota exceeded" in diagnostic:
        return "scanner-disk-budget"
    if any(marker in diagnostic for marker in ("no such host", "network is unreachable", "connection reset", "connection refused", "tls handshake timeout", "i/o timeout", "dial tcp")):
        return "scanner-network"
    if "java db" in diagnostic or "java-db" in diagnostic or "javadb" in diagnostic:
        return "scanner-java-db-download"
    if "database" in diagnostic or "vulnerability db" in diagnostic:
        return "scanner-db-download"
    return "scanner-command-exit"


def mount_record(path):
    with open("/proc/self/mountinfo") as stream:
        body = stream.read((1 << 20) + 1)
    if len(body) > 1 << 20:
        raise ValueError("mount inventory exceeds bound")
    found = []
    for line in body.splitlines():
        left, right = line.split(" - ", 1)
        fields, filesystem = left.split(), right.split()
        target = re.sub(r"\\([0-7]{3})", lambda match: chr(int(match[1], 8)), fields[4])
        if target == str(path):
            found.append({"filesystem": filesystem[0], "source": filesystem[1], "options": fields[5].split(",")})
    if len(found) > 1:
        raise ValueError("ambiguous scanner mount")
    return found[0] if found else None


def scanner_path(owner_path):
    return owner_path.parent / "catalog-fixture" / "scanner-state"


def cleanup_scanner(owner, owner_path, run):
    mount = owner["scannerMount"]
    path = Path(mount["path"])
    if path != scanner_path(owner_path):
        raise ValueError("scanner cleanup path differs from owned path")
    mounted = mount_record(path)
    if mounted:
        if mounted["filesystem"] != mount["filesystem"] or mounted["source"] != mount["source"] or not set(mount["options"]).issubset(mounted["options"]):
            raise ValueError("foreign scanner mount cannot authorize cleanup")
        info = path.stat()
        volume = os.statvfs(path)
        if info.st_uid != mount["uid"] or info.st_gid != mount["gid"] or volume.f_frsize * volume.f_blocks != mount["bytes"]:
            raise ValueError("scanner mount owner differs")
        run(["sudo", "-n", "umount", "--", str(path)])
        if mount_record(path):
            raise ValueError("scanner mount cleanup incomplete")


def bounded_file(path, limit):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or not 0 < info.st_size <= limit:
            raise ValueError("scanner evidence file exceeds bound or is unsafe")
        body = stream.read(limit + 1)
    if len(body) > limit:
        raise ValueError("scanner evidence file exceeds bound")
    return body


def file_digest(path, limit):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        before = os.fstat(stream.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or not 0 < before.st_size <= limit:
            raise ValueError("scanner database identity/size refused")
        digest, count = hashlib.sha256(), 0
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            count += len(chunk)
            if count > limit:
                raise ValueError("scanner database exceeds bound")
            digest.update(chunk)
        after = os.fstat(stream.fileno())
        if before.st_size != count or (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
            raise ValueError("scanner database changed during observation")
    return {"bytes": count, "sha256": digest.hexdigest()}


def expected_identity():
    result = {key: os.environ[name] for key, name in (("sourceSHA", "GITHUB_SHA"), ("runID", "GITHUB_RUN_ID"), ("runAttempt", "GITHUB_RUN_ATTEMPT"))}
    if not re.fullmatch(r"[0-9a-f]{40}", result["sourceSHA"]) or any(not re.fullmatch(r"[1-9][0-9]*", result[key]) for key in ("runID", "runAttempt")):
        raise ValueError("invalid independent Actions identity")
    return result


def source_identity(run, expected):
    head = run(["git", "-C", str(ROOT), "rev-parse", "HEAD"]).strip()
    tree = run(["git", "-C", str(ROOT), "rev-parse", "HEAD^{tree}"]).strip()
    dirty = run(["git", "-C", str(ROOT), "status", "--porcelain", "--untracked-files=all"]).strip()
    if head != expected["sourceSHA"] or dirty or not re.fullmatch(r"[0-9a-f]{40}", tree):
        raise ValueError("source differs from independent Actions identity or is dirty")
    return dict(expected, treeSHA=tree)


def validate_owner(owner, identity, owner_path=None):
    if not isinstance(owner, dict) or set(owner) != {"schema", "kind", "nonce", "network", "sourceSHA", "treeSHA", "runID", "runAttempt", "scannerMount"}:
        raise ValueError("invalid catalog owner record")
    if owner["schema"] != 1 or owner["kind"] != "catalog-qualification" or not re.fullmatch(r"[0-9a-f]{16}", owner["nonce"]) or owner["network"] != "kanz-catalog-" + owner["nonce"]:
        raise ValueError("invalid catalog resource ownership")
    if any(owner[key] != identity[key] for key in ("sourceSHA", "treeSHA", "runID", "runAttempt")):
        raise ValueError("stale or foreign catalog owner")
    mount = owner["scannerMount"]
    if not isinstance(mount, dict) or set(mount) != {"path", "source", "filesystem", "bytes", "options", "uid", "gid"}:
        raise ValueError("invalid scanner mount owner")
    if not isinstance(mount["path"], str) or not Path(mount["path"]).is_absolute() or mount["source"] != owner["network"] + "-scanner" or mount["filesystem"] != "tmpfs" or mount["bytes"] != SCANNER_DISK_LIMIT or mount["options"] != ["nosuid", "nodev", "noexec"] or any(type(mount[key]) is not int or mount[key] < 0 for key in ("uid", "gid")):
        raise ValueError("invalid scanner mount identity or budget")
    if owner_path is not None and Path(mount["path"]) != scanner_path(owner_path):
        raise ValueError("scanner mount outside assigned scratch")


def write_owner(path, owner):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as output:
        output.write(json.dumps(owner, sort_keys=True))
        output.flush()
        os.fsync(output.fileno())


def read_owner(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor) as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid() or not 0 < info.st_size <= 4096:
            raise ValueError("unsafe catalog owner record")
        def unique(pairs):
            result = {}
            for key, value in pairs:
                if key in result:
                    raise ValueError("duplicate catalog owner key")
                result[key] = value
            return result
        return json.loads(stream.read(4097), object_pairs_hook=unique)


def labels(owner):
    return {"kanz.catalog.owner": owner["nonce"], "kanz.catalog.source": owner["sourceSHA"], "kanz.catalog.run": owner["runID"], "kanz.catalog.attempt": owner["runAttempt"]}


def cleanup_owned(path, identity, run):
    owner = read_owner(path)
    validate_owner(owner, identity, path)
    required = labels(owner)
    foreign = False
    # Exact names are authorized before creation. Failed starts and interrupted
    # --rm clients are included, but a preexisting foreign collision is refused.
    for role in ROLES:
        name = owner["network"] + "-" + role
        found = run(["docker", "ps", "--all", "--filter", "name=^/" + name + "$", "--format", "{{.Names}}"])
        if not found.strip():
            continue
        if found.strip() != name:
            raise ValueError("ambiguous catalog container name")
        info = json.loads(run(["docker", "inspect", "--type", "container", name]))
        if len(info) != 1 or any(info[0].get("Config", {}).get("Labels", {}).get(k) != v for k, v in required.items()):
            foreign = True
            continue
        container_id = info[0]["Id"]
        if not re.fullmatch(r"[0-9a-f]{64}", container_id):
            raise ValueError("invalid owned container ID")
        run(["docker", "rm", "--force", "--volumes", container_id])
        if run(["docker", "ps", "--all", "--filter", "id=" + container_id, "--format", "{{.ID}}"]):
            raise ValueError("owned container cleanup incomplete")
    network = owner["network"]
    found = run(["docker", "network", "ls", "--filter", "name=^" + network + "$", "--format", "{{.Name}}"])
    if found.strip():
        if found.strip() != network:
            raise ValueError("ambiguous catalog network name")
        info = json.loads(run(["docker", "network", "inspect", network]))
        if len(info) != 1 or any(info[0].get("Labels", {}).get(k) != v for k, v in required.items()):
            raise ValueError("foreign network cannot authorize cleanup")
        if not re.fullmatch(r"[0-9a-f]{64}", info[0]["Id"]):
            raise ValueError("invalid owned network ID")
        run(["docker", "network", "rm", info[0]["Id"]])
        if run(["docker", "network", "ls", "--filter", "name=^" + network + "$", "--format", "{{.Name}}"]):
            raise ValueError("owned network cleanup incomplete")
    cleanup_scanner(owner, path, run)
    if foreign:
        raise ValueError("foreign container cannot authorize cleanup")
    return owner


def validate_receipt(report, identity, images):
    if any(report.get(k) != v for k, v in identity.items()) or report.get("status") != "passed" or report.get("cleanupPassed") is not True or report.get("failure") is not None:
        raise ValueError("incomplete or mismatched catalog receipt")
    validate_owner(report.get("owner"), identity)
    if report.get("cases") != list(CASES) or report.get("selectedImages") != images or set(report.get("images", {})) != set(images) or report.get("scanFindings"):
        raise ValueError("catalog case/image/scan inventory incomplete")
    for role, reference in images.items():
        item = report["images"][role]
        if item.get("reference") != reference or not re.fullmatch(r"sha256:[0-9a-f]{64}", item.get("imageID", "")) or not re.fullmatch(r"[0-9a-f]{64}", item.get("scanSHA256", "")):
            raise ValueError("catalog executed image binding incomplete")
    if report.get("dependencies", {}).get("packages", {}).get("acryl-datahub") != "1.8.0" or report.get("projectionTests", {}).get("count") != 14:
        raise ValueError("catalog dependency/test inventory incomplete")
    if report.get("qualificationControls", {}).get("count") != 13 or report.get("requiredTests") != sorted(REQUIRED_TESTS):
        raise ValueError("catalog mandatory case evidence incomplete")
    scanner = report.get("scanner", {})
    if any(scanner.get(key) != value for key, value in {"scanner": "trivy-0.72.0", "fileLimitBytes": SCANNER_FILE_LIMIT, "cacheTempLimitBytes": SCANNER_DISK_LIMIT, "stdoutLimitBytes": OUTPUT_LIMIT, "defaultFileLimitBytes": DEFAULT_FILE_LIMIT, "commandTimeoutSeconds": 600, "releasedBeforeRuntime": True}.items()) or scanner.get("referenceObservedLayers") != REFERENCE_DB_LAYERS or set(scanner.get("databases", {})) != {"db", "java-db"}:
        raise ValueError("scanner resource/database proof incomplete")
    for name, version in (("db", 2), ("java-db", 1)):
        item = scanner["databases"][name]
        database = item.get("database", {})
        if item.get("metadata", {}).get("Version") != version or not re.fullmatch(r"[0-9a-f]{64}", item.get("metadataSHA256", "")) or not re.fullmatch(r"[0-9a-f]{64}", database.get("sha256", "")) or not isinstance(database.get("bytes"), int) or not 0 < database["bytes"] <= SCANNER_FILE_LIMIT:
            raise ValueError("scanner database observation incomplete")
    for key in ("testsSHA256", "projectionSHA256", "testBinarySHA256"):
        if not re.fullmatch(r"[0-9a-f]{64}", report.get(key, "")):
            raise ValueError("catalog evidence hash absent")


class Fixture:
    def __init__(self, scratch, binary, identity=None):
        self.scratch = scratch
        self.binary = binary
        self.identity = identity
        self.owner_path = Path(os.environ["RUNNER_TEMP"]) / "catalog-owner.json"
        if scratch != (self.owner_path.parent / "catalog-fixture").resolve():
            raise ValueError("catalog scratch differs from the assigned run directory")
        nonce = secrets.token_hex(8)
        self.network = "kanz-catalog-" + nonce
        self.owner = dict(identity or {}, schema=1, kind="catalog-qualification", nonce=nonce, network=self.network)
        self.owner["scannerMount"] = {"path": str(scanner_path(self.owner_path)), "source": self.network + "-scanner", "filesystem": "tmpfs", "bytes": SCANNER_DISK_LIMIT, "options": ["nosuid", "nodev", "noexec"], "uid": os.getuid(), "gid": os.getgid()}
        self.environment = {"PATH": os.environ["PATH"], "HOME": str(scratch / "host-home"), "DOCKER_CONFIG": str(scratch / "docker-config"), "LANG": "C.UTF-8"}
        for path in ("host-home", "docker-config", "home"):
            (scratch / path).mkdir()
        self.secret = secrets.token_hex(32)
        self.auth = "Basic __datahub_system:" + self.secret
        self.lock = json.loads((HERE / "images.json").read_text())
        self.images = self.lock["images"]
        if self.lock["platform"] != "linux/amd64" or set(self.images) != {"gms", "upgrade", "ingestion", "mysql", "search", "broker"} or not all(PIN.fullmatch(v) for v in self.images.values()):
            raise ValueError("closed image inventory differs")
        self.stage = "initialization"

    def run(self, args, timeout=180, environment=None, scanner=False):
        file_limit = command_limits(args, scanner)
        def limits():
            resource.setrlimit(resource.RLIMIT_FSIZE, (file_limit, file_limit))
            resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
        process = None
        try:
            process = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=environment or self.environment, preexec_fn=limits, start_new_session=True)
            code, output = capture_process(process, timeout)
            if code:
                raise CommandFailure(self.stage, failure_category(output, code, scanner), code)
            return output.decode("utf-8", errors="replace")
        except (CommandFailure, subprocess.TimeoutExpired) as error:
            category = error.category if isinstance(error, CommandFailure) else "timeout"
            self.last_failure = {"stage": self.stage, "category": category, "exitCode": getattr(error, "code", None)}
            raise CommandFailure(self.stage, category, getattr(error, "code", None)) from None
        except (OSError, subprocess.SubprocessError):
            self.last_failure = {"stage": self.stage, "category": "command-unavailable"}
            raise CommandFailure(self.stage, "command-unavailable") from None
        finally:
            if process is not None:
                if process.poll() is None:
                    try:
                        stop_process(process)
                    except CommandFailure as error:
                        self.last_failure = {"stage": self.stage, "category": error.category}
                        raise CommandFailure(self.stage, error.category) from None
                if process.stdout is not None:
                    process.stdout.close()

    def prepare_scanner(self):
        path = Path(self.owner["scannerMount"]["path"])
        if path != self.scratch / "scanner-state" or os.path.lexists(path) or mount_record(path):
            raise ValueError("scanner requires a fresh assigned mount directory")
        path.mkdir(mode=0o700)
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or path.is_symlink() or any(path.iterdir()):
            raise ValueError("unsafe scanner mount directory")
        mount = self.owner["scannerMount"]
        options = f"size={SCANNER_DISK_LIMIT},nosuid,nodev,noexec,mode=0700,uid={mount['uid']},gid={mount['gid']}"
        self.run(["sudo", "-n", "mount", "-t", "tmpfs", "-o", options, "--", mount["source"], str(path)])
        loaded = mount_record(path)
        volume = os.statvfs(path)
        if not loaded or loaded["filesystem"] != "tmpfs" or loaded["source"] != mount["source"] or not set(mount["options"]).issubset(loaded["options"]) or volume.f_frsize * volume.f_blocks != SCANNER_DISK_LIMIT or path.stat().st_uid != mount["uid"] or path.stat().st_gid != mount["gid"]:
            raise ValueError("scanner mount/budget readback differs")
        (path / "cache").mkdir()
        (path / "tmp").mkdir()
        return path

    def scan_images(self, identities):
        self.scanner_evidence = {"scanner": "trivy-0.72.0", "fileLimitBytes": SCANNER_FILE_LIMIT,
                                 "cacheTempLimitBytes": SCANNER_DISK_LIMIT, "stdoutLimitBytes": OUTPUT_LIMIT,
                                 "defaultFileLimitBytes": DEFAULT_FILE_LIMIT, "commandTimeoutSeconds": 600,
                                 "referenceObservedLayers": REFERENCE_DB_LAYERS,
                                 "releasedBeforeRuntime": False, "postScanUsedBytesMaximum": 0}
        try:
            self.stage = "scanner-workspace"
            path = self.prepare_scanner()
            config = path / "trivy.yaml"
            config.write_text("{}")
            environment = dict(self.environment, TMPDIR=str(path / "tmp"))
            options = ["--config", str(config), "--ignorefile", "/dev/null", "--cache-dir", str(path / "cache")]
            self.stage = "scanner-version"
            version = self.run(["trivy", "--version"], environment=environment, scanner=True)
            if not re.search(r"(?m)^Version: 0\.72\.0\s*$", version):
                raise CommandFailure(self.stage, "scanner-version-mismatch")
            self.scanner_evidence["actualVersion"] = "0.72.0"
            self.stage = "scanner-main-db-download"
            try:
                self.run(["trivy", "image", *options, "--download-db-only"], timeout=600, environment=environment, scanner=True)
            except CommandFailure as error:
                if error.category == "scanner-command-exit":
                    raise CommandFailure(self.stage, "scanner-db-download", error.code) from None
                raise
            for role, image in self.images.items():
                self.stage = "fixed-critical-image-scan:" + role
                output = path / ("scan-" + role + ".json")
                failure = None
                try:
                    self.run(["trivy", "image", *options, "--skip-db-update", "--scanners", "vuln", "--ignore-unfixed", "--severity", "CRITICAL", "--exit-code", "1", "--format", "json", "--output", str(output), image], timeout=600, environment=environment, scanner=True)
                except CommandFailure as error:
                    failure = error
                if output.exists():
                    try:
                        body = bounded_file(output, OUTPUT_LIMIT)
                        scan = json.loads(body)
                    except (ValueError, OSError):
                        if failure:
                            raise failure from None
                        raise CommandFailure(self.stage, "scanner-report-refused") from None
                    (self.scratch / output.name).write_bytes(body)
                    if any(result.get("Vulnerabilities") for result in scan.get("Results", [])):
                        self.last_failure = {"stage": self.stage, "category": "scanner-advisory", "exitCode": getattr(failure, "code", None)}
                        raise CommandFailure(self.stage, "scanner-advisory", getattr(failure, "code", None))
                    identities[role]["scanSHA256"] = hashlib.sha256(body).hexdigest()
                elif failure is None:
                    raise CommandFailure(self.stage, "scanner-report-absent")
                if failure:
                    raise failure
                usage = os.statvfs(path)
                self.scanner_evidence["postScanUsedBytesMaximum"] = max(self.scanner_evidence["postScanUsedBytesMaximum"], (usage.f_blocks - usage.f_bfree) * usage.f_frsize)
            databases = {}
            for folder, filename in (("db", "trivy.db"), ("java-db", "trivy-java.db")):
                root = path / "cache" / folder
                metadata_path = root / "metadata.json"
                metadata = json.loads(bounded_file(metadata_path, 64 << 10))
                databases[folder] = {"database": file_digest(root / filename, SCANNER_FILE_LIMIT),
                                     "metadataSHA256": hashlib.sha256(bounded_file(metadata_path, 64 << 10)).hexdigest(),
                                     "metadata": {key: metadata[key] for key in ("Version", "UpdatedAt", "NextUpdate", "DownloadedAt") if key in metadata}}
            self.scanner_evidence["databases"] = databases
            return identities
        finally:
            # Preserve available completed DB observations even when a later
            # download/image refuses. Incomplete observations stay explicit and
            # cannot turn the failed command into accepted scanner evidence.
            observed = self.scanner_evidence.setdefault("databases", {})
            for folder, filename in (("db", "trivy.db"), ("java-db", "trivy-java.db")):
                root = Path(self.owner["scannerMount"]["path"]) / "cache" / folder
                if folder not in observed and (root / "metadata.json").exists() and (root / filename).exists():
                    try:
                        body = bounded_file(root / "metadata.json", 64 << 10)
                        metadata = json.loads(body)
                        observed[folder] = {"database": file_digest(root / filename, SCANNER_FILE_LIMIT), "metadataSHA256": hashlib.sha256(body).hexdigest(), "metadata": {key: metadata[key] for key in ("Version", "UpdatedAt", "NextUpdate", "DownloadedAt") if key in metadata}}
                    except (ValueError, OSError, KeyError):
                        self.scanner_evidence["databaseObservationRefused"] = True
            previous = self.stage
            self.stage = "scanner-release"
            try:
                cleanup_scanner(self.owner, self.owner_path, self.run)
                self.scanner_evidence["releasedBeforeRuntime"] = mount_record(Path(self.owner["scannerMount"]["path"])) is None
            finally:
                self.stage = previous

    def start(self, role, options, command=(), memory="1g", entrypoint=None):
        name = self.network + "-" + role
        if self.run(["docker", "ps", "--all", "--filter", "name=^/" + name + "$", "--format", "{{.Names}}"]):
            raise ValueError("catalog container name already exists")
        args = ["docker", "run", "--detach", "--name", name, "--network", self.network,
                "--network-alias", role, "--platform", "linux/amd64", "--read-only",
                "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "256",
                "--cpus", "2", "--memory", memory, "--memory-swap", memory,
                "--ulimit", "nofile=65536:65536", "--log-opt", "max-size=1m", "--log-opt", "max-file=1", "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m"]
        if entrypoint:
            args += ["--entrypoint", entrypoint]
        args += self.label_options() + options + [self.images[role]] + list(command)
        self.run(args)
        return name

    def label_options(self):
        return [item for key, value in labels(self.owner).items() for item in ("--label", key + "=" + value)]

    def cli(self, command, metadata="metadata.json", network=True):
        child = dict(self.environment, DATAHUB_GMS_AUTHORIZATION=self.auth)
        args = ["docker", "run", "--rm", "--name", self.network + "-cli", *self.label_options(), "--platform", "linux/amd64", "--network", self.network if network else "none",
                "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
                "--memory", "1g", "--memory-swap", "1g", "--cpus", "2", "--user", f"{os.getuid()}:{os.getgid()}",
                "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m", "--mount", f"type=bind,src={HERE},dst=/catalog,readonly",
                "--mount", f"type=bind,src={ROOT / '.github/workflows/catalog-verification.yml'},dst=/catalog-workflow.yml,readonly",
                "--mount", f"type=bind,src={self.scratch},dst=/scratch", "--workdir", "/scratch",
                "--env", "HOME=/scratch/home", "--env", "PYTHONDONTWRITEBYTECODE=1", "--env", "DATAHUB_TELEMETRY_ENABLED=false",
                "--env", "DATAHUB_GMS_URL=http://gms:8080", "--env", "DATAHUB_GMS_AUTHORIZATION",
                "--env", "KANZ_CATALOG_WORKFLOW=/catalog-workflow.yml",
                "--env", "KANZ_CATALOG_METADATA_FILE=/scratch/" + metadata,
                "--entrypoint", command[0], self.images["ingestion"], *command[1:]]
        return self.run(args, timeout=240, environment=child)

    def setup(self):
        self.stage = "immutable-inputs"
        identities = {}
        for role, image in self.images.items():
            self.run(["docker", "pull", "--platform", "linux/amd64", image], timeout=600)
            info = json.loads(self.run(["docker", "image", "inspect", image]))[0]
            if info["Os"] != "linux" or info["Architecture"] != "amd64" or not any(x.endswith("@" + image.rsplit("@", 1)[1]) for x in info.get("RepoDigests", [])):
                raise ValueError("executed image identity differs")
            identities[role] = {"reference": image, "imageID": info["Id"]}
        identities = self.scan_images(identities)
        if self.run(["docker", "network", "ls", "--filter", "name=^" + self.network + "$", "--format", "{{.Name}}"]):
            raise ValueError("catalog network name already exists")
        self.run(["docker", "network", "create", "--internal", *self.label_options(), self.network])
        self.stage = "broker"
        broker = self.start("broker", ["--tmpfs", "/var/lib/redpanda/data:rw,nosuid,nodev,size=1g,uid=101,gid=101"],
                            ["redpanda", "start", "--config", "/tmp/redpanda.yaml", "--overprovisioned", "--smp", "1", "--memory", "512M", "--reserve-memory", "0M", "--node-id", "0", "--check=false", "--kafka-addr", "0.0.0.0:9092", "--advertise-kafka-addr", "broker:9092", "--set", "redpanda.auto_create_topics_enabled=true"])
        self.stage = "mysql"
        mysql = self.start("mysql", ["--user", "999:999", "--tmpfs", "/var/lib/mysql:rw,nosuid,nodev,size=1g,uid=999,gid=999", "--tmpfs", "/var/run/mysqld:rw,nosuid,nodev,size=16m,uid=999,gid=999", "--env", "MYSQL_ROOT_PASSWORD=" + self.secret, "--env", "MYSQL_ROOT_HOST=%", "--env", "MYSQL_DATABASE=datahub"], memory="1g")
        self.stage = "search"
        search = self.start("search", ["--user", "1000:1000", "--tmpfs", "/usr/share/opensearch/data:rw,nosuid,nodev,size=1g,uid=1000,gid=1000", "--tmpfs", "/usr/share/opensearch/logs:rw,nosuid,nodev,size=64m,uid=1000,gid=1000", "--env", "OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m"],
                            ["-Ediscovery.type=single-node", "-Eplugins.security.disabled=true", "-Enetwork.host=0.0.0.0", "-Enode.store.allow_mmap=false"], memory="2g", entrypoint="/usr/share/opensearch/bin/opensearch")
        self.stage = "storage-readiness"
        self.wait_exec(broker, ["rpk", "cluster", "info", "-X", "brokers=broker:9092"])
        self.wait_exec(mysql, ["mysqladmin", "ping", "--silent"])
        self.wait_exec(search, ["curl", "--fail", "--silent", "http://search:9200/_cluster/health"])
        common = {
            "EBEAN_DATASOURCE_DRIVER": "com.mysql.jdbc.Driver", "EBEAN_DATASOURCE_HOST": "mysql:3306",
            "EBEAN_DATASOURCE_URL": "jdbc:mysql://mysql:3306/datahub?useSSL=false&allowPublicKeyRetrieval=true&characterEncoding=UTF-8",
            "EBEAN_DATASOURCE_USERNAME": "root", "EBEAN_DATASOURCE_PASSWORD": self.secret,
            "ELASTICSEARCH_HOST": "search", "ELASTICSEARCH_PORT": "9200", "ELASTICSEARCH_PROTOCOL": "http",
            "ELASTICSEARCH_USE_SSL": "false", "ELASTICSEARCH_IMPLEMENTATION": "opensearch", "GRAPH_SERVICE_IMPL": "elasticsearch",
            "KAFKA_BOOTSTRAP_SERVER": "broker:9092", "KAFKA_SCHEMAREGISTRY_URL": "http://gms:8080/schema-registry/api/",
            "SCHEMA_REGISTRY_TYPE": "INTERNAL", "DATAHUB_SYSTEM_CLIENT_ID": "__datahub_system",
            "DATAHUB_SYSTEM_CLIENT_SECRET": self.secret, "DATAHUB_TOKEN_SERVICE_SIGNING_KEY": secrets.token_hex(32),
            "DATAHUB_TOKEN_SERVICE_SALT": secrets.token_hex(32), "DATAHUB_TELEMETRY_ENABLED": "false",
            "ENTITY_REGISTRY_CONFIG_PATH": "/datahub/datahub-gms/resources/entity-registry.yml",
            "JAVA_OPTS": "-Xms512m -Xmx1g", "EXTRACT_JAR_ENABLED": "false", "ENTITY_VERSIONING_ENABLED": "true",
        }
        options = [item for key, value in common.items() for item in ("--env", key + "=" + value)]
        self.stage = "system-update"
        upgrade = self.start("upgrade", options + ["--env", "DATAHUB_PRECREATE_TOPICS=true", "--env", "DATAHUB_SQL_SETUP_ENABLED=true", "--env", "CREATE_USER=false", "--env", "SCHEMA_REGISTRY_SYSTEM_UPDATE=true", "--env", "PARTITIONS=1"], ["-u", "SystemUpdate"], memory="2g")
        code = self.run(["docker", "wait", upgrade], timeout=300).strip()
        if code != "0":
            raise ValueError("catalog system update refused")
        self.stage = "gms"
        gms = self.start("gms", options + ["--env", "METADATA_SERVICE_AUTH_ENABLED=true", "--env", "MAE_CONSUMER_ENABLED=true", "--env", "MCE_CONSUMER_ENABLED=true", "--env", "PE_CONSUMER_ENABLED=true", "--env", "DEV_TOOLING_ENABLED=false"], memory="2g")
        self.wait_exec(gms, ["curl", "--fail", "--silent", "http://gms:8080/health"], timeout=300)
        return identities

    def go_tests(self, name, evidence):
        self.stage = name
        command = ["docker", "run", "--rm", "--name", self.network + "-tests", *self.label_options(), "--network", "none", "--read-only", "--cap-drop", "ALL",
                   "--security-opt", "no-new-privileges", "--pids-limit", "64", "--memory", "1g", "--memory-swap", "1g", "--cpus", "2",
                   "--user", f"{os.getuid()}:{os.getgid()}", "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m",
                   "--mount", f"type=bind,src={self.scratch},dst=/scratch",
                   "--mount", f"type=bind,src={self.binary},dst=/catalog.test,readonly",
                   "--env", "TEST_CATALOG_SCRATCH=/scratch", "--entrypoint", "/catalog.test", self.images["ingestion"],
                   "-test.v", "-test.count=1", "-test.timeout=120s", "-test.run=" + name]
        output = self.run(["go", "tool", "test2json", "-p", "github.com/eighred/kanz/services/lineage/internal/server", "-t", *command], timeout=180)
        (self.scratch / evidence).write_text(output)

    def wait_exec(self, container, args, timeout=120):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                self.run(["docker", "exec", container, *args], timeout=15)
                return
            except ValueError:
                state = json.loads(self.run(["docker", "inspect", "--format", "{{json .State}}", container]))
                if not state["Running"]:
                    raise ValueError("fixture stopped at " + self.stage) from None
                time.sleep(2)
        raise ValueError("fixture readiness exceeded bound at " + self.stage)

    def close(self):
        try:
            cleanup_owned(self.owner_path, self.identity, self.run)
            return True
        except (ValueError, OSError, KeyError):
            return False


def qualify(scratch, binary):
    fixture = Fixture(scratch, binary)
    expected = expected_identity()
    identity = source_identity(fixture.run, expected)
    fixture.identity = identity
    fixture.owner.update(identity)
    validate_owner(fixture.owner, identity, fixture.owner_path)
    write_owner(fixture.owner_path, fixture.owner)
    report = dict(identity,
              owner=fixture.owner,
              **{
              "status": "failed", "cases": [], "selectedImages": fixture.images,
              "testBinarySHA256": hashlib.sha256(binary.read_bytes()).hexdigest()})
    fatal_failure = None
    try:
        report["images"] = fixture.setup()
        fixture.stage = "locked-cli-version"
        dependencies = json.loads(fixture.cli(["python", "-c", "import importlib.metadata,json,sys; print(json.dumps({'python':sys.version.split()[0],'packages':{d.metadata['Name']:d.version for d in importlib.metadata.distributions()}}))"], network=False))
        if dependencies["packages"].get("acryl-datahub") != "1.8.0":
            raise ValueError("actual ingestion package version differs")
        report["dependencies"] = dependencies
        fixture.go_tests("^TestCatalogFixtureExport$", "export-tests.jsonl")
        fixture.stage = "projection-negative-controls"
        unit_output = fixture.cli(["python", "-B", "-m", "unittest", "discover", "-s", "/catalog", "-p", "test_*.py", "-v"], network=False)
        if not re.search(r"Ran 27 tests in", unit_output) or "skipped" in unit_output:
            raise ValueError("complete projection negative-control inventory required")
        (scratch / "projection-tests.txt").write_text(unit_output)
        report["projectionTests"] = {"count": 14, "sha256": hashlib.sha256(unit_output.encode()).hexdigest()}
        report["qualificationControls"] = {"count": 13}
        fixture.stage = "metadata-projection"
        fixture.cli(["python", "/catalog/projection.py", "--mapping", "/catalog/fixture_mapping.json", "--events", "/scratch/events.json", "--output", "/scratch/metadata.json"], network=False)
        fixture.stage = "actual-ingestion"
        fixture.cli(["datahub", "ingest", "-c", "/catalog/datahub-recipe.yaml"])
        fixture.stage = "actual-readback"
        fixture.cli(["python", "/catalog/catalog_client.py", "readback"])
        fixture.stage = "actual-metadata-alteration"
        fixture.cli(["datahub", "ingest", "-c", "/catalog/datahub-recipe.yaml"], metadata="altered.json")
        fixture.cli(["python", "/catalog/catalog_client.py", "altered"])
        fixture.go_tests(".", "server-tests.jsonl")
        report["cases"] = json.loads((scratch / "readback.json").read_text())["cases"]
        report["projectionSHA256"] = hashlib.sha256((scratch / "metadata.json").read_bytes()).hexdigest()
        required = REQUIRED_TESTS
        events = [json.loads(line) for line in (scratch / "server-tests.jsonl").read_text().splitlines()]
        if any(e.get("Action") in {"skip", "fail"} for e in events) or not required.issubset({e.get("Test") for e in events if e.get("Action") == "pass"}):
            raise ValueError("mandatory lineage cases did not pass without skips")
        report["testsSHA256"] = hashlib.sha256((scratch / "server-tests.jsonl").read_bytes()).hexdigest()
        report["requiredTests"] = sorted(required)
        report["cases"].append("lineage-authorization-after-alteration")
        report["status"] = "passed"
    except CommandFailure as error:
        fatal_failure = {"stage": error.stage, "category": error.category, "exitCode": error.code}
    except (ValueError, KeyError, TypeError, OSError):
        fatal_failure = {"stage": fixture.stage, "category": "evidence-refused"}
    finally:
        report["stage"] = fixture.stage
        report["failure"] = fatal_failure
        report["scanner"] = getattr(fixture, "scanner_evidence", None)
        report["cleanupPassed"] = fixture.close()
        if not report["cleanupPassed"]:
            report["status"] = "failed"
        report["scanFindings"] = []
        for path in scratch.glob("scan-*.json"):
            try:
                scan = json.loads(path.read_text())
            except (ValueError, OSError):
                report["status"] = "failed"
                report["scanFindings"].append({"image": fixture.images[path.stem.removeprefix("scan-")], "error": "incomplete-scanner-output"})
                continue
            for result in scan.get("Results", []):
                for item in result.get("Vulnerabilities", []):
                    report["scanFindings"].append({"image": fixture.images[path.stem.removeprefix("scan-")], "id": item["VulnerabilityID"], "package": item["PkgName"], "installed": item["InstalledVersion"], "fixed": item.get("FixedVersion", ""), "severity": item["Severity"]})
        if report["status"] == "passed":
            try:
                final_identity = source_identity(fixture.run, expected)
                validate_receipt(report, final_identity, fixture.images)
            except (ValueError, KeyError, TypeError):
                report["status"] = "failed"
                report["validationFailure"] = "final-source-or-receipt-refused"
        (scratch / "catalog-receipt.json").write_text(json.dumps(report, indent=2, sort_keys=True))
        if report["status"] != "passed":
            raise ValueError("catalog qualification or cleanup refused at " + report["stage"]) from None


def cleanup_main():
    path = Path(os.environ["RUNNER_TEMP"]) / "catalog-owner.json"
    if not os.path.lexists(path):
        return
    # Never instantiate a new ownership record to recover an interrupted one.
    # Use only the current independently checked source/run and exact old nonce.
    runner = object.__new__(Fixture)
    home = Path(os.environ["RUNNER_TEMP"]) / "catalog-cleanup-home"
    home.mkdir(mode=0o700, exist_ok=True)
    runner.environment = {"PATH": os.environ["PATH"], "HOME": str(home), "DOCKER_CONFIG": str(home), "LANG": "C.UTF-8"}
    runner.stage = "interrupted-cleanup"
    identity = source_identity(runner.run, expected_identity())
    cleanup_owned(path, identity, runner.run)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--scratch", type=Path)
    parser.add_argument("--test-binary", type=Path)
    parser.add_argument("--cleanup", action="store_true")
    args = parser.parse_args()
    if sys.platform != "linux":
        raise SystemExit("catalog qualification requires Linux")
    if args.cleanup:
        cleanup_main()
        raise SystemExit(0)
    if not args.scratch or not args.test_binary or not args.scratch.is_dir():
        raise SystemExit("catalog qualification requires assigned Linux scratch")
    if not args.test_binary.is_file() or args.test_binary.resolve().is_relative_to(args.scratch.resolve()):
        raise SystemExit("readonly test binary must be outside assigned scratch")
    qualify(args.scratch.resolve(), args.test_binary.resolve())

"""Disposable Linux catalog proof. Pull exact inputs before internal networking.

No production endpoint/configuration is accepted. Containers receive no host
credentials/socket; all listeners stay inside one internal network. Only the
CLI sees read-only catalog source and its assigned scratch directory.
"""
import argparse
import hashlib
import importlib.util
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

import image_patch
import image_identity

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
READINESS_OUTPUT_LIMIT = 64 << 10
READINESS_ATTEMPTS_RETAINED = 8
READINESS_ROLES = frozenset({"broker", "mysql", "search", "gms"})
REFERENCE_DB_LAYERS = [
    {"repository": "mirror.gcr.io/aquasec/trivy-db:2", "compressedBytes": 140857902, "digest": "sha256:d1962eb370fc230dd1d8d31261144eaaab765192d88f4ca4e8156c803d59e8ac"},
    {"repository": "mirror.gcr.io/aquasec/trivy-java-db:1", "compressedBytes": 999527167, "digest": "sha256:7fd2ba5c26dc70e021bc57f9bd478227549e5e566c6515477a9f8e9dd9e4ef51"},
]
MYSQL_USER = "584788:584788"
MYSQL_VERSION = "8.4.11-0ubuntu0.26.04.1"
MYSQL_TMPFS = {"/tmp": "rw,nosuid,nodev,size=512m",
               "/var/lib/mysql": "rw,nosuid,nodev,size=1g,uid=584788,gid=584788",
               "/run/mysqld": "rw,nosuid,nodev,size=16m,uid=584788,gid=584788"}
MYSQL_PUBLIC_ENV = {"PEBBLE": "/tmp/catalog-pebble", "PEBBLE_COPY_ONCE": "/var/lib/pebble/default",
                    "PEBBLE_PERSIST": "never", "XDG_CONFIG_HOME": "/tmp/catalog-xdg"}
MYSQL_PROBE = r'''set -eu
test "$(awk '/^Name:/{print $2}' /proc/1/status)" = pebble
pid="$(cat /run/mysqld/mysqld.pid)"
case "$pid" in ''|*[!0-9]*) exit 1 ;; esac
test "$(awk '/^Name:/{print $2}' /proc/"$pid"/status)" = mysqld
sql="$(MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --batch --skip-column-names --user=root --socket=/run/mysqld/mysqld.sock --execute='SELECT VERSION(), @@GLOBAL.skip_networking')"
tab="$(printf '\t')"
case "$sql" in *"${tab}0") ;; *) exit 1 ;; esac
awk '/^Uid:/{print "pebbleUID=" $3}/^Gid:/{print "pebbleGID=" $3}' /proc/1/status
awk '/^Uid:/{print "mysqlUID=" $3}/^Gid:/{print "mysqlGID=" $3}' /proc/"$pid"/status
printf 'sql=%s\n' "$sql"
'''


def mysql_container_profile(info, image_id, network, required_labels):
    """Project only the verified disposable profile, never inspect.Env values."""
    if not isinstance(info, dict) or not isinstance(info.get("Config"), dict) or not isinstance(info.get("HostConfig"), dict) or not isinstance(info["Config"].get("Labels"), dict):
        raise ValueError("catalog MySQL container shape refused")
    config, host = info.get("Config", {}), info.get("HostConfig", {})
    expected_env = dict(MYSQL_PUBLIC_ENV, MYSQL_ROOT_HOST="%", MYSQL_DATABASE="datahub")
    environment = config.get("Env", [])
    if not isinstance(environment, list) or any(not isinstance(value, str) for value in environment):
        raise ValueError("catalog MySQL environment shape refused")
    env_matches = all([value for value in environment if value.startswith(key + "=")] == [key + "=" + expected] for key, expected in expected_env.items())
    if info.get("Image") != image_id or config.get("User") != MYSQL_USER or config.get("Entrypoint") != ["/usr/bin/pebble", "enter"] or config.get("Cmd") not in (None, []) or any(config.get("Labels", {}).get(key) != value for key, value in required_labels.items()):
        raise ValueError("catalog MySQL image/user/vendor entrypoint refused")
    if host.get("ReadonlyRootfs") is not True or host.get("CapDrop") != ["ALL"] or host.get("CapAdd") or host.get("Privileged") is not False or host.get("SecurityOpt") != ["no-new-privileges"] or host.get("NetworkMode") != network or host.get("PortBindings") or host.get("PublishAllPorts") is not False:
        raise ValueError("catalog MySQL isolation profile refused")
    if host.get("Memory") != (1 << 30) or host.get("MemorySwap") != (1 << 30) or host.get("NanoCpus") != 2_000_000_000 or host.get("PidsLimit") != 256 or host.get("Tmpfs") != MYSQL_TMPFS or host.get("Binds") or host.get("Mounts") or any(not isinstance(mount, dict) or mount.get("Type") != "tmpfs" for mount in info.get("Mounts", [])):
        raise ValueError("catalog MySQL resources/mounts refused")
    if not env_matches or any(value.split("=", 1)[0] in {"DOCKER_CONFIG", "GITHUB_TOKEN", "GH_TOKEN", "MYSQL_RANDOM_ROOT_PASSWORD", "MYSQL_ALLOW_EMPTY_PASSWORD"} for value in environment):
        raise ValueError("catalog MySQL environment contract refused")
    return {"imageID": image_id, "user": MYSQL_USER, "vendorEntrypoint": True,
            "isolationVerified": True, "resourceLimitsVerified": True, "publicEnvironmentVerified": True}


def mysql_observation(output):
    # Closed projections retain only known public literals and effective UIDs.
    # Unrecognised output (including credentials) never enters a receipt/error.
    if not isinstance(output, str) or len(output) > 4096:
        raise ValueError("catalog MySQL public observation bound refused")
    lines = output.splitlines()
    result = {"capturedBytes": len(output.encode()), "capturedSHA256": hashlib.sha256(output.encode()).hexdigest()}
    if len(lines) != 5 or any(not re.fullmatch(key + r"=[0-9]{1,10}", line) for key, line in zip(("pebbleUID", "pebbleGID", "mysqlUID", "mysqlGID"), lines[:4])) or not lines[4].startswith("sql="):
        raise ValueError("catalog MySQL public observation shape refused")
    result.update({line.split("=", 1)[0]: int(line.split("=", 1)[1]) for line in lines[:4]})
    sql_matches = lines[4] == "sql=" + MYSQL_VERSION + "\t0"
    result.update(sqlVersion=MYSQL_VERSION if sql_matches else None, sqlVersionMatches=sql_matches, networkingEnabled=sql_matches)
    return result


def validate_mysql_runtime(evidence, image_id):
    if not isinstance(evidence, dict) or evidence.get("container") != {"imageID": image_id, "user": MYSQL_USER, "vendorEntrypoint": True, "isolationVerified": True, "resourceLimitsVerified": True, "publicEnvironmentVerified": True}:
        raise ValueError("catalog MySQL container evidence incomplete")
    observed = evidence.get("observation", {})
    if not isinstance(observed, dict) or any(observed.get(key) != 584788 for key in ("pebbleUID", "pebbleGID", "mysqlUID", "mysqlGID")) or observed.get("sqlVersion") != MYSQL_VERSION or observed.get("sqlVersionMatches") is not True or observed.get("networkingEnabled") is not True or not isinstance(observed.get("capturedBytes"), int) or not 0 < observed["capturedBytes"] <= 4096 or not re.fullmatch(r"[0-9a-f]{64}", observed.get("capturedSHA256", "")):
        raise ValueError("catalog MySQL effective identity/version evidence incomplete")


def load_registry_auth():
    # Host-only seam: /catalog unit imports never load or mount this helper's
    # private profile. The shared owner returns no credential values.
    spec = importlib.util.spec_from_file_location("catalog_registry_auth", ROOT / ".github/scripts/catalog_registry_auth.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def registry_profile(root, profile, identity, forbidden):
    profile = Path(profile)
    prefix = "catalog-registry-" + identity["sourceSHA"][:12] + "-" + identity["runID"] + "-" + identity["runAttempt"] + "-"
    if profile.parent != root or not profile.name.startswith(prefix) or not re.fullmatch(r"[0-9a-f]{16}", profile.name[len(prefix):]) or any(profile == path or profile.is_relative_to(path) or path.is_relative_to(profile) for path in forbidden):
        raise ValueError("registry profile overlaps target mounts/context or lacks run ownership")
    load_registry_auth().verify_backend(root, profile, {"source": identity["sourceSHA"], "run": identity["runID"], "attempt": identity["runAttempt"]}, authenticated=True)
    return profile


class CommandFailure(ValueError):
    def __init__(self, stage, category, code=None):
        self.stage, self.category, self.code = stage, category, code
        super().__init__("catalog operation refused at " + stage + ":" + category)


def command_limits(args, scanner):
    if scanner and Path(args[0]).name != "trivy":
        raise ValueError("scanner resources require the scanner command")
    return SCANNER_FILE_LIMIT if scanner else DEFAULT_FILE_LIMIT


def append_output(output, chunk, limit=OUTPUT_LIMIT):
    if type(limit) is not int or not 0 < limit <= OUTPUT_LIMIT or len(output) + len(chunk) > limit:
        raise CommandFailure("capture", "output-budget")
    output.extend(chunk)


def capture_process(process, timeout, limit=OUTPUT_LIMIT):
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
                    append_output(output, chunk, limit)
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
    if code == image_patch.ADVISORY_EXIT:
        # The pinned Trivy returns this code only for the requested finding
        # exit after scan/filter/report. Informational DB logs cannot mask it.
        # A complete exact-image fixed-CRITICAL report is still mandatory.
        return "scanner-advisory-exit"
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


def runtime_output(output, code, source):
    """Informational closed signals; no text can authorise readiness/cleanup."""
    if source not in {"readiness-command", "broker-startup-log"} or not isinstance(output, bytes) or len(output) > READINESS_OUTPUT_LIMIT or type(code) is not int or not -255 <= code <= 255:
        raise ValueError("runtime diagnostic source/size refused")
    diagnostic = output.decode("utf-8", errors="replace").lower()
    patterns = {
        "configuration-refused": ("unable to load config", "unable to decode config", "yaml: unmarshal", "yaml: line", "unknown configuration property"),
        "permission-refused": ("permission denied", "operation not permitted", "read-only file system"),
        "resource-refused": ("no space left on device", "file too large", "cannot allocate memory", "std::bad_alloc", "out of memory"),
        "option-refused": ("unknown flag", "unknown command", "unrecognized option", "unrecognised option"),
        "connection-refused": ("connection refused", "no brokers available", "all brokers are unreachable", "unable to dial", "i/o timeout"),
        "address-conflict": ("address already in use",),
        "startup-fatal": ("startup aborted", "startup failed", "fatal error"),
    }
    signals = sorted(key for key, markers in patterns.items() if any(marker in diagnostic for marker in markers))
    return {"informationalOnly": True, "source": source, "captureComplete": True,
            "outputLimitBytes": READINESS_OUTPUT_LIMIT, "capturedBytes": len(output),
            "capturedSHA256": hashlib.sha256(output).hexdigest(), "exitCode": code,
            "category": "multiple-signals" if len(signals) > 1 else "signal-observed" if signals else "unclassified-output",
            "matchedSignals": signals}


def runtime_refusal(category, source, code=None):
    if category not in {"capture-unavailable", "timeout", "output-budget", "command-unavailable", "reap-timeout", "reap-unavailable"} or source not in {"readiness-command", "broker-startup-log"} or code is not None and (type(code) is not int or not -255 <= code <= 255):
        raise ValueError("runtime refusal category refused")
    return {"informationalOnly": True, "source": source, "captureComplete": False,
            "outputLimitBytes": READINESS_OUTPUT_LIMIT, "capturedBytes": None,
            "capturedSHA256": None, "exitCode": code, "category": category, "matchedSignals": []}


def owned_runtime_state(info, container, image_id, network, required_labels):
    if not isinstance(info, dict) or not re.fullmatch(r"[0-9a-f]{64}", info.get("Id", "")) or info.get("Name") != "/" + container or info.get("Image") != image_id or not isinstance(info.get("Config"), dict) or not isinstance(info["Config"].get("Labels"), dict) or not isinstance(info.get("HostConfig"), dict) or info["HostConfig"].get("NetworkMode") != network or any(info["Config"]["Labels"].get(key) != value for key, value in required_labels.items()):
        raise ValueError("readiness container ownership refused")
    state = info.get("State", {})
    booleans = ("Running", "Paused", "Restarting", "OOMKilled", "Dead")
    if not isinstance(state, dict) or state.get("Status") not in {"created", "running", "paused", "restarting", "removing", "exited", "dead"} or any(type(state.get(key)) is not bool for key in booleans) or type(state.get("ExitCode")) is not int or not -255 <= state["ExitCode"] <= 255 or not isinstance(state.get("Error"), str):
        raise ValueError("readiness state shape refused")
    health = state.get("Health")
    if health is not None and (not isinstance(health, dict) or health.get("Status") not in {"starting", "healthy", "unhealthy"}):
        raise ValueError("readiness health shape refused")
    return {"containerID": info["Id"], "imageID": image_id, "ownerVerified": True,
            "status": state["Status"], **{key: state[key] for key in booleans},
            "exitCode": state["ExitCode"], "errorPresent": bool(state["Error"]),
            "healthStatus": health["Status"] if health is not None else "not-configured"}


def retain_readiness(evidence, role, record):
    if role not in READINESS_ROLES:
        raise ValueError("unselected readiness role refused")
    entry = evidence.setdefault(role, {"informationalOnly": True, "attempts": 0, "recent": []})
    entry["attempts"] += 1
    entry["recent"] = (entry["recent"] + [record])[-READINESS_ATTEMPTS_RETAINED:]


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
    if not isinstance(owner, dict) or set(owner) != {"schema", "kind", "nonce", "network", "sourceSHA", "treeSHA", "runID", "runAttempt", "scannerMount", "derivedImage", "registryProfile", "registryHelperSHA256", "registryControlsSHA256", "buildDiagnosticControlsSHA256"}:
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
    # Derive the only authorized build identity from this source and run. No
    # recorded arbitrary path, image/tag or package can authorize deletion.
    expected_path = owner_path or Path(mount["path"]).parent.parent / "catalog-owner.json"
    if owner["derivedImage"] != image_patch.owned_spec(owner, expected_path):
        raise ValueError("derived image source/package/owner differs")
    profile = Path(owner["registryProfile"])
    prefix = "catalog-registry-" + identity["sourceSHA"][:12] + "-" + identity["runID"] + "-" + identity["runAttempt"] + "-"
    if profile.parent != expected_path.parent or not profile.name.startswith(prefix) or not re.fullmatch(r"[0-9a-f]{16}", profile.name[len(prefix):]) or any(not re.fullmatch(r"[0-9a-f]{64}", owner[key]) for key in ("registryHelperSHA256", "registryControlsSHA256", "buildDiagnosticControlsSHA256")):
        raise ValueError("registry source/run ownership differs")


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
    derived_cleanup = image_patch.cleanup(owner["derivedImage"], run)
    if foreign:
        raise ValueError("foreign container cannot authorize cleanup")
    return {"owner": owner, "derivedImage": derived_cleanup}


def validate_receipt(report, identity, images, registry_pending=False):
    if any(report.get(k) != v for k, v in identity.items()) or report.get("status") != "passed" or report.get("cleanupPassed") is not True or report.get("failure") is not None:
        raise ValueError("incomplete or mismatched catalog receipt")
    validate_owner(report.get("owner"), identity)
    registry = report.get("registryAuth", {})
    if registry.get("verifiedBeforeTargets") is not True or registry.get("helperSHA256") != report["owner"]["registryHelperSHA256"] or registry.get("controlsSHA256") != report["owner"]["registryControlsSHA256"] or registry.get("controlsCount") != 7:
        raise ValueError("registry isolation/control evidence incomplete")
    if report.get("buildDiagnosticControls") != {"count": 1, "sha256": report["owner"]["buildDiagnosticControlsSHA256"]}:
        raise ValueError("pre-authentication diagnostic control evidence incomplete")
    if not registry_pending:
        cleanup = report.get("registryCleanup", {})
        result = cleanup.get("cleanup", {})
        if report.get("acceptanceComplete") is not True or cleanup.get("sourceVerified") is not True or any(cleanup.get(key) != value for key, value in identity.items()) or cleanup.get("profile") != report["owner"]["registryProfile"] or any(result.get(key) is not True for key in ("ownershipVerified", "fileBackend", "credentialsAbsent", "directoryRemoved", "passed")) or result.get("logout") != {"attempted": True, "exitCode": 0}:
            raise ValueError("registry credential absence/cleanup proof incomplete")
    if report.get("cases") != list(CASES) or report.get("selectedImages") != images or set(report.get("images", {})) != set(images) or report.get("scanFindings"):
        raise ValueError("catalog case/image/scan inventory incomplete")
    if "mysql" in images:
        validate_mysql_runtime(report.get("mysqlRuntime", {}), report["images"]["mysql"].get("imageID"))
    for role, reference in images.items():
        item = report["images"][role]
        selected = image_identity.selections()[role]
        if item.get("descriptor") != dict(selected, rawManifestVerified=True, rawConfigVerified=True) or item.get("identity", {}).get("engineID") != item.get("imageID"):
            raise ValueError("catalog raw descriptor/engine receipt incomplete")
        if role != "ingestion":
            image_identity.verify_projected(item["identity"], selected)
        executed = item.get("imageID") if role == "ingestion" else reference
        if item.get("reference") != executed or not re.fullmatch(r"sha256:[0-9a-f]{64}", item.get("imageID", "")) or not re.fullmatch(r"[0-9a-f]{64}", item.get("scanSHA256", "")) or item.get("scanVerdict") != "passed":
            raise ValueError("catalog executed image binding incomplete")
    if "ingestion" in images:
        selected = image_identity.selections()["ingestion"]
        item = report["images"]["ingestion"]
        derived = item.get("derived", {})
        if derived.get("imageID") != item["imageID"] or derived.get("baseReference") != images["ingestion"] or derived.get("originalBehaviorPreserved") is not True or derived.get("labels") != report["owner"]["derivedImage"]["labels"] or any(derived.get(key) != report["owner"]["derivedImage"][key] for key in ("baseConfigDigest", "baseManifestDigest", "descriptorLockSHA256", "packageSHA256", "dockerfileSHA256", "renderedDockerfileSHA256", "verifierSHA256", "lockSHA256")) or derived["baseImageID"] == item["imageID"] or not re.fullmatch(r"[0-9a-f]{64}", derived.get("behaviorSHA256", "")):
            raise ValueError("catalog base/derived image distinction incomplete")
        image_identity.verify_projected(derived.get("baseIdentity", {}), selected)
        if derived["baseIdentity"]["engineID"] != derived["baseImageID"] or item["identity"] != {"engineID": item["imageID"], "engineIDKind": "owned-build", "os": "linux", "architecture": "amd64", "rootFSType": "layers", "rootFSDiffIDs": derived.get("rootFSDiffIDs")} or not derived["rootFSDiffIDs"][:len(selected["rootFSDiffIDs"])] == selected["rootFSDiffIDs"] or len(derived["rootFSDiffIDs"]) <= len(selected["rootFSDiffIDs"]):
            raise ValueError("derived/base ordered rootfs evidence differs")
        image_patch.validate_cleanup(report.get("derivedCleanup", {}), derived)
        image_patch.validate_build(derived.get("build"))
        if derived.get("packageInput") != image_patch.lock():
            raise ValueError("catalog package provenance selection differs")
        image_patch.validate_proof(report.get("packagePatch", {}), report.get("dependencies", {}))
        preparation = report.get("preparation", {})
        observed = preparation.get("base", {})
        package = preparation.get("package", {})
        selected_package = image_patch.lock()
        if observed.get("imageID") != derived["baseImageID"] or observed.get("engineIDKind") != derived["baseIdentity"]["engineIDKind"] or observed.get("onBuildCount") != 0 or observed.get("onBuildEmpty") is not True or observed.get("selectedManifestDigestMatches") is not True or package.get("category") != "passed" or package.get("receivedBytes") != selected_package["bytes"] or package.get("receivedSHA256") != selected_package["sha256"] or type(package.get("httpStatus")) is not int or not 200 <= package["httpStatus"] < 300:
            raise ValueError("catalog public preparation evidence incomplete")
    if report.get("dependencies", {}).get("packages", {}).get("acryl-datahub") != image_patch.lock()["cliPackageVersion"] or report.get("projectionTests", {}).get("count") != 14:
        raise ValueError("catalog dependency/test inventory incomplete")
    if report.get("qualificationControls", {}).get("count") != 35 or report.get("requiredTests") != sorted(REQUIRED_TESTS):
        raise ValueError("catalog mandatory case evidence incomplete")
    scanner = report.get("scanner", {})
    if any(scanner.get(key) != value for key, value in {"scanner": "trivy-0.72.0", "fileLimitBytes": SCANNER_FILE_LIMIT, "cacheTempLimitBytes": SCANNER_DISK_LIMIT, "stdoutLimitBytes": OUTPUT_LIMIT, "defaultFileLimitBytes": DEFAULT_FILE_LIMIT, "commandTimeoutSeconds": 600, "releasedBeforeRuntime": True, "advisoryRoles": [], "advisoryExitCode": image_patch.ADVISORY_EXIT}.items()) or scanner.get("referenceObservedLayers") != REFERENCE_DB_LAYERS or set(scanner.get("databases", {})) != {"db", "java-db"}:
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
        current = expected_identity()
        profile = registry_profile(self.owner_path.parent, os.environ["DOCKER_CONFIG"], current,
                                   (scratch, self.owner_path.parent / (self.network + "-image-context"), HERE, binary, ROOT / ".github/workflows/catalog-verification.yml"))
        controls = bounded_file(self.owner_path.parent / "catalog-registry-controls.txt", OUTPUT_LIMIT)
        if not re.search(rb"Ran 7 tests in", controls) or b"skipped" in controls or not re.search(rb"(?m)^OK\s*$", controls):
            raise ValueError("complete shared registry control inventory required")
        self.owner.update(registryProfile=str(profile), registryHelperSHA256=hashlib.sha256((ROOT / ".github/scripts/catalog_registry_auth.py").read_bytes()).hexdigest(), registryControlsSHA256=hashlib.sha256(controls).hexdigest())
        diagnostic_controls = bounded_file(self.owner_path.parent / "catalog-build-diagnostics.txt", OUTPUT_LIMIT)
        if not re.search(rb"Ran 1 test in", diagnostic_controls) or b"skipped" in diagnostic_controls or not re.search(rb"(?m)^OK\s*$", diagnostic_controls):
            raise ValueError("pre-authentication build diagnostic control required")
        self.owner["buildDiagnosticControlsSHA256"] = hashlib.sha256(diagnostic_controls).hexdigest()
        self.environment = {"PATH": os.environ["PATH"], "HOME": str(scratch / "host-home"), "DOCKER_CONFIG": str(profile), "LANG": "C.UTF-8"}
        for path in ("host-home", "home"):
            (scratch / path).mkdir()
        self.secret = secrets.token_hex(32)
        self.auth = "Basic __datahub_system:" + self.secret
        self.lock = json.loads((HERE / "images.json").read_text())
        self.source_images = dict(self.lock["images"])
        self.images = dict(self.source_images)
        if self.lock["platform"] != "linux/amd64" or set(self.images) != {"gms", "upgrade", "ingestion", "mysql", "search", "broker"} or not all(PIN.fullmatch(v) for v in self.images.values()):
            raise ValueError("closed image inventory differs")
        self.stage = "initialization"

    def prepare_ingestion(self, base):
        self.preparation_evidence = {}
        self.stage = "fixed-ingestion-selection"
        record = self.owner["derivedImage"]
        if record != image_patch.owned_spec(self.owner, self.owner_path) or record["baseReference"] != self.source_images["ingestion"]:
            raise ValueError("fixed package base/source binding differs")
        self.stage = "fixed-ingestion-base"
        self.preparation_evidence["base"] = image_patch.observe_base(base, record)
        base_identity = image_patch.validate_base(base, record)
        self.stage = "fixed-ingestion-context-absence"
        context = Path(record["context"])
        if os.path.lexists(context):
            raise ValueError("fixed package requires a fresh private build context")
        self.stage = "fixed-ingestion-context-create"
        context.mkdir(mode=0o700)
        context_info = context.lstat()
        if context.is_symlink() or not stat.S_ISDIR(context_info.st_mode) or context_info.st_uid != os.getuid() or stat.S_IMODE(context_info.st_mode) != 0o700:
            raise ValueError("fixed package context owner/mode differs")
        self.stage = "fixed-ingestion-context-inputs"
        for name in ("verify_patch.py", "package-lock.json"):
            (context / name).write_bytes((image_patch.HERE / name).read_bytes())
        (context / "Dockerfile").write_bytes(image_patch.render_dockerfile(record["labels"]))
        self.stage = "fixed-ingestion-package-download"
        self.preparation_evidence["package"] = {}
        body = image_patch.download(image_patch.lock(), evidence=self.preparation_evidence["package"])
        self.stage = "fixed-ingestion-package-write"
        (context / "linux-libc-dev.deb").write_bytes(body)
        self.stage = "fixed-ingestion-docker-version"
        versions = json.loads(self.run(["docker", "version", "--format", '{"client":{{json .Client.Version}},"server":{{json .Server.Version}},"clientAPI":{{json .Client.APIVersion}},"serverAPI":{{json .Server.APIVersion}}}']))
        build = dict(image_patch.BUILD, docker=versions)
        image_patch.validate_build(build)
        self.preparation_evidence["build"] = build
        iid = context / "derived.iid"
        # Classic build uses the existing daemon without another builder
        # service. The base is already pulled and only public inputs
        # enter this fresh context; no tag or dependency network is accepted.
        self.stage = "fixed-ingestion-launch-record"
        image_patch.write_marker(record, "started", base=base_identity)
        self.stage = "offline-ingestion-build"
        self.run(["docker", "build", "--pull=false", "--network=none", "--no-cache", "--force-rm", "--memory", "1g", "--memory-swap", "1g", "--cpu-quota", "200000", "--cpu-period", "100000", "--iidfile", str(iid), str(context)], timeout=300, environment=dict(self.environment, DOCKER_BUILDKIT="0"), build=True)
        image_id = bounded_file(iid, 128).decode().strip()
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
            raise ValueError("derived ingestion ID absent")
        derived = json.loads(self.run(["docker", "image", "inspect", image_id]))[0]
        evidence = image_patch.validate_derived(base, derived, record)
        filters = [part for key, value in record["labels"].items() for part in ("--filter", "label=" + key + "=" + value)]
        ids = set(self.run(["docker", "image", "ls", "--all", "--no-trunc", *filters, "--format", "{{.ID}}"]).splitlines())
        if len(ids) > 16:
            raise ValueError("build image chain exceeds bound")
        chain = image_patch.image_chain({value: json.loads(self.run(["docker", "image", "inspect", value]))[0] for value in ids}, record, base_identity["engineID"])
        if not chain or chain[0] != image_id:
            raise ValueError("returned build ID differs from exact owned chain")
        image_patch.write_marker(record, "completed", image_id, chain, base=base_identity)
        evidence["imageChain"] = chain
        evidence["build"] = build
        evidence["packageInput"] = image_patch.lock()
        self.images["ingestion"] = image_id
        return {"reference": image_id, "imageID": image_id, "identity": image_identity.derived_engine(derived), "derived": evidence}

    def run(self, args, timeout=180, environment=None, scanner=False, build=False, output_limit=OUTPUT_LIMIT, runtime_source=None):
        if build and (args[:2] != ["docker", "build"] or scanner):
            raise ValueError("build diagnostics require the fixed Docker build seam")
        if type(output_limit) is not int or not 0 < output_limit <= OUTPUT_LIMIT:
            raise ValueError("command output bound refused")
        if runtime_source is not None and (scanner or build or output_limit != READINESS_OUTPUT_LIMIT or runtime_source not in {"readiness-command", "broker-startup-log"} or runtime_source == "readiness-command" and args[:2] != ["docker", "exec"] or runtime_source == "broker-startup-log" and args[:4] != ["docker", "logs", "--tail", "40"]):
            raise ValueError("runtime diagnostic command seam refused")
        if runtime_source is not None:
            self.last_runtime_result = runtime_refusal("capture-unavailable", runtime_source)
        file_limit = command_limits(args, scanner)
        def limits():
            resource.setrlimit(resource.RLIMIT_FSIZE, (file_limit, file_limit))
            resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
        process, capture_complete = None, False
        try:
            process = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=environment or self.environment, preexec_fn=limits, start_new_session=True)
            code, output = capture_process(process, timeout, output_limit)
            capture_complete = True
            if runtime_source is not None:
                self.last_runtime_result = runtime_output(output, code, runtime_source)
            if build:
                self.preparation_evidence["build"]["diagnostic"] = image_patch.build_diagnostic(output, code)
            if code:
                raise CommandFailure(self.stage, self.preparation_evidence["build"]["diagnostic"]["category"] if build else failure_category(output, code, scanner), code)
            return output.decode("utf-8", errors="replace")
        except (CommandFailure, subprocess.TimeoutExpired) as error:
            category = error.category if isinstance(error, CommandFailure) else "timeout"
            if runtime_source is not None and not capture_complete:
                self.last_runtime_result = runtime_refusal(category, runtime_source, getattr(error, "code", None))
            if build and not capture_complete:
                self.preparation_evidence["build"]["diagnostic"] = {"category": category, "informationalOnly": True, "processCaptureComplete": False, "capturedBytes": None, "capturedSHA256": None, "exitCode": None}
            self.last_failure = {"stage": self.stage, "category": category, "exitCode": getattr(error, "code", None)}
            raise CommandFailure(self.stage, category, getattr(error, "code", None)) from None
        except (OSError, subprocess.SubprocessError):
            if runtime_source is not None:
                self.last_runtime_result = runtime_refusal("command-unavailable", runtime_source)
            if build:
                self.preparation_evidence["build"]["diagnostic"] = {"category": "command-unavailable", "informationalOnly": True, "processCaptureComplete": False, "capturedBytes": None, "capturedSHA256": None, "exitCode": None}
            self.last_failure = {"stage": self.stage, "category": "command-unavailable"}
            raise CommandFailure(self.stage, "command-unavailable") from None
        finally:
            if process is not None:
                try:
                    # The direct child may have exited while a descendant still
                    # owns its stdout. A failed capture must kill/reap the owned
                    # process group regardless of the parent's exit status.
                    if not capture_complete or process.poll() is None:
                        stop_process(process)
                except CommandFailure as error:
                    if runtime_source is not None:
                        self.last_runtime_result = runtime_refusal(error.category, runtime_source)
                    self.last_failure = {"stage": self.stage, "category": error.category}
                    raise CommandFailure(self.stage, error.category) from None
                finally:
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
                                 "advisoryExitCode": image_patch.ADVISORY_EXIT,
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
                    self.run(["trivy", "image", *options, "--image-src", "docker", "--skip-db-update", "--scanners", "vuln", "--ignore-unfixed", "--severity", "CRITICAL", "--exit-code", str(image_patch.ADVISORY_EXIT), "--format", "json", "--output", str(output), image], timeout=600, environment=environment, scanner=True)
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
                    advisory = image_patch.validate_scan(scan, image, identities[role]["identity"])
                    identities[role]["scanSHA256"] = hashlib.sha256(body).hexdigest()
                    identities[role]["scanVerdict"] = "advisory" if advisory else "passed"
                    # Only a complete structured fixed-CRITICAL report and
                    # the advisory exit code may continue to the next image.
                    # Network/DB/resource failures still stop immediately.
                    if advisory and failure and failure.category == "scanner-advisory-exit" and failure.code == image_patch.ADVISORY_EXIT:
                        failure = None
                    elif advisory and failure is None:
                        raise CommandFailure(self.stage, "scanner-report-exit-mismatch")
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
            refused = image_patch.scan_verdicts(identities, self.source_images)
            self.scanner_evidence["advisoryRoles"] = refused
            if refused:
                self.stage = "fixed-critical-image-scan:all-six"
                raise CommandFailure(self.stage, "scanner-advisory", image_patch.ADVISORY_EXIT)
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

    def start_mysql(self):
        # Keep Canonical's generated Pebble plan and vendor initialization.
        # COPY_ONCE seeds that read-only plan into the existing bounded /tmp;
        # neither the image configuration nor its package graph is changed.
        options = ["--user", MYSQL_USER]
        for path in ("/var/lib/mysql", "/run/mysqld"):
            options += ["--tmpfs", path + ":" + MYSQL_TMPFS[path]]
        for key, value in MYSQL_PUBLIC_ENV.items():
            options += ["--env", key + "=" + value]
        options += ["--env", "MYSQL_ROOT_PASSWORD=" + self.secret, "--env", "MYSQL_ROOT_HOST=%", "--env", "MYSQL_DATABASE=datahub"]
        return self.start("mysql", options, memory="1g")

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
        self.image_identities = identities
        for role, image in self.images.items():
            self.stage = "immutable-descriptors:" + role
            selected = image_identity.fetch(image_identity.selections()[role])
            self.stage = "immutable-inputs:" + role
            self.run(["docker", "pull", "--platform", "linux/amd64", image], timeout=600)
            info = json.loads(self.run(["docker", "image", "inspect", image]))[0]
            identity = image_identity.verify_engine(info, selected)
            identities[role] = {"reference": image, "imageID": info["Id"], "identity": identity, "descriptor": selected}
            if role == "ingestion":
                base = info
        descriptor = identities["ingestion"]["descriptor"]
        identities["ingestion"] = dict(self.prepare_ingestion(base), descriptor=descriptor)
        self.image_identities = identities
        identities = self.scan_images(identities)
        if self.run(["docker", "network", "ls", "--filter", "name=^" + self.network + "$", "--format", "{{.Name}}"]):
            raise ValueError("catalog network name already exists")
        self.run(["docker", "network", "create", "--internal", *self.label_options(), self.network])
        self.stage = "broker"
        broker = self.start("broker", ["--tmpfs", "/var/lib/redpanda/data:rw,nosuid,nodev,size=1g,uid=101,gid=101"],
                            ["redpanda", "start", "--config", "/tmp/redpanda.yaml", "--overprovisioned", "--smp", "1", "--memory", "512M", "--reserve-memory", "0M", "--node-id", "0", "--check=false", "--kafka-addr", "0.0.0.0:9092", "--advertise-kafka-addr", "broker:9092", "--set", "redpanda.auto_create_topics_enabled=true"])
        self.stage = "mysql"
        mysql = self.start_mysql()
        self.mysql_evidence = {"container": mysql_container_profile(json.loads(self.run(["docker", "inspect", mysql]))[0], identities["mysql"]["imageID"], self.network, labels(self.owner))}
        self.stage = "search"
        search = self.start("search", ["--user", "1000:1000", "--tmpfs", "/usr/share/opensearch/data:rw,nosuid,nodev,size=1g,uid=1000,gid=1000", "--tmpfs", "/usr/share/opensearch/logs:rw,nosuid,nodev,size=64m,uid=1000,gid=1000", "--env", "OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m"],
                            ["-Ediscovery.type=single-node", "-Eplugins.security.disabled=true", "-Enetwork.host=0.0.0.0", "-Enode.store.allow_mmap=false"], memory="2g", entrypoint="/usr/share/opensearch/bin/opensearch")
        self.stage = "storage-readiness"
        self.wait_exec(broker, ["rpk", "cluster", "info", "-X", "brokers=broker:9092"])
        # mysqladmin can pass for the temporary socket-only init server. Wait
        # for the authenticated final server, then verify both effective users.
        self.stage = "mysql-runtime"
        observed = self.wait_exec(mysql, ["bash", "-ec", MYSQL_PROBE])
        self.mysql_evidence["observation"] = mysql_observation(observed)
        validate_mysql_runtime(self.mysql_evidence, identities["mysql"]["imageID"])
        self.stage = "storage-readiness"
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

    def readiness_state(self, container, selector):
        role = next((value for value in READINESS_ROLES if container == self.network + "-" + value), None)
        if role is None or selector != container and not re.fullmatch(r"[0-9a-f]{64}", selector):
            raise ValueError("unowned readiness name refused")
        inspected = json.loads(self.run(["docker", "inspect", selector], timeout=10, output_limit=READINESS_OUTPUT_LIMIT))
        if not isinstance(inspected, list) or len(inspected) != 1:
            raise ValueError("readiness inspection inventory refused")
        state = owned_runtime_state(inspected[0], container, self.image_identities[role]["imageID"], self.network, labels(self.owner))
        if selector != container and state["containerID"] != selector:
            raise ValueError("readiness immutable container ID differs")
        return state

    def readiness_failure(self, container, binding, probe):
        role = next((value for value in READINESS_ROLES if container == self.network + "-" + value), None)
        if role is None:
            raise ValueError("unowned readiness name refused")
        if not hasattr(self, "readiness_evidence"):
            self.readiness_evidence = {}
        record = {"informationalOnly": True, "stage": self.stage, "binding": dict(binding), "probe": dict(probe)}
        try:
            state = self.readiness_state(container, binding["containerID"])
            record["container"] = state
            # Only this broker's exact immutable ID may supply startup signals.
            # GMS/MySQL logs can contain synthetic credentials and are not read.
            if role == "broker":
                try:
                    self.run(["docker", "logs", "--tail", "40", state["containerID"]], timeout=10,
                             output_limit=READINESS_OUTPUT_LIMIT, runtime_source="broker-startup-log")
                except CommandFailure:
                    pass  # The closed result records a failed/incomplete capture.
                record["brokerLog"] = dict(self.last_runtime_result)
            return state
        except (ValueError, KeyError, TypeError, OSError):
            record["collectionFailure"] = "owned-container-inspection-refused"
            self.readiness_evidence.setdefault(role, {"informationalOnly": True, "attempts": 0, "recent": []})["waitOutcome"] = "diagnostics-refused"
            raise ValueError("owned readiness diagnostics refused") from None
        finally:
            retain_readiness(self.readiness_evidence, role, record)

    def wait_exec(self, container, args, timeout=120):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            # Bind source/run/nonce/image/name before any probe output exists.
            # Later reads and execution cannot follow a replacement name.
            try:
                binding = self.readiness_state(container, container)
            except (ValueError, KeyError, TypeError, OSError):
                role = container.removeprefix(self.network + "-")
                if role in READINESS_ROLES:
                    if not hasattr(self, "readiness_evidence"):
                        self.readiness_evidence = {}
                    self.readiness_evidence.setdefault(role, {"informationalOnly": True, "attempts": 0, "recent": []})["waitOutcome"] = "binding-refused"
                raise ValueError("readiness source ownership binding refused") from None
            try:
                output = self.run(["docker", "exec", binding["containerID"], *args], timeout=15,
                                  output_limit=READINESS_OUTPUT_LIMIT, runtime_source="readiness-command")
                if hasattr(self, "readiness_evidence"):
                    role = container.removeprefix(self.network + "-")
                    if role in self.readiness_evidence:
                        self.readiness_evidence[role]["waitOutcome"] = "command-exit-zero"
                return output
            except ValueError:
                probe = dict(getattr(self, "last_runtime_result", runtime_refusal("capture-unavailable", "readiness-command")))
                state = self.readiness_failure(container, binding, probe)
                if not state["Running"]:
                    role = container.removeprefix(self.network + "-")
                    self.readiness_evidence[role]["waitOutcome"] = "terminal-state" if state["status"] in {"exited", "dead"} else "container-not-running"
                    raise ValueError("fixture stopped at " + self.stage) from None
                time.sleep(2)
        role = container.removeprefix(self.network + "-")
        if role not in READINESS_ROLES:
            raise ValueError("unowned readiness deadline refused")
        if not hasattr(self, "readiness_evidence"):
            self.readiness_evidence = {}
        self.readiness_evidence.setdefault(role, {"informationalOnly": True, "attempts": 0, "recent": []})["waitOutcome"] = "deadline-exceeded"
        raise ValueError("fixture readiness exceeded bound at " + self.stage)

    def close(self):
        try:
            self.cleanup_evidence = cleanup_owned(self.owner_path, self.identity, self.run)["derivedImage"]
            return True
        except (ValueError, OSError, KeyError):
            return False


def qualify(scratch, binary):
    fixture = Fixture(scratch, binary)
    expected = expected_identity()
    identity = source_identity(fixture.run, expected)
    fixture.identity = identity
    fixture.owner.update(identity)
    fixture.owner["derivedImage"] = image_patch.owned_spec(fixture.owner, fixture.owner_path)
    validate_owner(fixture.owner, identity, fixture.owner_path)
    write_owner(fixture.owner_path, fixture.owner)
    report = dict(identity,
              owner=fixture.owner,
              **{
              "status": "failed", "cases": [], "selectedImages": fixture.source_images,
              "acceptanceComplete": False,
              "registryAuth": {"verifiedBeforeTargets": True, "helperSHA256": fixture.owner["registryHelperSHA256"], "controlsSHA256": fixture.owner["registryControlsSHA256"], "controlsCount": 7},
              "buildDiagnosticControls": {"count": 1, "sha256": fixture.owner["buildDiagnosticControlsSHA256"]},
              "testBinarySHA256": hashlib.sha256(binary.read_bytes()).hexdigest()})
    fatal_failure = None
    try:
        report["images"] = fixture.setup()
        fixture.stage = "locked-cli-version"
        dependencies = json.loads(fixture.cli(["python", "-c", "import importlib.metadata,json,sys; print(json.dumps({'python':sys.version.split()[0],'packages':{d.metadata['Name']:d.version for d in importlib.metadata.distributions()}}))"], network=False))
        if dependencies["packages"].get("acryl-datahub") != image_patch.lock()["cliPackageVersion"]:
            raise ValueError("actual ingestion package version differs")
        report["dependencies"] = dependencies
        patch = json.loads(fixture.cli(["cat", "/opt/kanz-catalog-patch.json"], network=False))
        image_patch.validate_proof(patch, dependencies)
        report["packagePatch"] = patch
        fixture.go_tests("^TestCatalogFixtureExport$", "export-tests.jsonl")
        fixture.stage = "projection-negative-controls"
        unit_output = fixture.cli(["python", "-B", "-m", "unittest", "discover", "-s", "/catalog", "-p", "test_*.py", "-v"], network=False)
        if not re.search(r"Ran 49 tests in", unit_output) or "skipped" in unit_output:
            raise ValueError("complete projection negative-control inventory required")
        (scratch / "projection-tests.txt").write_text(unit_output)
        report["projectionTests"] = {"count": 14, "sha256": hashlib.sha256(unit_output.encode()).hexdigest()}
        report["qualificationControls"] = {"count": 35}
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
        package_category = getattr(fixture, "preparation_evidence", {}).get("package", {}).get("category")
        fatal_failure = {"stage": fixture.stage, "category": package_category if fixture.stage == "fixed-ingestion-package-download" and package_category in image_patch.PACKAGE_REFUSALS else "evidence-refused"}
    finally:
        report["stage"] = fixture.stage
        report["failure"] = fatal_failure
        report["scanner"] = getattr(fixture, "scanner_evidence", None)
        report["images"] = getattr(fixture, "image_identities", report.get("images", {}))
        report["preparation"] = getattr(fixture, "preparation_evidence", None)
        report["mysqlRuntime"] = getattr(fixture, "mysql_evidence", None)
        report["readiness"] = getattr(fixture, "readiness_evidence", {})
        report["cleanupPassed"] = fixture.close()
        report["derivedCleanup"] = getattr(fixture, "cleanup_evidence", None)
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
                validate_receipt(report, final_identity, fixture.source_images, registry_pending=True)
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


def source_runner(stage):
    runner = object.__new__(Fixture)
    home = Path(os.environ["RUNNER_TEMP"]) / "catalog-controller-home"
    home.mkdir(mode=0o700, exist_ok=True)
    runner.environment = {"PATH": os.environ["PATH"], "HOME": str(home), "DOCKER_CONFIG": str(home), "LANG": "C.UTF-8"}
    runner.stage = stage
    return runner


def replace_receipt(path, report):
    temporary = path.with_suffix(".closing.json")
    write_owner(temporary, report)
    os.replace(temporary, path)


def registry_cleanup_main():
    root = Path(os.environ["RUNNER_TEMP"])
    expected = expected_identity()
    identity, source_verified = dict(expected), False
    try:
        identity = source_identity(source_runner("registry-final-source").run, expected)
        source_verified = True
    except (ValueError, OSError):
        pass  # Still remove the exact shared-owned credential profile.
    profile = Path(os.environ["DOCKER_CONFIG"])
    result = load_registry_auth().cleanup(root, profile, {"source": expected["sourceSHA"], "run": expected["runID"], "attempt": expected["runAttempt"]})
    cleanup = dict(identity, profile=str(profile), sourceVerified=source_verified, cleanup=result)
    write_owner(root / "catalog-registry-cleanup.json", cleanup)
    path = root / "catalog-fixture" / "catalog-receipt.json"
    target_closed = False
    if os.path.lexists(path):
        report = json.loads(bounded_file(path, OUTPUT_LIMIT))
        same_source = source_verified and all(report.get(key) == value for key, value in identity.items())
        same_profile = report.get("owner", {}).get("registryProfile") == str(profile)
        report["registryCleanup"] = cleanup
        report["acceptanceComplete"] = same_source and same_profile and result.get("passed") is True and not os.path.lexists(profile) and os.environ.get("KANZ_CATALOG_TARGET_CLEANUP") == "success" and report.get("status") == "passed"
        if report["acceptanceComplete"]:
            try:
                validate_receipt(report, identity, json.loads((HERE / "images.json").read_text())["images"])
            except (ValueError, KeyError, TypeError):
                report["acceptanceComplete"] = False
        if not report["acceptanceComplete"]:
            report["status"] = "failed"
        replace_receipt(path, report)
        target_closed = report["acceptanceComplete"]
    return 0 if source_verified and result.get("passed") is True and target_closed else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--scratch", type=Path)
    parser.add_argument("--test-binary", type=Path)
    parser.add_argument("--cleanup", action="store_true")
    parser.add_argument("--verify-source", action="store_true")
    parser.add_argument("--registry-cleanup", action="store_true")
    args = parser.parse_args()
    if sys.platform != "linux":
        raise SystemExit("catalog qualification requires Linux")
    if args.verify_source:
        source_identity(source_runner("before-registry-authentication").run, expected_identity())
        raise SystemExit(0)
    if args.registry_cleanup:
        raise SystemExit(registry_cleanup_main())
    if args.cleanup:
        cleanup_main()
        raise SystemExit(0)
    if not args.scratch or not args.test_binary or not args.scratch.is_dir():
        raise SystemExit("catalog qualification requires assigned Linux scratch")
    if not args.test_binary.is_file() or args.test_binary.resolve().is_relative_to(args.scratch.resolve()):
        raise SystemExit("readonly test binary must be outside assigned scratch")
    qualify(args.scratch.resolve(), args.test_binary.resolve())

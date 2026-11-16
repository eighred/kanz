"""Parent-side disposable proof; candidate processes never inherit runner access.

Run on Linux after provisioning the unchanged canonical SDK/module inputs and
the pinned images. No Docker socket, credential, host home or writable source is
mounted in a worker. Only the parent can create/remove the named containers.
"""

import argparse
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import re
import selectors
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid


GO = "ghcr.io/eighred/base/golang:1.26.10@sha256:eb36c1664dd974cde625f736e02c204383deebe03977365caaec5bf49f794348"
VAULT = "hashicorp/vault:1.21.4@sha256:6c77f568e6b6310d5bc68befb5711b9215c574de7da489e7c24332581176888b"
POSTGRES = "postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9"
PYTHON = "python:3.13.18-alpine3.24@sha256:7712cfd23e0501f5cb58faa5e6e0d515c9939bdbce866921799b31e696a1d13c"
PACKAGE = "github.com/eighred/kanz/internal/pg"
ROLE_TEST = "TestRuntimePrivilegesWithSeparateRoles"
VAULT_TEST = "TestRuntimeVaultIsolation"
REQUIRED = {ROLE_TEST, VAULT_TEST}
REQUIRED.update(ROLE_TEST + "/" + name for name in (
    "UPDATE", "DELETE", "TRUNCATE", "ALTER", "direct-truncate", "public-truncate",
    "trigger-authority", "schema-create", "database-create", "security-definer", "assumable-owner", "assumable-truncate", "predefined-capability", "role-createrole", "downscoped-session-owner", "writable-migration-bookkeeping", "writable-migration-bookkeeping/table", "writable-migration-bookkeeping/column"))
REQUIRED.update(VAULT_TEST + "/" + name for name in ("accounting", "audit", "identity"))
MANIFEST_TESTS = {"test_base_separates_all_three_roles_and_preserves_callers", "test_tokyo_preserves_transformed_immutable_images_and_scopes_database_peer", "test_changed_contracts_fail_closed"}
PROBE_FAILURES = frozenset({
    "unexpected worker environment name", "Python locale coercion added LC_CTYPE",
    "scratch environment mismatch", "worker is root", "external network interface present",
    "effective capability present", "kernel privilege/seccomp guard absent", "file limit missing",
    "descriptor limit missing", "core dump limit missing", "memory limit missing", "process limit missing",
    "CPU limit missing", "scratch disk limit missing", "unexpected input write refusal",
    "input/toolchain write was permitted", "scratch write positive control failed",
})


class ProofFailure(Exception):
    """A safe classification, never a captured child diagnostic."""


def bounded(argv, *, timeout=30, stdout_limit=1 << 20, stderr_limit=1 << 20, started=None):
    """Bound both streams while running, including failed/noisy children."""
    process = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, start_new_session=True)
    streams = selectors.DefaultSelector()
    output = [bytearray(), bytearray()]
    for index, pipe in enumerate((process.stdout, process.stderr)):
        streams.register(pipe, selectors.EVENT_READ, index)
    deadline = time.monotonic() + timeout
    try:
        if started is not None:
            started()
        while streams.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise ProofFailure("child wall-clock limit exceeded")
            for key, _ in streams.select(min(remaining, 0.2)):
                data = os.read(key.fileobj.fileno(), 65536)
                if not data:
                    streams.unregister(key.fileobj)
                    continue
                index = key.data
                if len(output[index]) + len(data) > (stdout_limit, stderr_limit)[index]:
                    raise ProofFailure("child output limit exceeded")
                output[index].extend(data)
        code = process.wait(timeout=max(0.1, deadline - time.monotonic()))
        return code, bytes(output[0]), bytes(output[1])
    finally:
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)
        streams.close()
        process.stdout.close()
        process.stderr.close()


def checked(argv, **limits):
    code, output, _ = bounded(argv, **limits)
    if code:
        raise ProofFailure("parent command refused; raw diagnostics discarded")
    return output


def write_exclusive(directory_fd, name, data, mode=0o600):
    """Only the parent writes retained metadata or the next worker's input."""
    if not re.fullmatch(r"[a-z0-9][a-z0-9_.-]{0,80}", name):
        raise ProofFailure("invalid owned artifact name")
    descriptor = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         mode, dir_fd=directory_fd)
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise ProofFailure("owned artifact is not an exclusive regular file")
        with os.fdopen(descriptor, "wb", closefd=False) as stream:
            stream.write(data)
    finally:
        os.close(descriptor)


def verify_regular(directory_fd, name, limit):
    descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory_fd)
    try:
        before = os.fstat(descriptor)
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or not 0 < before.st_size <= limit:
            raise ProofFailure("tool copy failed regular-file/size qualification")
        result = bytearray()
        while len(result) <= limit:
            chunk = os.read(descriptor, min(65536, limit + 1 - len(result)))
            if not chunk:
                break
            result.extend(chunk)
        after = os.fstat(descriptor)
        fields = ("st_dev", "st_ino", "st_mode", "st_nlink", "st_size", "st_mtime_ns", "st_ctime_ns")
        if any(getattr(before, field) != getattr(after, field) for field in fields) or len(result) != before.st_size or len(result) > limit:
            raise ProofFailure("tool changed during parent-side qualification")
        return bytes(result)
    finally:
        os.close(descriptor)


class Sandbox:
    def __init__(self, source, modules, yaml_path, tools, run_id):
        self.source, self.modules, self.yaml_path, self.tools = source, modules, yaml_path, tools
        self.prefix = "kanz-runtime-credentials-" + uuid.uuid4().hex[:16]
        self.created = []
        self.workers = []
        self.run_id = run_id

    def arguments(self, suffix, image, *, network="none", user="1000:1000", memory="512m", scratch="128m", cpus="1", pids="64", executable=False):
        name = self.prefix + "-" + suffix
        return ["docker", "create", "--name", name, "--pull", "never", "--network", network,
                "--label", "io.kanz.runtime-credentials-proof=true", "--label", "io.kanz.runtime-credentials-run=" + self.run_id,
                "--label", "io.kanz.runtime-credentials-owner=" + self.prefix,
                "--read-only", "--user", user, "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                "--cpus", cpus, "--memory", memory, "--memory-swap", memory, "--pids-limit", pids,
                "--ulimit", "nofile=256:256", "--ulimit", "fsize=100663296:100663296",
                "--ulimit", "core=0:0", "--stop-timeout", "3", "--log-driver", "none",
                "--tmpfs", "/dev/shm:ro,noexec,nosuid,nodev,size=64k",
                "--tmpfs", "/scratch:rw," + ("exec" if executable else "noexec") + ",nosuid,nodev,size=" + scratch + ",mode=1777",
                "--entrypoint", "/usr/bin/env", image, "-i",
                "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                "HOME=/scratch", "TMPDIR=/scratch", "LANG=C.UTF-8"]

    def mounts(self, argv, *, modules=False, tools=False, python=False, work="/source/kanz/internal/pg"):
        # docker options belong before the image/entrypoint. Insert at --entrypoint.
        options = ["--mount", "type=bind,src=" + str(self.source) + ",dst=/source,readonly", "--workdir", work]
        if modules:
            options += ["--mount", "type=bind,src=" + str(self.modules) + ",dst=/modules,readonly"]
        if tools:
            options += ["--mount", "type=bind,src=" + str(self.tools) + ",dst=/tools,readonly"]
        if python:
            options += ["--mount", "type=bind,src=" + str(self.yaml_path) + ",dst=/python-deps,readonly"]
        position = argv.index("--entrypoint")
        return argv[:position] + options + argv[position:]

    def inspect(self, container_id):
        raw = checked(["docker", "inspect", container_id], timeout=10, stdout_limit=128 << 10)
        values = json.loads(raw)
        if len(values) != 1 or values[0].get("Id") != container_id:
            raise ProofFailure("container identity evidence mismatch")
        return values[0]

    def owned(self, value, container_id, name):
        labels = value.get("Config", {}).get("Labels", {})
        return (value.get("Id") == container_id and value.get("Name") == "/" + name
                and labels.get("io.kanz.runtime-credentials-proof") == "true"
                and labels.get("io.kanz.runtime-credentials-run") == self.run_id
                and labels.get("io.kanz.runtime-credentials-owner") == self.prefix)

    def create(self, arguments, *, tool_copy=False):
        name = arguments[arguments.index("--name") + 1]
        if not re.fullmatch(re.escape(self.prefix) + r"-[a-z0-9-]+", name):
            raise ProofFailure("invalid owned container name")
        if tool_copy:
            image = arguments[-1]
        else:
            image_index = arguments.index("--entrypoint") + 2
            image = arguments[image_index]
            # Official image-declared volumes must not become anonymous writable
            # mounts. Shadow them with small read-only tmpfs before creation.
            image_info = json.loads(checked(["docker", "image", "inspect", image], stdout_limit=128 << 10))[0]
            volume_options = []
            for path in image_info.get("Config", {}).get("Volumes", {}) or {}:
                if not re.fullmatch(r"/[a-zA-Z0-9_./-]+", path) or ".." in Path(path).parts or path == "/scratch":
                    raise ProofFailure("unreviewed image volume boundary")
                volume_options += ["--tmpfs", path + ":ro,noexec,nosuid,nodev,size=64k"]
            arguments[arguments.index("--entrypoint"):arguments.index("--entrypoint")] = volume_options
            image_index = arguments.index("--entrypoint") + 2
            # Hold a short trusted prelude so every worker can be inspected while
            # running, including otherwise sub-second projection oracles.
            prelude = image_index + 2
            while prelude < len(arguments) and "=" in arguments[prelude] and not arguments[prelude].startswith("/"):
                prelude += 1
            environment = arguments[image_index + 2:prelude]
            # The trusted inspection shell can add its own metadata. Clear it
            # again immediately before the target, replaying only the exact
            # explicit assignments already supplied before the prelude.
            arguments[prelude:prelude] = ["/bin/sh", "-eu", "-c", 'sleep 3; exec /usr/bin/env -i "$@"', "runtime-proof", *environment]
        raw_id = checked(arguments, timeout=30).decode().strip()
        if not re.fullmatch(r"[0-9a-f]{64}", raw_id):
            raise ProofFailure("create did not return a full container identity")
        # Only a successful create's returned ID enters the cleanup set. A name
        # collision or failed create cannot adopt/delete somebody else's object.
        self.created.append((raw_id, name))
        actual = self.inspect(raw_id)
        if not self.owned(actual, raw_id, name):
            raise ProofFailure("new container ownership evidence mismatch")
        if tool_copy:
            if actual["Config"]["Image"] != image:
                raise ProofFailure("approved tool image mismatch")
            return raw_id
        host, config = actual["HostConfig"], actual["Config"]
        image_info = json.loads(checked(["docker", "image", "inspect", image], stdout_limit=128 << 10))[0]
        expected_mounts = {}
        expected_tmpfs = {}
        for index, item in enumerate(arguments):
            if item == "--mount":
                fields = dict(part.split("=", 1) for part in arguments[index + 1].split(",") if "=" in part)
                expected_mounts[fields["dst"]] = fields["src"]
            if item == "--tmpfs":
                path, options = arguments[index + 1].split(":", 1)
                expected_tmpfs[path] = options
        actual_mounts = {item["Destination"]: item for item in actual["Mounts"] if item["Type"] == "bind"}
        if set(actual_mounts) != set(expected_mounts) or any(item["Type"] not in ("bind", "tmpfs") for item in actual["Mounts"]):
            raise ProofFailure("unexpected writable/anonymous mount")
        if any(actual_mounts[path]["RW"] or actual_mounts[path]["Source"] != expected_mounts[path] for path in expected_mounts):
            raise ProofFailure("input mount identity/read-only mismatch")
        expected_memory = int(arguments[arguments.index("--memory") + 1][:-1]) << 20
        expected_pids = int(arguments[arguments.index("--pids-limit") + 1])
        expected_cpu = int(float(arguments[arguments.index("--cpus") + 1]) * 1_000_000_000)
        expected_user = arguments[arguments.index("--user") + 1]
        expected_network = arguments[arguments.index("--network") + 1]
        limits = {item["Name"]: (item["Soft"], item["Hard"]) for item in host["Ulimits"]}
        if not (actual["Image"] == image_info["Id"] and config["Image"] == image and config["User"] == expected_user
                and config["Entrypoint"] == ["/usr/bin/env"] and config["Cmd"] == arguments[image_index + 1:]
                and host["ReadonlyRootfs"] and not host["Privileged"] and not host.get("CapAdd")
                and host["CapDrop"] == ["ALL"] and host["SecurityOpt"] == ["no-new-privileges"]
                and host["NetworkMode"] == expected_network and not host.get("PortBindings") and not host.get("Links")
                and not host.get("PidMode") and host.get("IpcMode") == "private"
                and host["Memory"] == host["MemorySwap"] == expected_memory and host["NanoCpus"] == expected_cpu
                and host["PidsLimit"] == expected_pids and host["Tmpfs"] == expected_tmpfs
                and limits == {"nofile": (256, 256), "fsize": (100663296, 100663296), "core": (0, 0)}):
            raise ProofFailure("actual Docker configuration differs from required sandbox")
        digest = image.split("@", 1)[1]
        if not any(value.endswith("@" + digest) for value in image_info.get("RepoDigests", [])):
            raise ProofFailure("local image is not bound to the requested digest")
        self.workers.append({"id": raw_id, "name": name, "image": image, "image_id": actual["Image"],
                             "uid_gid": expected_user, "network": expected_network,
                             "mounts": [{"target": path, "read_only": True} for path in sorted(expected_mounts)],
                             "tmpfs": expected_tmpfs, "memory_bytes": expected_memory, "cpu_nano": expected_cpu,
                             "pids": expected_pids, "ulimits": limits, "read_only_root": True,
                             "capabilities": [], "no_new_privileges": True, "observed_running": False})
        return raw_id

    def observe_running(self, container_id):
        worker = next(item for item in self.workers if item["id"] == container_id)
        for _ in range(20):
            actual = self.inspect(container_id)
            if actual["State"]["Running"]:
                break
            if actual["State"]["Status"] in ("exited", "dead"):
                raise ProofFailure("worker exited before running-state inspection")
            time.sleep(0.05)
        else:
            raise ProofFailure("worker did not enter running state")
        if not self.owned(actual, container_id, worker["name"]):
            raise ProofFailure("running worker ownership changed")
        def kernel(path):
            return checked(["docker", "exec", container_id, "/usr/bin/env", "-i", "/bin/cat", path], timeout=3, stdout_limit=16 << 10).decode()
        process = dict(line.split(":", 1) for line in kernel("/proc/1/status").splitlines() if ":" in line)
        memory = int(kernel("/sys/fs/cgroup/memory.max").strip())
        pids = int(kernel("/sys/fs/cgroup/pids.max").strip())
        quota, period = kernel("/sys/fs/cgroup/cpu.max").split()
        interfaces = {line.split(":", 1)[0].strip() for line in kernel("/proc/1/net/dev").splitlines() if ":" in line}
        uid = int(process["Uid"].split()[0])
        if not (uid == int(worker["uid_gid"].split(":")[0]) and int(process["CapEff"].strip(), 16) == 0
                and process["NoNewPrivs"].strip() == "1" and process["Seccomp"].strip() == "2"
                and memory == worker["memory_bytes"] and pids == worker["pids"]
                and quota != "max" and int(quota) * 1_000_000_000 == int(period) * worker["cpu_nano"]
                and interfaces == {"lo"}):
            raise ProofFailure("actual kernel uid/network/cgroup/privilege boundary differs")
        worker.update({"observed_running": True, "kernel_uid": uid, "kernel_memory_max": memory,
                       "kernel_pids_max": pids, "kernel_cpu_max": [int(quota), int(period)],
                       "kernel_interfaces": ["lo"], "kernel_seccomp": 2, "kernel_cap_eff": 0})

    def run(self, arguments, **limits):
        container_id = self.create(arguments)
        return bounded(["docker", "start", "--attach", container_id], started=lambda: self.observe_running(container_id), **limits)

    def start(self, arguments):
        container_id = self.create(arguments)
        checked(["docker", "start", container_id], timeout=10)
        self.observe_running(container_id)
        return container_id

    def cleanup(self):
        results = []
        for container_id, name in reversed(self.created):
            try:
                actual = self.inspect(container_id)
                if not self.owned(actual, container_id, name):
                    results.append({"id": container_id, "status": "refused ownership mismatch"})
                    continue
                code, _, _ = bounded(["docker", "rm", "-f", "-v", container_id], timeout=20)
                # Absence must be observed through a successful exact-ID query;
                # a daemon/permission failure is not evidence of removal.
                remaining = checked(["docker", "ps", "-aq", "--no-trunc", "--filter", "id=" + container_id], timeout=10).strip()
                results.append({"id": container_id, "status": "removed and absent" if code == 0 and not remaining else "removal/absence failed"})
            except Exception:
                results.append({"id": container_id, "status": "cleanup observation failed"})
        return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--modules", type=Path, required=True)
    parser.add_argument("--yaml-dependency", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True, help="new external evidence directory")
    parser.add_argument("--run-id", required=True, help="numeric CI run-attempt identity, used only for owned cleanup")
    parser.add_argument("--canonical-binding", type=Path, required=True, help="parent-verified canonical provisioning input receipt")
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9]{1,20}-[0-9]{1,8}", args.run_id):
        parser.error("invalid CI run-attempt identity")
    if sys.platform != "linux" or not all(hasattr(os, name) for name in ("O_NOFOLLOW", "O_NONBLOCK", "O_DIRECTORY")):
        raise SystemExit("Linux no-follow/nonblocking proof parent is required")
    source = Path(__file__).resolve().parents[2]
    source_sha = checked(["git", "-C", str(source), "rev-parse", "HEAD"]).decode().strip()
    tree = checked(["git", "-C", str(source), "rev-parse", "HEAD^{tree}"]).decode().strip()
    if checked(["git", "-C", str(source), "status", "--porcelain", "--untracked-files=normal"]):
        raise ProofFailure("proof requires a clean committed source checkout")
    binding = json.loads(args.canonical_binding.read_bytes())
    if binding.get("source_sha") != source_sha or not re.fullmatch(r"[0-9a-f]{40}", binding.get("canonical_sha", "")):
        raise ProofFailure("canonical provisioning receipt belongs to another source")
    for path, digest in binding.get("inputs", {}).items():
        if not re.fullmatch(r"[A-Za-z0-9_.\-/]+", path) or ".." in Path(path).parts:
            raise ProofFailure("invalid canonical input path")
        if hashlib.sha256((source / path).read_bytes()).hexdigest() != digest:
            raise ProofFailure("canonical provisioning input changed before target execution")
    verifier_path = ".github/scripts/check_migration_evidence.py"
    if verifier_path not in binding.get("inputs", {}):
        raise ProofFailure("parent evidence verifier is not bound to unchanged canonical main")
    for path in (args.modules, args.yaml_dependency):
        if not path.is_dir() or "," in str(path.resolve()):
            raise ProofFailure("invalid provisioned dependency directory")
    args.evidence.mkdir(parents=True, exist_ok=False)
    evidence_fd = os.open(args.evidence, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    receipt = {"schema": 1, "source_sha": source_sha, "source_tree": tree, "phase": "preparation-only",
               "images": {"go": GO, "vault": VAULT, "postgres": POSTGRES, "python": PYTHON},
               "dependencies": {"PyYAML": "6.0.3", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "GOPROXY": "off"},
               "sandbox": {"external_network": False, "ambient_environment": False, "read_only_source": True,
                           "read_only_toolchain": True, "read_only_dependencies": True, "scratch_only_writes": True,
                           "host_ports": False, "docker_socket": False, "capabilities": [],
                           "cpu_max": 2, "memory_max_mib": 3072, "scratch_max_mib": 3072,
                           "pids_max": 256, "file_max_bytes": 100663296, "worker_wall_seconds_max": 300},
               "run_id": args.run_id, "canonical_provisioning": binding,
               "operator_or_deployed_acceptance": "not run", "stage": "start", "status": "not run"}
    sandbox = None
    try:
        with tempfile.TemporaryDirectory(prefix="kanz-runtime-parent-") as scratch:
            tools = Path(scratch)
            os.chmod(tools, 0o755)
            tools_fd = os.open(tools, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            sandbox = Sandbox(source, args.modules.resolve(), args.yaml_dependency.resolve(), tools, args.run_id)
            try:
                receipt["stage"] = "approved-tool-copy"
                copy_name = sandbox.prefix + "-vault-tool"
                copy_id = sandbox.create(["docker", "create", "--name", copy_name, "--network", "none", "--pull", "never",
                    "--label", "io.kanz.runtime-credentials-proof=true", "--label", "io.kanz.runtime-credentials-run=" + args.run_id,
                    "--label", "io.kanz.runtime-credentials-owner=" + sandbox.prefix, VAULT], tool_copy=True)
                checked(["docker", "cp", copy_id + ":/bin/vault", str(tools / "vault")], timeout=30)
                vault_binary = verify_regular(tools_fd, "vault", 512 << 20)
                if not vault_binary.startswith(b"\x7fELF"):
                    raise ProofFailure("approved Vault tool is not an ELF binary")
                receipt["vault_cli_sha256"] = hashlib.sha256(vault_binary).hexdigest()
                del vault_binary
                vault_fd = os.open("vault", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=tools_fd)
                try:
                    os.fchmod(vault_fd, 0o555)
                finally:
                    os.close(vault_fd)
                receipt["stage"] = "networkless-build"
                build = sandbox.mounts(sandbox.arguments("build", GO, memory="3072m", scratch="3072m", cpus="2", pids="256", executable=True), modules=True)
                build += ["GOTOOLCHAIN=local", "GOPATH=/scratch/go", "GOMODCACHE=/modules", "GOCACHE=/scratch/cache",
                          "GOTMPDIR=/scratch", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0",
                          "/bin/sh", "-eu", "-c", "go test -c -o /scratch/runtime-pg.test . >&2; cat /scratch/runtime-pg.test"]
                build_code, binary, build_errors = sandbox.run(build, timeout=300, stdout_limit=96 << 20)
                if build_code:
                    receipt["source_diagnostic_locations"] = sorted(set(re.findall(r"(runtime_(?:role|vault)(?:_integration_test)?\.go:[0-9]{1,5})", build_errors.decode(errors="replace"))))
                    raise ProofFailure("isolated source build failed")
                if not binary.startswith(b"\x7fELF"):
                    raise ProofFailure("networkless build did not produce its single ELF input")
                receipt["test_binary_sha256"] = hashlib.sha256(binary).hexdigest()
                write_exclusive(tools_fd, "runtime-pg.test", binary, 0o555)
                receipt["stage"] = "manifest-oracles"
                probe = sandbox.mounts(sandbox.arguments("sandbox-probe", PYTHON), modules=True, tools=True, python=True, work="/source/tools/runtime_credentials")
                probe += ["PYTHONPATH=/python-deps", "PYTHONDONTWRITEBYTECODE=1", "python", "-B", "sandbox_probe.py"]
                probe_code, probe_output, probe_errors = sandbox.run(probe, timeout=30)
                if probe_code or probe_output != b"Sandbox kernel controls passed\n":
                    # SystemExit diagnostics in the probe are fixed strings.
                    # Never retain raw output, a traceback or environment data.
                    classification = probe_errors.decode(errors="replace").strip()
                    receipt["sandbox_probe_failure"] = classification if classification in PROBE_FAILURES else "unclassified probe failure"
                    receipt["sandbox_probe_exit"] = probe_code
                    raise ProofFailure("kernel sandbox negative/positive probe failed")
                receipt["sandbox_probe"] = "kernel limits, empty environment, loopback only, read-only inputs and scratch writes passed"
                manifests = sandbox.mounts(sandbox.arguments("manifests", PYTHON), python=True, work="/source/tools/runtime_credentials")
                manifests += ["PYTHONPATH=/python-deps", "PYTHONDONTWRITEBYTECODE=1", "python", "-B", "test_manifests.py"]
                manifest_code, manifest_output, _ = sandbox.run(manifests, timeout=30)
                manifest_result = json.loads(manifest_output) if manifest_code == 0 else {}
                if manifest_code or manifest_result != {"tests": sorted(MANIFEST_TESTS), "passed": 3, "skipped": [], "yaml_version": "6.0.3"}:
                    raise ProofFailure("manifest fixture inventory failed")
                receipt["manifest_tests"] = manifest_result
                receipt["stage"] = "disposable-servers"
                postgres = sandbox.arguments("postgres", POSTGRES, user="70:70", scratch="512m", pids="64")
                postgres += ["PGDATA=/scratch/pgdata", "POSTGRES_USER=fixture_admin", "POSTGRES_PASSWORD=fixture", "POSTGRES_DB=fixture",
                             "POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256", "/usr/local/bin/docker-entrypoint.sh", "postgres",
                             "-c", "listen_addresses=127.0.0.1", "-c", "unix_socket_directories=/scratch", "-c", "max_connections=20",
                             "-c", "log_statement=none", "-c", "fsync=off", "-c", "dynamic_shared_memory_type=mmap"]
                postgres_id = sandbox.start(postgres)
                namespace = "container:" + postgres_id
                vault = sandbox.arguments("vault", VAULT, network=namespace, user="100:1000", scratch="64m")
                vault += ["VAULT_DISABLE_MLOCK=true", "/bin/vault", "server", "-dev", "-dev-root-token-id=fixture-root-token",
                          "-dev-listen-address=127.0.0.1:8200", "-log-level=error"]
                vault_id = sandbox.start(vault)
                ready = False
                for _ in range(30):
                    pg_ready, _, _ = bounded(["docker", "exec", postgres_id, "/usr/bin/env", "-i", "PATH=/usr/local/bin:/usr/bin:/bin", "pg_isready", "-h", "127.0.0.1", "-U", "fixture_admin", "-d", "fixture"], timeout=3)
                    vault_ready, _, _ = bounded(["docker", "exec", vault_id, "/usr/bin/env", "-i", "VAULT_ADDR=http://127.0.0.1:8200", "/bin/vault", "status"], timeout=3)
                    if pg_ready == vault_ready == 0:
                        ready = True
                        break
                    time.sleep(1)
                if not ready:
                    raise ProofFailure("owned dummy servers did not become ready")
                receipt["stage"] = "actual-vault-postgres-boundary"
                test = sandbox.mounts(sandbox.arguments("proof", GO, network=namespace, memory="768m", scratch="128m", pids="96"), tools=True)
                test += ["TEST_RUNTIME_ROLE_ADMIN_URL=postgresql://fixture_admin:fixture@127.0.0.1:5432/fixture?sslmode=disable",
                         "TEST_RUNTIME_VAULT_ADDR=http://127.0.0.1:8200", "TEST_RUNTIME_VAULT_ADMIN_TOKEN=fixture-root-token",
                         "GOTOOLCHAIN=local", "GOTMPDIR=/scratch", "GOCACHE=/scratch/cache", "GOPROXY=off", "GOSUMDB=off",
                         "go", "tool", "test2json", "-p", PACKAGE, "/tools/runtime-pg.test", "-test.v=test2json", "-test.timeout=270s", "-test.count=1", "-test.run=^(TestRuntimePrivilegesWithSeparateRoles|TestRuntimeVaultIsolation)$"]
                code, raw_events, _ = sandbox.run(test, timeout=290, stdout_limit=2 << 20)
                events = [json.loads(line) for line in raw_events.splitlines()]
                receipt["tests"] = [{"test": event["Test"], "status": event["Action"]} for event in events
                    if event.get("Package") == PACKAGE and event.get("Test") in REQUIRED and event.get("Action") in ("pass", "fail", "skip")]
                receipt["source_diagnostic_locations"] = sorted(set(location for event in events
                    for location in re.findall(r"(runtime_(?:role|vault)_integration_test\.go:[0-9]{1,5})", event.get("Output", ""))))
                # Reuse the repository's existing no-skip/no-truncation oracle.
                if hashlib.sha256((source / verifier_path).read_bytes()).hexdigest() != binding["inputs"][verifier_path]:
                    raise ProofFailure("canonical parent verifier changed before import")
                spec = importlib.util.spec_from_file_location("migration_evidence", source / verifier_path)
                verifier = importlib.util.module_from_spec(spec)
                spec.loader.exec_module(verifier)
                try:
                    verifier.verify(events, {"internal/pg": REQUIRED})
                except (ValueError, KeyError) as error:
                    # Its error names only known package/test identities below.
                    raise ProofFailure("required fixture inventory failed or skipped") from error
                if code:
                    raise ProofFailure("fixture process failed despite incomplete event evidence")
                # No child Output, unknown test name, token, DSN, key or server
                # log can enter retained evidence. Only fixed acceptance names.
                statuses = []
                for event in events:
                    if event.get("Package") != PACKAGE or event.get("Test") not in REQUIRED or event.get("Action") != "pass":
                        continue
                    elapsed = event.get("Elapsed", 0)
                    if not isinstance(elapsed, (int, float)) or not math.isfinite(elapsed) or not 0 <= elapsed <= 300:
                        raise ProofFailure("invalid bounded test event")
                    statuses.append({"test": event["Test"], "status": "pass", "elapsed": elapsed})
                receipt["tests"] = sorted(statuses, key=lambda item: item["test"])
                receipt["packages"] = [PACKAGE]
                receipt["skips"] = []
                receipt["status"] = "passed isolated phase-1 source proof"
            finally:
                os.close(tools_fd)
                primary_failure = sys.exc_info()[0] is not None
                receipt["workers"] = sandbox.workers
                receipt["cleanup"] = sandbox.cleanup()
                if any(item["status"] != "removed and absent" for item in receipt["cleanup"]) and not primary_failure:
                    raise ProofFailure("isolated proof passed but owned cleanup/absence evidence failed")
    except Exception as error:
        receipt["status"] = "needs validation"
        receipt["failure"] = str(error) if isinstance(error, ProofFailure) else "parent verifier could not complete safely"
        raise
    finally:
        write_exclusive(evidence_fd, "receipt.json", (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode())
        os.close(evidence_fd)


if __name__ == "__main__":
    try:
        main()
    except (ProofFailure, ValueError, OSError, subprocess.SubprocessError):
        raise SystemExit("Isolated runtime-credential proof did not complete; inspect the non-secret receipt stage.")

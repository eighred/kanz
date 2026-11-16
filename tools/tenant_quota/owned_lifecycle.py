"""Trusted parent lifecycle for this one disposable quota fixture.

This is kind/Docker orchestration, not a target-isolation certificate. Reviewed
host tools remain in the workflow TCB. No subprocess output is retained. A
failed startup still has an independently retained failure/cleanup receipt.
Only lifecycle command groups are observed; unchanged quota-body subprocess
descendants are outside that scope.
"""
import ast
from contextlib import contextmanager, nullcontext
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import stat
import subprocess
import sys
import time
import uuid


ROOT = Path(__file__).resolve().parents[2]
STATE = "quota-proof-owned"
REPORT = "quota-owned-lifecycle.json"
CLAIM_KEYS = {"format", "source", "tree", "run", "attempt", "cluster", "node", "inputs"}
INPUTS = ("tools/tenant_quota/owned_lifecycle.py", "tools/tenant_quota/qualify.py",
          "tools/tenant_quota/test_owned_lifecycle.py", "tools/feed_identity/proof.py",
          "kanz/infra/tenancy/tenantctl.sh", "kanz/test/arch/native_kafka_provision_test.go",
          ".github/workflows/redpanda-identity-proof.yml")
CREATE_SECONDS = 240
DELETE_SECONDS = 90
CLEANUP_SECONDS = 30  # One shared reserve, never another reserve per command.
MAX_OUTPUT = 64 << 10
MAX_JSON = 16 << 10
HEX64 = re.compile(r"[0-9a-f]{64}\Z")
HEX40 = re.compile(r"[0-9a-f]{40}\Z")
CLUSTER = re.compile(r"quota-proof-[0-9a-f]{12}\Z")
FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC


class Refused(RuntimeError):
    def __init__(self, category):
        super().__init__(category)
        self.category = category


PRIMARY_CATEGORIES = frozenset({"success", "timeout", "command_nonzero", "output_bound",
    "unknown", "body_failed", "preexisting_collision", "create_identity", "image_observation"})


def primary_class(error):
    if error is None:
        return "success"
    value = error.category if isinstance(error, Refused) else "unknown"
    return value if value in PRIMARY_CATEGORIES else "unknown"


# Fixed diagnostic labels only. They locate the active cleanup boundary, not
# an inferred operating-system or Kind cause. Unexpected failures stay unknown.
CLEANUP_PHASES = frozenset({"not_started", "journal", "allocation", "capture",
    "inventory", "delete", "container_absence", "network_absence", "work_retire"})
CLEANUP_CATEGORIES = frozenset({
    "allocation_custody", "budget_custody", "budget_holder_live", "budget_inventory",
    "budget_role", "claim_changed", "claim_custody", "cleanup_deadline", "clock_identity",
    "command_nonzero", "container_count", "container_identity", "duplicate_json_key",
    "holder_identity", "inventory_changed", "inventory_encoding", "inventory_shape",
    "launch_inventory", "network_identity", "network_observation_unknown",
    "observation_accounting_refused", "output_bound", "output_deadline",
    "owned_container_remains", "owned_endpoint_remains", "process_claim",
    "process_cleanup_unverified", "process_group_identity", "process_group_live_or_unknown",
    "process_identity", "process_inventory_bound", "process_stat", "process_stat_bound",
    "receipt_changed", "receipt_custody", "receipt_size", "receipt_write",
    "state_entry", "state_entry_bound", "timeout", "unbound_command", "unowned_collision",
    "work_cleanup_unverified", "work_custody"})


def cleanup_diagnostic(owner, error):
    phase = getattr(owner, "cleanup_phase", None)
    phase = phase if type(phase) is str and phase in CLEANUP_PHASES else "unknown"
    # Projection occurs after cleanup returned, including Reserve.__exit__.
    # A late accounting error must not become a completed cleanup observation.
    if phase == "work_retire" and getattr(owner, "cleaned", None) is True and error is None:
        phase = "completed"
    value = error.category if type(error) is Refused else None
    category = ("none" if error is None else
        value if type(value) is str and value in CLEANUP_CATEGORIES else "unknown")
    return dict(cleanup_phase=phase, cleanup_category=category)


def finish_owner(owner, cases, body_error):
    cleanup_error = retention_error = None
    try:
        owner.cleanup()
    except BaseException as error:
        cleanup_error = error
    try:
        owner.seal(cases, primary_class(body_error), cleanup_error)
    except BaseException as error:
        retention_error = error
    finally:
        try:
            owner.close()
        except BaseException as error:
            retention_error = error
    return dict(format="kanz-quota-outcome-v1", primary=primary_class(body_error),
        cleanup="verified_absent" if owner.cleaned and cleanup_error is None else "refused",
        retention="retained" if retention_error is None else "refused",
        **cleanup_diagnostic(owner, cleanup_error))


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode() + b"\n"


def same_file(before, after):
    return all(getattr(before, k) == getattr(after, k) for k in
               ("st_dev", "st_ino", "st_mode", "st_uid", "st_gid", "st_nlink",
                "st_size", "st_mtime_ns", "st_ctime_ns"))


def read_json(fd, name):
    leaf = os.open(name, FLAGS, dir_fd=fd)
    try:
        before = os.fstat(leaf)
        if (not stat.S_ISREG(before.st_mode) or before.st_nlink != 1
                or before.st_uid != os.getuid() or stat.S_IMODE(before.st_mode) != 0o600
                or not 0 < before.st_size <= MAX_JSON):
            raise Refused("claim_custody")
        raw = os.read(leaf, MAX_JSON + 1)
        if len(raw) != before.st_size or not same_file(before, os.fstat(leaf)):
            raise Refused("claim_changed")
        value = json.loads(raw, object_pairs_hook=unique_object)
        return value, hashlib.sha256(raw).hexdigest()
    finally:
        os.close(leaf)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise Refused("duplicate_json_key")
        result[key] = value
    return result


def write_json(fd, name, value):
    raw = canonical(value)
    if len(raw) > MAX_JSON:
        raise Refused("receipt_size")
    leaf = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
                   | os.O_CLOEXEC, 0o600, dir_fd=fd)
    try:
        os.fchmod(leaf, 0o600)
        before = os.fstat(leaf)
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
            raise Refused("receipt_custody")
        view = memoryview(raw)
        while view:
            count = os.write(leaf, view)
            if count <= 0:
                raise Refused("receipt_write")
            view = view[count:]
        os.fsync(leaf)
        after = os.fstat(leaf)
        if (after.st_dev, after.st_ino, after.st_nlink, after.st_size,
                stat.S_IMODE(after.st_mode)) != (before.st_dev, before.st_ino, 1, len(raw), 0o600):
            raise Refused("receipt_changed")
        os.fsync(fd)
    finally:
        os.close(leaf)
    return hashlib.sha256(raw).hexdigest()


def open_directory(path):
    """Walk existing components without following a symlink or changing modes."""
    path = Path(path)
    if not path.is_absolute() or ".." in path.parts:
        raise Refused("assigned_directory")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        for part in path.parts[1:]:
            child = os.open(part, FLAGS | os.O_DIRECTORY, dir_fd=fd)
            os.close(fd)
            fd = child
        value = os.fstat(fd)
        if value.st_uid != os.getuid() or stat.S_IMODE(value.st_mode) & 0o022:
            raise Refused("assigned_directory")
        return fd
    except BaseException:
        os.close(fd)
        raise


def node_pin():
    tree = ast.parse((ROOT / "tools/feed_identity/proof.py").read_bytes())
    values = [n.value.value for n in tree.body if isinstance(n, ast.Assign)
              and any(isinstance(t, ast.Name) and t.id == "NODE" for t in n.targets)
              and isinstance(n.value, ast.Constant) and isinstance(n.value.value, str)]
    if len(values) != 1 or not re.fullmatch(r"kindest/node:v[0-9.]+@sha256:[0-9a-f]{64}", values[0]):
        raise Refused("node_pin")
    return values[0]


class Reserve:
    def __init__(self, clock=time.monotonic):
        self.clock = clock
        self.remaining = CLEANUP_SECONDS
        self.entered = None
        self.depth = 0
        self.begin = self.finish = None
        self.refused = False

    @contextmanager
    def observe(self, enabled=True):
        if not enabled:
            yield
            return
        if self.refused:
            raise Refused("observation_accounting_refused")
        if self.depth == 0:
            if self.begin is not None:
                self.token, self.remaining = self.begin()
            self.entered = self.clock()
        self.depth += 1
        failure = None
        try:
            self.left()
            yield
        except BaseException as error:
            failure = error
            raise
        finally:
            self.depth -= 1
            if self.depth == 0:
                self.remaining -= self.clock() - self.entered
                self.entered = None
                if self.finish is not None:
                    try:
                        self.finish(self.token)
                    except BaseException:
                        self.refused = True
                        if failure is None:
                            raise

    def left(self):
        left = self.remaining - (self.clock() - self.entered if self.depth else 0)
        if left <= 0:
            raise Refused("cleanup_deadline")
        return left


def process_group(group):
    """Only scalar identity facts; never retain command lines or environments."""
    result = []
    with os.scandir("/proc") as entries:
        for index, entry in enumerate(entries):
            if index >= 65536:
                raise Refused("process_inventory_bound")
            if not entry.name.isdecimal():
                continue
            try:
                with open("/proc/" + entry.name + "/stat", "rb") as f:
                    raw = f.read(4097)
                if len(raw) > 4096:
                    raise Refused("process_stat_bound")
                fields = raw[raw.rindex(b")") + 2:].split()
                if len(fields) < 20:
                    raise Refused("process_stat")
                if int(fields[2]) == group:
                    if int(fields[3]) != group or os.stat("/proc/" + entry.name).st_uid != os.getuid():
                        raise Refused("process_group_identity")
                    result.append((int(entry.name), fields[0] == b"Z", int(fields[19])))
            except (FileNotFoundError, ProcessLookupError):
                continue
    return result


def process_birth(pid):
    if type(pid) is not int or not 0 < pid <= 2147483647:
        raise Refused("holder_identity")
    try:
        with open("/proc/" + str(pid) + "/stat", "rb") as f:
            raw = f.read(4097)
        if len(raw) > 4096:
            raise Refused("holder_identity")
        fields = raw[raw.rindex(b")") + 2:].split()
        return int(fields[19])
    except (FileNotFoundError, ProcessLookupError):
        return None


def boot_id():
    fd = os.open("/proc/sys/kernel/random/boot_id", FLAGS)
    try:
        raw = os.read(fd, 38)
    finally:
        os.close(fd)
    if not re.fullmatch(rb"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\n", raw):
        raise Refused("clock_identity")
    return raw.decode().strip()


class Commands:
    """Private finite-call transport. Callers never supply a command through CLI."""
    def __init__(self):
        self.reserve = Reserve()
        self.last_exit = None
        self.timed_out = False
        self.groups_absent = True
        self.group_cleanup_refused = False
        self.before_spawn = self.after_spawn = None

    def run(self, argv, seconds, *, observation=False, missing_ok=False, on_spawn=None, preclaim=False):
        if preclaim:
            if (argv != ["git", "rev-parse", "HEAD", "HEAD^{tree}"] or seconds != 10
                    or observation or on_spawn is not None or self.before_spawn is not None):
                raise Refused("preclaim_command")
        elif self.reserve.begin is None or self.before_spawn is None or self.after_spawn is None:
            raise Refused("unbound_command")
        with self.reserve.observe(observation):
            return self._run(argv, seconds, observation=observation, missing_ok=missing_ok,
                             on_spawn=on_spawn, preclaim=preclaim)

    def _run(self, argv, seconds, *, observation, missing_ok, on_spawn, preclaim):
        if observation:
            seconds = min(seconds, self.reserve.left())
        self.last_exit, self.timed_out = None, False
        ticket = self.before_spawn() if self.before_spawn is not None else None
        process = selector = None
        failure = None
        total_end = time.monotonic() + seconds
        # One preclaim Git call per workflow parent: leave two seconds INSIDE
        # its ten-second maximum for owned-child shutdown, with no fresh reserve.
        work_end = total_end - 2 if preclaim else total_end
        try:
            process = subprocess.Popen(argv, cwd=ROOT, stdin=subprocess.DEVNULL,
                                       stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                       start_new_session=True, close_fds=True)
            group = process.pid
            # Every operation after a successful spawn is inside this cleanup
            # try, including custody callbacks and selector construction.
            if on_spawn is not None or self.after_spawn is not None:
                with self.reserve.observe():
                    entries = process_group(group)
                    leader = [birth for pid, _, birth in entries if pid == group]
                    if len(leader) != 1:
                        raise Refused("process_identity")
                    record = {"pid": group, "starttime": leader[0], "uid": os.getuid()}
                    if self.after_spawn is not None:
                        self.after_spawn(ticket, record)
                    if on_spawn is not None:
                        on_spawn(record)
            selector = selectors.DefaultSelector()
            selector.register(process.stdout, selectors.EVENT_READ)
            output = bytearray()
            while True:
                remaining = work_end - time.monotonic()
                if remaining <= 0:
                    self.timed_out = True
                    raise Refused("timeout")
                for key, _ in selector.select(min(remaining, 0.05)):
                    chunk = os.read(key.fd, min(8192, MAX_OUTPUT + 1 - len(output)))
                    if not chunk:
                        selector.unregister(key.fileobj)
                    else:
                        output.extend(chunk)
                        if len(output) > MAX_OUTPUT:
                            raise Refused("output_bound")
                if os.waitid(os.P_PID, group, os.WEXITED | os.WNOHANG | os.WNOWAIT) is not None:
                    break
            self._stop(process, deadline=total_end if preclaim else None)
            self.last_exit = process.returncode
            while selector.get_map():
                if time.monotonic() >= work_end:
                    raise Refused("output_deadline")
                for key, _ in selector.select(min(0.05, work_end - time.monotonic())):
                    chunk = os.read(key.fd, min(8192, MAX_OUTPUT + 1 - len(output)))
                    if not chunk:
                        selector.unregister(key.fileobj)
                    else:
                        output.extend(chunk)
                        if len(output) > MAX_OUTPUT:
                            raise Refused("output_bound")
            if self.last_exit != 0 and not missing_ok:
                raise Refused("command_nonzero")
            return self.last_exit, bytes(output)
        except BaseException as error:
            failure = error
            raise
        finally:
            try:
                if process is not None:
                    if process.returncode is None:
                        self._stop(process, deadline=total_end if preclaim else None)
                    self.last_exit = process.returncode
            except BaseException:
                self.groups_absent = False
                self.group_cleanup_refused = True
                if failure is None:
                    raise
            finally:
                try:
                    if selector is not None:
                        selector.close()
                finally:
                    if process is not None and process.stdout is not None:
                        process.stdout.close()

    def _stop(self, process, *, deadline=None):
        # This method's kill authority exists ONLY for our still-unreaped direct
        # child. WNOWAIT keeps its PID allocated until every kill is finished.
        scope = self.reserve.observe() if deadline is None else nullcontext()
        def left():
            value = self.reserve.left() if deadline is None else deadline - time.monotonic()
            if value <= 0:
                raise Refused("cleanup_deadline")
            return value
        def members():
            left()
            value = process_group(process.pid)
            left()
            return value
        try:
            with scope:
                alive = any(not zombie for _, zombie, _ in members())
                if alive:
                    self.groups_absent = False
                    os.killpg(process.pid, signal.SIGTERM)
                    term_end = min(time.monotonic() + 2, time.monotonic() + left())
                    while any(not z for _, z, _ in members()) and time.monotonic() < term_end:
                        time.sleep(min(0.02, left()))
                    if any(not z for _, z, _ in members()):
                        os.killpg(process.pid, signal.SIGKILL)
                    while any(not z for _, z, _ in members()):
                        time.sleep(min(0.02, left()))
                process.wait(timeout=left())
                # No further kill is permitted after wait releases the leader PID.
                while members():
                    time.sleep(min(0.02, left()))
                self.groups_absent = not self.group_cleanup_refused
        except BaseException:
            self.groups_absent = False
            self.group_cleanup_refused = True
            if process.returncode is None:
                # Stop only the still-held child's own group even when proof
                # time is exhausted. This does NOT claim join/absence success.
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            raise

    def reconcile_group(self, record):
        self.groups_absent = False
        if (type(record) is not dict or set(record) != {"pid", "starttime", "uid"}
                or any(type(record[k]) is not int for k in record)
                or record["pid"] <= 1 or record["starttime"] <= 0 or record["uid"] != os.getuid()):
            raise Refused("process_claim")
        with self.reserve.observe():
            # Cross-parent observation grants no numeric-PGID kill authority.
            # A birth read cannot atomically protect a later kill from reuse.
            if process_group(record["pid"]):
                raise Refused("process_group_live_or_unknown")
            self.groups_absent = not self.group_cleanup_refused


def lines(raw, pattern, maximum=256):
    try:
        values = raw.decode("ascii").splitlines()
    except UnicodeError:
        raise Refused("inventory_encoding") from None
    if len(values) > maximum or len(values) != len(set(values)) or any(not pattern.fullmatch(v) for v in values):
        raise Refused("inventory_shape")
    return values


def network_endpoint_ids(raw):
    """Accept the exact-network CLI's bounded JSON object, never blank output."""
    if type(raw) is not bytes:
        raise Refused("inventory_shape")
    if len(raw) > MAX_OUTPUT:
        raise Refused("output_bound")
    def refuse_nonfinite(_value):
        raise Refused("network_observation_unknown")
    try:
        value = json.loads(raw.decode("utf-8", errors="strict"), object_pairs_hook=unique_object,
                           parse_constant=refuse_nonfinite)
    except (ValueError, UnicodeError):
        raise Refused("network_observation_unknown") from None
    if type(value) is not dict:
        raise Refused("network_observation_unknown")
    if (len(value) > 4 or any(not HEX64.fullmatch(key) or type(endpoint) is not dict
                             for key, endpoint in value.items())):
        raise Refused("inventory_shape")
    return set(value)


def validate_claim(value, identity, node, inputs):
    if (type(value) is not dict or set(value) != CLAIM_KEYS
            or value["format"] != "kanz-quota-owned-claim-v1"
            or any(value[k] != identity[k] for k in ("source", "tree", "run", "attempt"))
            or not isinstance(value["cluster"], str) or not CLUSTER.fullmatch(value["cluster"])
            or value["node"] != node or value["inputs"] != inputs):
        raise Refused("foreign_claim")
    return value["cluster"]


def identity(commands):
    raw = commands.run(["git", "rev-parse", "HEAD", "HEAD^{tree}"], 10, preclaim=True)[1]
    try:
        values = raw.decode("ascii").splitlines()
    except UnicodeError:
        raise Refused("run_identity") from None
    if len(values) != 2 or any(not HEX40.fullmatch(v) for v in values):
        raise Refused("run_identity")
    source, tree = values
    run, attempt = os.environ.get("GITHUB_RUN_ID", ""), os.environ.get("GITHUB_RUN_ATTEMPT", "")
    if not re.fullmatch(r"[1-9][0-9]{0,19}", run) or not re.fullmatch(r"[1-9][0-9]{0,5}", attempt):
        raise Refused("run_identity")
    return dict(source=source, tree=tree, run=run, attempt=attempt)


def input_hashes():
    result = {}
    for path in INPUTS:
        fd = os.open(ROOT / path, FLAGS)
        try:
            before = os.fstat(fd)
            if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or not 0 < before.st_size <= MAX_OUTPUT:
                raise Refused("source_custody")
            raw = os.read(fd, MAX_OUTPUT + 1)
            if len(raw) != before.st_size or not same_file(before, os.fstat(fd)):
                raise Refused("source_changed")
            result[path] = hashlib.sha256(raw).hexdigest()
        finally:
            os.close(fd)
    return result


class OwnedLifecycle:
    def __init__(self, *, cleanup_only=False, commands=None):
        if sys.platform != "linux" or not all(hasattr(os, k) for k in ("O_NOFOLLOW", "WNOWAIT")):
            raise Refused("linux_required")
        self.commands = commands or Commands()
        self.identity = identity(self.commands)
        self.node, self.inputs = node_pin(), input_hashes()
        # Only two bounded Git identity reads. These actual-byte hashes must
        # independently match the accepted tree's canonical input manifest;
        # formatting metadata by itself is not source attestation.
        self.parent = open_directory(os.environ["RUNNER_TEMP"])
        self.fd = None
        self.claim = None
        self.created = False
        self.create_verified = False
        self.cleaned = False
        self.cleanup_phase = "not_started"
        self.containers = []
        self.networks = []
        self.primary = "unknown"
        self.phase = "claim"
        self.create_exit = None
        self.create_timeout = False
        self.image = {"present": None, "id": None, "requested_digest_match": None}
        self.launch_index = 0
        try:
            if not cleanup_only:
                os.mkdir(STATE, 0o700, dir_fd=self.parent)
            self.fd = os.open(STATE, FLAGS | os.O_DIRECTORY, dir_fd=self.parent)
            info = os.fstat(self.fd)
            if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
                raise Refused("state_custody")
            if cleanup_only:
                self.claim, self.claim_hash = read_json(self.fd, "claim.json")
                validate_claim(self.claim, self.identity, self.node, self.inputs)
                self.phase = "reconciliation"
            else:
                name = "quota-proof-" + uuid.uuid4().hex[:12]
                self.claim = dict(format="kanz-quota-owned-claim-v1", **self.identity,
                                  cluster=name, node=self.node, inputs=self.inputs)
                self.claim_hash = write_json(self.fd, "claim.json", self.claim)
            self.commands.before_spawn = self.begin_launch
            self.commands.after_spawn = self.record_launch
            self.commands.reserve.begin = lambda: self.begin_budget("observe")
            self.commands.reserve.finish = self.finish_budget
        except BaseException:
            self.close()
            raise

    @property
    def name(self):
        return self.claim["cluster"]

    def launch_records(self):
        names = os.listdir(self.fd)
        if len(names) > 200:
            raise Refused("state_entry_bound")
        allowed = {"claim.json", "preflight.json", "started.json", "inventory.json", "process.json", "work.json", "work"}
        for name in names:
            if (name not in allowed and not re.fullmatch(r"(?:launch|child)-[0-9]{2}\.json", name)
                    and not re.fullmatch(r"budget-(?:observe|delete)-[0-9]{2}-(?:begin|end)\.json", name)):
                raise Refused("state_entry")
        indexes = sorted(int(name[7:9]) for name in names if re.fullmatch(r"launch-[0-9]{2}\.json", name))
        if len(indexes) > 32 or indexes != list(range(1, len(indexes) + 1)):
            raise Refused("launch_inventory")
        if any(int(name[6:8]) not in indexes for name in names if re.fullmatch(r"child-[0-9]{2}\.json", name)):
            raise Refused("launch_inventory")
        return indexes

    def begin_launch(self):
        indexes = self.launch_records()
        index = len(indexes) + 1
        if index > 32:
            raise Refused("launch_inventory")
        write_json(self.fd, f"launch-{index:02}.json", {"claim": self.claim_hash, "launch": index})
        return index

    def record_launch(self, index, record):
        write_json(self.fd, f"child-{index:02}.json", {"claim": self.claim_hash, "launch": index, "process": record})

    def reconcile_launches(self):
        checked = []
        for index in self.launch_records():
            intent, _ = read_json(self.fd, f"launch-{index:02}.json")
            record, _ = read_json(self.fd, f"child-{index:02}.json")
            if (intent != {"claim": self.claim_hash, "launch": index}
                    or set(record) != {"claim", "launch", "process"}
                    or record["claim"] != self.claim_hash or record["launch"] != index):
                raise Refused("launch_inventory")
            self.commands.reconcile_group(record["process"])
            checked.append(record["process"])
        return checked

    def begin_budget(self, role):
        # Two closed allowances, carried across qualification/always-cleanup
        # parents. Neither a new process nor a second command resets a budget.
        caps = {"observe": CLEANUP_SECONDS, "delete": DELETE_SECONDS}
        if role not in caps:
            raise Refused("budget_role")
        self.launch_records()  # Global closed-name/count bound, not just role entries.
        names = os.listdir(self.fd)
        pattern = re.compile(r"budget-" + role + r"-([0-9]{2})-begin\.json\Z")
        indexes = sorted(int(match.group(1)) for name in names if (match := pattern.fullmatch(name)))
        if indexes != list(range(1, len(indexes) + 1)) or len(indexes) >= 32:
            raise Refused("budget_inventory")
        end_pattern = re.compile(r"budget-" + role + r"-([0-9]{2})-end\.json\Z")
        if any(int(match.group(1)) not in indexes for name in names if (match := end_pattern.fullmatch(name))):
            raise Refused("budget_inventory")
        remaining = caps[role] * 1000000000
        current_boot = boot_id()
        now = time.monotonic_ns()
        for index in indexes:
            prefix = f"budget-{role}-{index:02}"
            begin, _ = read_json(self.fd, prefix + "-begin.json")
            fields = {"claim", "index", "boot", "remaining_ns", "started_ns", "holder_pid", "holder_birth"}
            if (type(begin) is not dict or set(begin) != fields or begin["claim"] != self.claim_hash
                    or begin["boot"] != current_boot or type(begin["index"]) is not int or begin["index"] != index
                    or any(type(begin[k]) is not int or begin[k] <= 0 for k in
                           ("remaining_ns", "started_ns", "holder_pid", "holder_birth"))
                    or begin["remaining_ns"] != remaining or begin["started_ns"] > now):
                raise Refused("budget_custody")
            try:
                end, _ = read_json(self.fd, prefix + "-end.json")
            except FileNotFoundError:
                if index != indexes[-1] or process_birth(begin["holder_pid"]) == begin["holder_birth"]:
                    raise Refused("budget_holder_live")
                # A canceled interval is charged until this observation. Never
                # assume zero use, grant another allowance or kill a holder.
                end = {"claim": self.claim_hash, "index": index, "finished_ns": now}
                write_json(self.fd, prefix + "-end.json", end)
            if (type(end) is not dict or set(end) != {"claim", "index", "finished_ns"}
                    or end["claim"] != self.claim_hash or type(end["index"]) is not int or end["index"] != index
                    or type(end["finished_ns"]) is not int
                    or not begin["started_ns"] <= end["finished_ns"] <= now):
                raise Refused("budget_custody")
            remaining -= end["finished_ns"] - begin["started_ns"]
            if remaining <= 0:
                raise Refused("cleanup_deadline")
        index = len(indexes) + 1
        prefix = f"budget-{role}-{index:02}"
        started = time.monotonic_ns()
        birth = process_birth(os.getpid())
        if birth is None:
            raise Refused("holder_identity")
        write_json(self.fd, prefix + "-begin.json", {"claim": self.claim_hash, "index": index,
            "boot": current_boot, "remaining_ns": remaining, "started_ns": started,
            "holder_pid": os.getpid(), "holder_birth": birth})
        left = remaining - (time.monotonic_ns() - started)
        if left <= 0:
            raise Refused("cleanup_deadline")
        return (role, index), left / 1000000000

    def finish_budget(self, token):
        role, index = token
        if role not in {"observe", "delete"} or type(index) is not int or not 1 <= index <= 32:
            raise Refused("budget_role")
        write_json(self.fd, f"budget-{role}-{index:02}-end.json", {
            "claim": self.claim_hash, "index": index, "finished_ns": time.monotonic_ns()})

    @property
    def work(self):
        return Path(os.environ["RUNNER_TEMP"]) / STATE / "work"

    def kind_names(self, observation=False):
        return lines(self.commands.run(["kind", "get", "clusters"], 10, observation=observation)[1],
                     re.compile(r"[a-z0-9][a-z0-9.-]{0,127}\Z"))

    def listed_ids(self, observation=False, exact_id=None):
        query = "id=" + exact_id if exact_id else "label=io.x-k8s.kind.cluster=" + self.name
        return lines(self.commands.run(["docker", "container", "ls", "--all", "--no-trunc", "--filter", query,
                                        "--format", "{{.ID}}"], 10, observation=observation)[1], HEX64, 4)

    def capture(self, observation=False):
        ids = self.listed_ids(observation)
        nodes, networks = [], set()
        template = ('{"id":{{json .Id}},"name":{{json .Name}},'
                    '"cluster":{{json (index .Config.Labels "io.x-k8s.kind.cluster")}},'
                    '"role":{{json (index .Config.Labels "io.x-k8s.kind.role")}},'
                    '"networks":{{json .NetworkSettings.Networks}}}')
        for cid in ids:
            raw = self.commands.run(["docker", "container", "inspect", "--format", template, cid], 10,
                                    observation=observation)[1]
            value = json.loads(raw, object_pairs_hook=unique_object)
            if (set(value) != {"id", "name", "cluster", "role", "networks"} or value["id"] != cid
                    or value["name"] != "/" + self.name + "-control-plane" or value["cluster"] != self.name
                    or value["role"] != "control-plane" or set(value["networks"]) != {"kind"}):
                raise Refused("container_identity")
            nid = value["networks"]["kind"].get("NetworkID")
            if not isinstance(nid, str) or not HEX64.fullmatch(nid):
                raise Refused("network_identity")
            nodes.append(cid)
            networks.add(nid)
        if len(nodes) > 1:
            raise Refused("container_count")
        return nodes, sorted(networks)

    def prepare(self):
        self.phase = "collision_check"
        if self.name in self.kind_names() or self.listed_ids():
            raise Refused("preexisting_collision")
        write_json(self.fd, "preflight.json", {"claim": self.claim_hash, "collision_absent": True})
        os.mkdir("work", 0o700, dir_fd=self.fd)
        info = os.stat("work", dir_fd=self.fd, follow_symlinks=False)
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
            raise Refused("work_custody")
        write_json(self.fd, "work.json", {"claim": self.claim_hash, "device": info.st_dev, "inode": info.st_ino})
        return self.work

    def retire_work(self):
        try:
            info = os.stat("work", dir_fd=self.fd, follow_symlinks=False)
        except FileNotFoundError:
            return
        owned, _ = read_json(self.fd, "work.json")
        if (owned != {"claim": self.claim_hash, "device": info.st_dev, "inode": info.st_ino}
                or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o700 or not shutil.rmtree.avoids_symlink_attacks):
            raise Refused("work_custody")
        # Original TemporaryDirectory cleanup semantics, scoped to the exact
        # exclusively allocated private directory and resistant to child links.
        shutil.rmtree("work", dir_fd=self.fd)
        try:
            os.stat("work", dir_fd=self.fd, follow_symlinks=False)
        except FileNotFoundError:
            return
        raise Refused("work_cleanup_unverified")

    def create(self, config):
        if config != self.work / "kubeconfig":
            raise Refused("config_role")
        try:
            self.phase = "image_observation"
            code, raw = self.commands.run(["docker", "image", "inspect", "--format",
                '{"id":{{json .Id}},"digests":{{json .RepoDigests}}}', self.node], 10, missing_ok=True)
            if code == 0:
                image = json.loads(raw, object_pairs_hook=unique_object)
                digests = image.get("digests") or []
                if (set(image) != {"id", "digests"} or not isinstance(image["id"], str)
                        or not re.fullmatch(r"sha256:[0-9a-f]{64}", image["id"])
                        or type(digests) is not list or len(digests) > 16
                        or any(not isinstance(v, str) or len(v) > 300 for v in digests)):
                    raise Refused("image_observation")
                requested = "kindest/node@" + self.node.split("@", 1)[1]
                self.image = dict(present=True, id=image["id"], requested_digest_match=requested in digests)
            # A nonzero inspect remains UNKNOWN, not evidence of a missing image
            # or a failed pull. No image is substituted or fetched by this code.
            write_json(self.fd, "started.json", {"claim": self.claim_hash, "create_started": True})
            self.created = True
            self.phase = "create"
            try:
                self.commands.run(["kind", "create", "cluster", "--name", self.name, "--image", self.node,
                                   "--kubeconfig", str(config), "--wait", "90s"], CREATE_SECONDS,
                                  on_spawn=lambda record: write_json(self.fd, "process.json",
                                      {"claim": self.claim_hash, "process": record}))
            finally:
                self.create_exit = self.commands.last_exit
                self.create_timeout = self.commands.timed_out
            self.phase = "create_identity"
            self.containers, self.networks = self.capture(True)
            if len(self.containers) != 1 or self.name not in self.kind_names(True):
                raise Refused("create_identity")
            write_json(self.fd, "inventory.json", {"claim": self.claim_hash,
                "containers": self.containers, "networks": self.networks})
            self.primary = "success"
            self.create_verified = True
            self.phase = "quota_body"
        except BaseException as error:
            self.primary = error.category if isinstance(error, Refused) else "unknown"
            raise

    def cleanup(self):
        self.cleanup_phase = "journal"
        self.commands.groups_absent = False
        # One interval for the whole pre-delete observation phase. Nested
        # per-record/per-command checks consume this same running allowance.
        with self.commands.reserve.observe():
            checked = self.reconcile_launches()
            if self.commands.group_cleanup_refused or self.commands.reserve.refused:
                raise Refused("process_cleanup_unverified")
            self.commands.groups_absent = True
            self.cleanup_phase = "allocation"
            try:
                started, _ = read_json(self.fd, "started.json")
            except FileNotFoundError:
                self.cleanup_phase = "container_absence"
                if self.name in self.kind_names(True) or self.listed_ids(True):
                    raise Refused("unowned_collision")
                self.cleanup_phase = "work_retire"
                self.retire_work()
                self.cleaned = self.commands.groups_absent
                return
            preflight, _ = read_json(self.fd, "preflight.json")
            if started != {"claim": self.claim_hash, "create_started": True} or preflight != {"claim": self.claim_hash, "collision_absent": True}:
                raise Refused("allocation_custody")
            process, _ = read_json(self.fd, "process.json")
            if (set(process) != {"claim", "process"} or process["claim"] != self.claim_hash
                    or sum(value == process["process"] for value in checked) != 1):
                raise Refused("process_claim")
            # This exact PID/starttime/UID was already checked in the journal.
            # Matching formatted metadata alone never substitutes for that check.
            self.cleanup_phase = "capture"
            current, networks = self.capture(True)
            self.cleanup_phase = "inventory"
            try:
                saved, _ = read_json(self.fd, "inventory.json")
            except FileNotFoundError:
                saved = {"claim": self.claim_hash, "containers": current, "networks": networks}
                write_json(self.fd, "inventory.json", saved)
            if (set(saved) != {"claim", "containers", "networks"} or saved["claim"] != self.claim_hash
                    or saved["containers"] != sorted(set(saved["containers"])) or len(saved["containers"]) > 1
                    or saved["networks"] != sorted(set(saved["networks"])) or len(saved["networks"]) > 1
                    or any(not HEX64.fullmatch(v) for v in saved["containers"] + saved["networks"])
                    or not set(current) <= set(saved["containers"]) or not set(networks) <= set(saved["networks"])):
                raise Refused("inventory_changed")
            self.containers, self.networks = saved["containers"], saved["networks"]
            if not self.commands.groups_absent:
                raise Refused("process_cleanup_unverified")
            delete_needed = bool(current) or self.name in self.kind_names(True)
        # Delete's existing 90s allowance is separate from observation time.
        if delete_needed:
            self.cleanup_phase = "delete"
            token, remaining = self.begin_budget("delete")
            delete_error = None
            try:
                self.commands.run(["kind", "delete", "cluster", "--name", self.name], remaining)
            except BaseException as error:
                delete_error = error
                raise
            finally:
                try:
                    self.finish_budget(token)
                except BaseException:
                    self.commands.reserve.refused = True
                    if delete_error is None:
                        raise
        self.cleanup_phase = "container_absence"
        with self.commands.reserve.observe():
            if self.name in self.kind_names(True) or self.listed_ids(True):
                raise Refused("owned_container_remains")
            for cid in self.containers:
                if self.listed_ids(True, exact_id=cid):
                    raise Refused("owned_container_remains")
            self.cleanup_phase = "network_absence"
            for nid in self.networks:
                code, raw = self.commands.run(["docker", "network", "inspect", "--format",
                    '{{json .Containers}}', nid], 10,
                    observation=True, missing_ok=True)
                if code != 0:
                    raise Refused("network_observation_unknown")
                if network_endpoint_ids(raw).intersection(self.containers):
                    raise Refused("owned_endpoint_remains")
            self.cleanup_phase = "work_retire"
            self.retire_work()
            self.cleaned = self.commands.groups_absent

    def seal(self, cases, primary, cleanup_error=None):
        if input_hashes() != self.inputs:
            raise Refused("source_changed")
        primary = primary if primary in PRIMARY_CATEGORIES else "unknown"
        if type(cases) is not int or not 0 <= cases <= 11:
            raise Refused("case_count")
        if (self.phase not in {"claim", "collision_check", "image_observation", "create", "create_identity", "quota_body", "reconciliation"}
                or type(self.create_timeout) is not bool
                or (self.create_exit is not None and type(self.create_exit) is not int)):
            raise Refused("receipt_shape")
        if primary == "success" and (not self.create_verified or self.create_exit != 0 or self.create_timeout):
            raise Refused("incomplete_success")
        value = dict(format="kanz-quota-owned-lifecycle-v1", **self.identity,
            inputs=self.inputs, claim_sha256=self.claim_hash, node=self.node,
            create_seconds=CREATE_SECONDS, delete_seconds=DELETE_SECONDS,
            shared_cleanup_reserve_seconds=CLEANUP_SECONDS, image=self.image,
            phase=self.phase,
            create_verified=self.create_verified,
            create_exit=self.create_exit, create_timed_out=self.create_timeout,
            cases_passed=cases, primary=primary, cleanup="verified_absent" if self.cleaned else "unknown",
            cleanup_error=cleanup_error is not None, **cleanup_diagnostic(self, cleanup_error),
            lifecycle_groups_absent=self.commands.groups_absent, quota_body_descendants="not_attested",
            owned_container_count=len(self.containers), owned_network_count=len(self.networks),
            passed=primary == "success" and cases == 11 and self.cleaned and cleanup_error is None)
        # Exclusive retention: a foreign or already-published report is never
        # overwritten. Reconciliation reports use a different fixed leaf.
        return write_json(self.parent, REPORT, value)

    def close(self):
        refused = False
        for name in ("fd", "parent"):
            descriptor = getattr(self, name, None)
            setattr(self, name, None)
            if descriptor is not None:
                try:
                    os.close(descriptor)
                except BaseException:
                    refused = True
        if refused:
            raise Refused("descriptor_cleanup_refused")


def main():
    if sys.argv[1:] != ["cleanup"]:
        raise SystemExit("quota lifecycle: fixed cleanup mode required")
    owner = None
    cleanup_error = retention_error = None
    try:
        owner = OwnedLifecycle(cleanup_only=True)
        try:
            owner.cleanup()
        except BaseException as error:
            cleanup_error = error
        try:
            prior, _ = read_json(owner.parent, REPORT)
        except FileNotFoundError:
            owner.seal(0, "unknown", cleanup_error)
        else:
            if (type(prior) is not dict or any(prior.get(k) != v for k, v in owner.identity.items())
                    or prior.get("claim_sha256") != owner.claim_hash or prior.get("inputs") != owner.inputs):
                raise Refused("foreign_report")
        if not owner.cleaned or cleanup_error is not None:
            raise Refused("cleanup_unverified")
    except BaseException:
        # Missing/foreign claims are refusals, never absence attestations.
        raise SystemExit("quota lifecycle: cleanup refused") from None
    finally:
        if owner is not None:
            try:
                # Failure to reconcile does not suppress its closed failure
                # observation. A preexisting foreign output is still refused.
                write_json(owner.parent, "quota-owned-cleanup.json", dict(
                    format="kanz-quota-owned-cleanup-v1", **owner.identity,
                    claim_sha256=owner.claim_hash,
                    cleanup="verified_absent" if owner.cleaned and cleanup_error is None else "unknown",
                    cleanup_error=cleanup_error is not None, **cleanup_diagnostic(owner, cleanup_error),
                    lifecycle_groups_absent=owner.commands.groups_absent, quota_body_descendants="not_attested",
                    owned_container_count=len(owner.containers), owned_network_count=len(owner.networks)))
            except BaseException as error:
                retention_error = error
            finally:
                owner.close()
            if retention_error is not None:
                raise SystemExit("quota lifecycle: retention refused") from None


if __name__ == "__main__":
    main()

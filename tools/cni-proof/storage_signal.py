"""Owned, networkless Linux PID1 control for the storage fixture's TERM handler."""
import json
import os
from pathlib import Path
import re
import sys
import time
import uuid

import migration
import proof

LABEL = "io.kanz.storage-signal"
BODY = "import os,time;print('pid1' if os.getpid()==1 else 'non_pid1',flush=True);assert os.getpid()==1;print('pid1_ready',flush=True);time.sleep(120)"


def startup_error(value):
    if not isinstance(value, str) or not value or len(value) > 8192:
        return "unknown"
    lower = value.lower()
    for category, markers in (
            ("pids_task_limit", ("pids.max", "pids limit", "pids-limit", "task limit", "process limit")),
            ("resource_unavailable", ("resource temporarily unavailable", "eagain")),
            ("permission_denied", ("permission denied", "operation not permitted")),
            ("readonly", ("read-only file system",)),
            ("no_space", ("no space left on device",)),
            ("missing_executable", ("executable file not found",))):
        if any(marker in lower for marker in markers):
            return category
    return "other_runtime_error"


def bounded_number(value, maximum):
    return value if type(value) is int and 0 <= value <= maximum else None


def state_projection(obj):
    state = obj.get("State", {})
    host = obj.get("HostConfig", {})
    status = state.get("Status")
    pid_mode = host.get("PidMode")
    return {"status": status if status in ("created", "running", "paused", "restarting", "removing", "exited", "dead") else "unknown",
            "running": state.get("Running") if type(state.get("Running")) is bool else None,
            "exitCode": state.get("ExitCode") if type(state.get("ExitCode")) is int and 0 <= state["ExitCode"] <= 255 else None,
            "oomKilled": state.get("OOMKilled") if type(state.get("OOMKilled")) is bool else None,
            "init": host.get("Init") if type(host.get("Init")) is bool else None,
            "pidMode": "private" if pid_mode in ("", "private") else "host" if pid_mode == "host" else "unknown",
            "startupError": startup_error(state.get("Error")),
            "pidsLimit": bounded_number(host.get("PidsLimit"), 65536),
            "memory": bounded_number(host.get("Memory"), 1 << 40),
            "nanoCpus": bounded_number(host.get("NanoCpus"), 1024 * 10**9)}


def runtime_preconditions(obj):
    host = obj.get("HostConfig", {})
    if host.get("Init") is not False or host.get("PidMode") not in ("", "private"):
        raise ValueError("direct private PID namespace required")


def sentinel(raw):
    if raw == "pid1\npid1_ready":
        return "pid1_ready"
    first = raw.splitlines()[0] if raw else ""
    if first in ("pid1", "non_pid1"):
        return first
    return "empty" if not raw else "unknown"


def image_stop_signal():
    values = json.loads(migration.command(["docker", "image", "inspect", proof.IMAGE], timeout=10))
    if (not isinstance(values, list) or len(values) != 1 or not isinstance(values[0], dict)
            or not isinstance(values[0].get("Config"), dict)
            or values[0]["Config"].get("StopSignal", "") not in ("", "SIGTERM", "15")):
        raise ValueError("pinned image stop signal is not default TERM")
    identity = values[0].get("Id", "")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", identity):
        raise ValueError("pinned image config identity missing")
    return identity


def owned(name, nonce, identity=None):
    value = json.loads(migration.command(["docker", "inspect", name], timeout=10))
    if len(value) != 1:
        raise ValueError("signal container inventory differs")
    obj = value[0]
    if (not re.fullmatch("[0-9a-f]{64}", obj.get("Id", ""))
            or (identity is not None and obj["Id"] != identity)
            or obj.get("Name") != "/" + name
            or obj.get("Config", {}).get("Labels", {}).get(LABEL) != nonce
            or obj.get("Config", {}).get("Image") != proof.IMAGE):
        raise ValueError("signal container ownership differs")
    return obj


def case(repaired, progress):
    nonce = uuid.uuid4().hex
    name = "kanz-storage-signal-" + nonce
    identity = None
    progress["stage"] = "create"
    progress["cleanup"] = "not_created"
    if migration.command(["docker", "ps", "-aq", "--filter", "name=^/" + name + "$"], timeout=10):
        raise ValueError("preexisting signal container refused")
    progress["cleanup"] = "unknown"
    try:
        # Name/nonce are retained before create, including a lost create response.
        identity = migration.command([
            "docker", "create", "--name", name, "--label", LABEL + "=" + nonce,
            "--network=none", "--read-only", "--user=65532:65532", "--cap-drop=ALL", "--init=false",
            "--security-opt=no-new-privileges", "--memory=64m", "--cpus=0.1", "--pids-limit=8",
            "--log-driver=local", "--log-opt=max-size=16k", "--log-opt=max-file=1",
            proof.IMAGE, "python3", "-I", "-B", "-c",
            (migration.STORAGE_SHUTDOWN if repaired else "") + BODY], timeout=60)
        obj = owned(name, nonce, identity)
        progress["stage"] = "runtime_preconditions"
        runtime_preconditions(obj)
        progress["stage"] = "start"
        migration.command(["docker", "start", identity], timeout=10)
        end = time.monotonic() + 10
        while True:
            progress["stage"] = "readiness_log"
            progress["sentinel"] = sentinel(migration.command(["docker", "logs", "--tail=8", identity], timeout=5))
            if progress["sentinel"] == "pid1_ready":
                break
            progress["stage"] = "readiness_state"
            if time.monotonic() >= end or not owned(name, nonce, identity)["State"]["Running"]:
                raise ValueError("PID1 readiness not observed")
            time.sleep(.1)
        progress["stage"] = "term"
        migration.command(["docker", "kill", "--signal=TERM", identity], timeout=5)
        if repaired:
            if migration.command(["docker", "wait", identity], timeout=10) != "0":
                raise ValueError("registered TERM did not exit cleanly")
            state = owned(name, nonce, identity)["State"]
            if state["Running"] or state["ExitCode"] != 0 or state["OOMKilled"]:
                raise ValueError("registered TERM exit state differs")
        else:
            # A bounded negative observation, not a speed threshold. The old
            # default TERM disposition must leave namespace init running.
            end = time.monotonic() + 1
            while time.monotonic() < end:
                if not owned(name, nonce, identity)["State"]["Running"]:
                    raise ValueError("old PID1 unexpectedly accepted TERM")
                time.sleep(.1)
    except (OSError, ValueError, TypeError, KeyError, RuntimeError, migration.subprocess.TimeoutExpired, proof.ProofError):
        try:
            progress["container"] = state_projection(owned(name, nonce, identity))
        except (OSError, ValueError, TypeError, KeyError, RuntimeError, migration.subprocess.TimeoutExpired, proof.ProofError):
            progress["container"] = "unknown"
        raise
    finally:
        existing = migration.command(["docker", "ps", "-aq", "--no-trunc", "--filter", "name=^/" + name + "$"], timeout=10)
        if existing:
            obj = owned(name, nonce, identity)
            # Force is only teardown of this negative-control container. The
            # Kubernetes storage probe retains its normal grace and wait=true.
            migration.command(["docker", "rm", "-f", obj["Id"]], timeout=10)
        if migration.command(["docker", "ps", "-aq", "--filter", "name=^/" + name + "$"], timeout=10):
            raise ValueError("signal container cleanup incomplete")
        progress["cleanup"] = "verified_absent"


def main():
    progress = {"format": "kanz-storage-pid1-v1", "case": "startup", "stage": "platform", "cleanup": "not_created", "passed": False}
    try:
        source = migration.command(["git", "rev-parse", "HEAD"], timeout=10)
        tree = migration.command(["git", "rev-parse", "HEAD^{tree}"], timeout=10)
        run, attempt = os.environ.get("GITHUB_RUN_ID", ""), os.environ.get("GITHUB_RUN_ATTEMPT", "")
        if (source != os.environ.get("GITHUB_SHA") or not re.fullmatch("[0-9a-f]{40}", source)
                or not re.fullmatch("[0-9a-f]{40}", tree) or not re.fullmatch("[1-9][0-9]*", run)
                or not re.fullmatch("[1-9][0-9]*", attempt)):
            raise ValueError("diagnostic source identity missing")
        progress.update(source=source, tree=tree, runID=run, runAttempt=attempt)
        if sys.platform != "linux":
            raise ValueError("actual Linux PID1 control required")
        progress["stage"] = "pull"
        migration.command(["docker", "pull", "--platform=linux/amd64", proof.IMAGE], timeout=120)
        progress["stage"] = "image_stop_signal"
        progress["imageID"] = image_stop_signal()
        progress["imageStopSignal"] = "TERM"
        for repaired in (False, True):
            progress["case"] = "registered" if repaired else "default"
            case(repaired, progress)
        progress.update(stage="complete", passed=True, oldTerm="running", registeredTerm="exit0")
    except (OSError, ValueError, TypeError, KeyError, RuntimeError, migration.subprocess.TimeoutExpired, proof.ProofError):
        # No raw Docker output, exception message, arguments or paths escape.
        pass
    raw = json.dumps(progress, separators=(",", ":"))
    directory = Path(os.environ["RUNNER_TEMP"])
    if directory.is_symlink() or not directory.is_dir() or len(raw.encode("utf-8")) > 16384:
        raise ValueError("diagnostic output boundary differs")
    with (directory / "storage-pid1.json").open("x", encoding="utf-8") as output:
        output.write(raw + "\n")
    print(raw)
    return 0 if progress["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

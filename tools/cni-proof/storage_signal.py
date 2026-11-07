"""Owned, networkless Linux PID1 control for the storage fixture's TERM handler."""
import json
import re
import sys
import time
import uuid

import migration
import proof

LABEL = "io.kanz.storage-signal"
BODY = "import os,time;assert os.getpid()==1;print('pid1_ready',flush=True);time.sleep(120)"


def image_stop_signal():
    values = json.loads(migration.command(["docker", "image", "inspect", proof.IMAGE], timeout=10))
    if (not isinstance(values, list) or len(values) != 1 or not isinstance(values[0], dict)
            or not isinstance(values[0].get("Config"), dict)
            or values[0]["Config"].get("StopSignal", "") not in ("", "SIGTERM", "15")):
        raise ValueError("pinned image stop signal is not default TERM")
    return "TERM"


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
            "--network=none", "--read-only", "--user=65532:65532", "--cap-drop=ALL",
            "--security-opt=no-new-privileges", "--memory=64m", "--cpus=0.1", "--pids-limit=8",
            "--log-driver=local", "--log-opt=max-size=16k", "--log-opt=max-file=1",
            proof.IMAGE, "python3", "-I", "-B", "-c",
            (migration.STORAGE_SHUTDOWN if repaired else "") + BODY], timeout=60)
        owned(name, nonce, identity)
        progress["stage"] = "ready"
        migration.command(["docker", "start", identity], timeout=10)
        end = time.monotonic() + 10
        while migration.command(["docker", "logs", "--tail=1", identity], timeout=5) != "pid1_ready":
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
        if sys.platform != "linux":
            raise ValueError("actual Linux PID1 control required")
        progress["stage"] = "pull"
        migration.command(["docker", "pull", "--platform=linux/amd64", proof.IMAGE], timeout=120)
        progress["stage"] = "image_stop_signal"
        progress["imageStopSignal"] = image_stop_signal()
        for repaired in (False, True):
            progress["case"] = "registered" if repaired else "default"
            case(repaired, progress)
        progress.update(stage="complete", passed=True, oldTerm="running", registeredTerm="exit0")
    except (OSError, ValueError, TypeError, KeyError, RuntimeError, migration.subprocess.TimeoutExpired, proof.ProofError):
        # No raw Docker output, exception message, arguments or paths escape.
        print(json.dumps(progress, separators=(",", ":")))
        return 1
    print(json.dumps(progress, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

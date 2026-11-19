"""Run the direct binary and Go process supervisor against the same local image."""
import argparse
import contextlib
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from proof import CASES, ROOT, command, container_boundary, oracle, required


BASE = "gcr.io/distroless/base-nossl-debian12@sha256:be40c00dfabd86576d92666e87e406714d5618342de1a0c213ad232de255172e"
ENTRY = "/opt/kanz/bin/curve-worker"


def inspect_image(image, source, bundle):
    info, = json.loads(required(["docker", "image", "inspect", image]))
    base, = json.loads(required(["docker", "image", "inspect", BASE]))
    config = info["Config"]
    layers = info["RootFS"]["Layers"]
    base_layers = base["RootFS"]["Layers"]
    added = layers[len(base_layers):]
    # Some builders emit a tar containing only the end-of-archive blocks for WORKDIR.
    empty_tar = "sha256:" + hashlib.sha256(bytes(1024)).hexdigest()
    if (info["Id"] != image or layers[:len(base_layers)] != base_layers
            or len(added) not in (1, 2) or added[0] == empty_tar or added[1:] not in ([], [empty_tar])
            or (info["Os"], info["Architecture"]) != ("linux", "amd64")
            or config["Entrypoint"] != [ENTRY] or config["User"] != "65532:65532"
            or config["Labels"].get("org.opencontainers.image.revision") != source
            or config["Labels"].get("org.eighred.calibration.test-bundle-sha256") != bundle):
        raise ValueError("immutable application identity or base layers differ")
    return added[0]


@contextlib.contextmanager
def worker(image, source, supervisor=None):
    label = "kanz.julia-proof=" + source
    name = "kanz-safe-bundle-" + uuid.uuid4().hex
    args = ["docker", "create", "--pull=never", "--name", name, "--label", label, "-i",
            "--platform", "linux/amd64", "--network", "none", "--read-only", "--user", "65532:65532",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--cpus", "1",
            "--memory", "1g", "--memory-swap", "1g", "--pids-limit", "64",
            "--tmpfs", "/tmp:rw,noexec,nosuid,size=16777216,mode=1777"]
    if supervisor:
        binary, launcher_digest = supervisor
        args.extend(["--mount", f"type=bind,src={binary},dst=/bounded-parent,readonly",
                     "--entrypoint", "/bounded-parent", image, "-test.v",
                     "-test.run=^TestCompiledApplicationSupervisor$",
                     "-application-launcher-sha256=" + launcher_digest])
    else:
        args.append(image)
    cid = required(args).decode().strip()
    if not re.fullmatch("[a-f0-9]{64}", cid):
        raise ValueError("invalid container ID")
    try:
        info, = json.loads(required(["docker", "inspect", cid]))
        container_boundary(info, image, source)
        mounts = [(m["Type"], m["Destination"], m["RW"]) for m in info["Mounts"] if m["Type"] != "tmpfs"]
        if mounts != ([("bind", "/bounded-parent", False)] if supervisor else []):
            raise ValueError("unexpected external mounts")
        yield cid
    finally:
        required(["docker", "rm", "--force", cid])
        if required(["docker", "ps", "-aq", "--filter", "id=" + cid]).strip():
            raise ValueError("qualification container survived cleanup")


def qualify(image, source, inventory):
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", image) or not re.fullmatch(r"[a-f0-9]{40}", source):
        raise ValueError("expected exact image and Git identities")
    layer = inspect_image(image, source, inventory["bundleSHA256"])
    launcher = inventory["application"]["bin/curve-worker"]["sha256"]
    if not re.fullmatch(r"[a-f0-9]{64}", launcher):
        raise ValueError("invalid launcher digest")
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        oracle(directory, "prepare")
        cases = json.loads((directory / "cases.json").read_bytes())
        if len(cases) != len(CASES) or {item["name"] for item in cases} != CASES:
            raise ValueError("incomplete oracle corpus")
        for item in cases:
            key = item["name"]
            with worker(image, source) as cid:
                code, wire = command(["docker", "start", "--attach", "--interactive", cid],
                                     data=(directory / (key + ".input")).read_bytes(), timeout=30)
                info, = json.loads(required(["docker", "inspect", cid]))
                if info["State"]["OOMKilled"] or info["State"]["ExitCode"] != code:
                    raise ValueError("worker did not exit normally")
            if code != (0 if item["exitZero"] else 1) or bool(wire) != item["exitZero"]:
                raise ValueError("unexpected worker response: " + key)
            (directory / (key + ".output")).write_bytes(wire)
        oracle(directory, "verify")
        executable = directory / "bounded-parent"
        env = {**os.environ, "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}
        required(["go", "test", "-mod=readonly", "-p", "1", "-c", "-tags", "julia_application_qualification",
                  "-trimpath", "-o", str(executable), "./tools/julia_conformance"], cwd=ROOT / "kanz", env=env, timeout=300)
        with worker(image, source, (executable, launcher)) as cid:
            output = required(["docker", "start", "--attach", cid], timeout=90).decode()
            info, = json.loads(required(["docker", "inspect", cid]))
            if info["State"]["OOMKilled"] or info["State"]["ExitCode"] != 0:
                raise ValueError("Go parent failed")
        matches = re.findall(r"--- PASS: TestCompiledApplicationSupervisor/supervise/(\w+) \(", output)
        if len(matches) != len(CASES) or set(matches) != CASES or not output.rstrip().endswith("PASS"):
            raise ValueError("incomplete supervised corpus")
        supervisor = hashlib.sha256(executable.read_bytes()).hexdigest()
    return {"image": image, "source": source, "base": BASE, "applicationLayerDiffID": layer,
            "launcherSHA256": launcher, "supervisorSHA256": supervisor, "cases": sorted(CASES)}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image")
    parser.add_argument("source")
    parser.add_argument("inventory", type=Path)
    parser.add_argument("report", type=Path)
    args = parser.parse_args()
    result = qualify(args.image, args.source, json.loads(args.inventory.read_bytes()))
    with args.report.open("x", encoding="utf-8") as out:
        json.dump(result, out, sort_keys=True, indent=2)
        out.write("\n")
    print("Passed 13 direct and 13 supervised cases, including deadline and cleanup checks.")

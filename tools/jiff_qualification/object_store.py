"""Exercise the object_store duration and timestamp backports in the Kanz lake worker."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile
import uuid

from run import IMAGE


ROOT = Path(__file__).resolve().parents[2]


def prepare(inputs, advisory_output):
    source = inputs / "kanz.tar"
    subprocess.run(["git", "-C", str(ROOT), "archive", "--format=tar", "--output=" + str(source),
                    "HEAD", "rust", "schemas/proto"], check=True, timeout=180)
    for name in ("chrono_advisories.py", "worker_sources.py"):
        (inputs / name).write_bytes(Path(__file__).with_name(name).read_bytes())
    (inputs / "verify.sh").write_bytes(Path(__file__).with_name("object_store.sh").read_bytes())
    from chrono_advisories import CHECKSUM, VERSION, collect, evaluate

    snapshot = collect()
    raw = json.dumps(snapshot, sort_keys=True, separators=(",", ":")).encode()
    (inputs / "chrono-advisories.json").write_bytes(raw)
    if advisory_output is not None:
        advisory_output.write_bytes(raw)
    evaluate(snapshot, VERSION, CHECKSUM)
    for path in inputs.iterdir():
        path.chmod(0o644)
    inputs.chmod(0o755)
    print(f"Publisher and inherited advisory snapshot SHA-256 {hashlib.sha256(raw).hexdigest()}", flush=True)


def qualify(advisory_output=None):
    with tempfile.TemporaryDirectory(prefix="kanz-object-store-") as temp:
        inputs = Path(temp).resolve() / "inputs"
        inputs.mkdir()
        prepare(inputs, advisory_output)
        volume = "kanz-object-store-" + uuid.uuid4().hex
        mount = "type=volume,source=" + volume + ",target=/work"
        args = ["docker", "create", "--pull", "never", "--read-only", "--platform", "linux/amd64",
                "--label", "kanz.jiff=" + volume,
                "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                "--memory", "5g", "--cpus", "3", "--pids-limit", "220",
                "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m,mode=1777",
                "--mount", mount]
        subprocess.run(["docker", "volume", "create", "--label", "kanz.jiff=" + volume, volume], check=True)
        try:
            run_container([*args, "--network", "none", "--user", "0:0", "--cap-add", "CHOWN",
                           IMAGE, "chown", "65532:65532", "/work"], timeout=30)
            for phase, network in (("fetch", "bridge"), ("test", "none")):
                run_container([*args, "--network", network, "--user", "65532:65532",
                               "--mount", f"type=bind,src={inputs},dst=/inputs,readonly",
                               IMAGE, "sh", "/inputs/verify.sh", phase], timeout=1200)
        finally:
            label = subprocess.check_output(["docker", "volume", "inspect", "--format",
                                             '{{index .Labels "kanz.jiff"}}', volume], text=True).strip()
            if label != volume:
                raise RuntimeError("refusing to remove a volume with a different owner")
            subprocess.run(["docker", "volume", "rm", volume], check=True)


def run_container(args, *, timeout):
    cid = subprocess.check_output(args, text=True).strip()
    if not re.fullmatch("[0-9a-f]{64}", cid):
        raise RuntimeError("Docker returned an invalid container ID")
    try:
        subprocess.run(["docker", "start", "--attach", cid], check=True, timeout=timeout)
    finally:
        subprocess.run(["docker", "rm", "--force", "--volumes", cid], check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--advisories", "--published-bridge", dest="advisory_output", metavar="ADVISORY_OUTPUT", type=Path,
                        help="qualify the published fork and save the fresh public advisory inputs")
    qualify(parser.parse_args().advisory_output)

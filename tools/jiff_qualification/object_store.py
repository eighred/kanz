"""Exercise the object_store duration and timestamp backports in the Kanz lake worker."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile
import uuid

from run import IMAGE, checked_input


ROOT = Path(__file__).resolve().parents[2]
UPSTREAM = "https://github.com/apache/arrow-rs-object-store.git"
RELEASE = "7a65b75b0d26fd8a282999462cb7030fb85fdcc3"
DURATION_CHANGE = "67985f7e58ee8246d9f2771e9f52665f97171869"
BACKPORT_TREE = "c91afce7acce0c87ca3147387ba2562a96120440"


def git(directory, *args):
    return subprocess.check_output(["git", "-c", "core.autocrlf=false", "-C", str(directory), *args], timeout=180)


def replace_once(source, old, new):
    if source.count(old) != 1:
        raise ValueError("unexpected upstream source at the adaptation point")
    return source.replace(old, new)


def prepare(work, inputs):
    checkout = work / "apache"
    checkout.mkdir()
    git(checkout, "init", "--quiet")
    git(checkout, "fetch", "--no-tags", "--depth=1", UPSTREAM, RELEASE, DURATION_CHANGE)
    git(checkout, "checkout", "--quiet", "--detach", RELEASE)
    # The 0.13 client imports HeaderValue through reqwest. The later release imports http.
    config = git(checkout, "show", DURATION_CHANGE + ":src/config.rs")
    config = replace_once(config, b"use http::header::HeaderValue;\nuse jiff::fmt::friendly::SpanPrinter;",
                          b"use jiff::fmt::friendly::SpanPrinter;\nuse reqwest::header::HeaderValue;")
    (checkout / "src/config.rs").write_bytes(config)
    # Pin the manifest and the 0.13.2 port of the upstream public timestamp change.
    patch = work / "timestamps.patch"
    patch.write_bytes(Path(__file__).with_name("object_timestamps.patch").read_bytes().replace(b"\r\n", b"\n"))
    git(checkout, "apply", "--index", str(patch))
    git(checkout, "add", "--", "src/config.rs")
    if git(checkout, "write-tree").decode().strip() != BACKPORT_TREE:
        raise ValueError("backported object_store source tree differs")
    git(checkout, "archive", "--format=tar", "--output=" + str(inputs / "object.tar"), BACKPORT_TREE)
    git(ROOT, "archive", "--format=tar", "--output=" + str(inputs / "kanz.tar"), "HEAD",
        "rust/services/lake_snapshot", "rust/Cargo.lock", "schemas/proto/envelope", "schemas/proto/lake")
    zone_url = ("https://raw.githubusercontent.com/apache/arrow-rs/"
                "1041671b007886b1d0b13794160c6af9c42bddec/arrow-array/src/timezone.rs")
    (inputs / "timezone.rs").write_bytes(checked_input(
        None, "timezone.rs", zone_url, 49562,
        "1f02dae57e17e81de748e91a80b4050625d9e882ac0df533448b7261055e5711"))
    for name in ("lake_sources.py", "named_zone.rs", "object_metadata.rs"):
        (inputs / name).write_bytes(Path(__file__).with_name(name).read_bytes())
    # Do not mount either repository or its Git configuration inside the compiler.
    (inputs / "verify.sh").write_bytes(Path(__file__).with_name("object_store.sh").read_bytes())


def qualify(advisory_output=None):
    with tempfile.TemporaryDirectory(prefix="kanz-object-store-") as temp:
        root = Path(temp).resolve()
        inputs = root / "inputs"
        inputs.mkdir()
        prepare(root, inputs)
        mode = "upstream"
        if advisory_output is not None:
            from chrono_advisories import CHECKSUM, VERSION, collect, evaluate

            snapshot = collect()
            raw = json.dumps(snapshot, sort_keys=True, separators=(",", ":")).encode()
            advisory_output.write_bytes(raw)
            evaluate(snapshot, VERSION, CHECKSUM)
            (inputs / "chrono-advisories.json").write_bytes(raw)
            for name in ("chrono_advisories.py", "published_bridge.py", "shared_arrow.rs"):
                (inputs / name).write_bytes(Path(__file__).with_name(name).read_bytes())
            print(f"Publisher and inherited advisory snapshot SHA-256 {hashlib.sha256(raw).hexdigest()}", flush=True)
            mode = "published"
        for path in inputs.iterdir():
            path.chmod(0o644)
        inputs.chmod(0o755)
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
                               IMAGE, "sh", "/inputs/verify.sh", phase, mode], timeout=1200)
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
    parser.add_argument("--published-bridge", metavar="ADVISORY_OUTPUT", type=Path,
                        help="qualify the published fork and save the fresh public advisory inputs")
    qualify(parser.parse_args().published_bridge)

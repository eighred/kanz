#!/usr/bin/env python3
"""Quota caller qualification against an owned, disposable kind API server.

Requires kind, Docker, kubectl, bash and jq. Never uses the current kubeconfig.
Only the unchanged tenantctl quota functions run; broker/identity lifecycle
qualification remains owned by tools/native-kafka-provision.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
import uuid


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools/feed-identity"))
from proof import NODE

SOURCE = ROOT / "kanz/infra/tenancy/tenantctl.sh"
QUOTA = {"rate_per_sec": 50, "burst": 100, "max_in_flight": 64}

# This wrapper forwards every ordinary operation to the owned real API. Faults
# affect one call boundary, not a simulated Kubernetes resourceVersion model.
WRAPPER = r'''#!/usr/bin/env python3
import os, pathlib, subprocess, sys, time
args = sys.argv[1:]
base = [os.environ["REAL_KUBECTL"], "--kubeconfig", os.environ["KUBECONFIG"],
        "--context", os.environ["QUOTA_CONTEXT"]]
mode = os.environ.get("FAULT", "")
root = pathlib.Path(os.environ["PROBE_DIR"])
tenant = os.environ["TENANT"]
read = "get" in args and "cm" in args
write = "create" in args or "replace" in args
if write:
    with (root / (tenant + ".writes")).open("a") as f:
        f.write("write\n")
if read and mode == "denied":
    args = ["--as=system:serviceaccount:quota-proof:denied"] + args
if read and mode == "unavailable":
    args = [x for x in args if not x.startswith("--request-timeout=")]
    args = ["--server=https://127.0.0.1:1", "--request-timeout=1s"] + args
if "replace" in args and mode == "conflicts":
    subprocess.run(base + ["-n", "quota-proof", "annotate", "cm", "quotas",
        "conflict=" + str(time.time_ns()), "--overwrite"], check=True,
        stdout=subprocess.DEVNULL, timeout=10)
result = subprocess.run(base + args, input=sys.stdin.buffer.read() if write else None,
                        capture_output=True, timeout=15)
if read and mode == "race" and not (root / (tenant + ".ready")).exists():
    (root / (tenant + ".ready")).touch()
    deadline = time.monotonic() + 10
    while len(list(root.glob("*.ready"))) < 2:
        if time.monotonic() > deadline:
            sys.exit("competing read did not arrive")
        time.sleep(.02)
sys.stdout.buffer.write(result.stdout)
sys.stderr.buffer.write(result.stderr)
if write and mode == "lost-ack" and result.returncode == 0:
    sys.exit(1)
sys.exit(result.returncode)
'''


class QuotaCases(unittest.TestCase):
    root: Path
    env: dict
    kubectl: list

    def api(self, *args, value=None):
        result = subprocess.run(
            self.kubectl + list(args), input=json.dumps(value) if value is not None else None,
            text=True, capture_output=True, timeout=20, check=True,
        )
        return result.stdout

    def setUp(self):
        self.probe = Path(tempfile.mkdtemp(dir=self.root))
        self.api("-n", "quota-proof", "delete", "cm", "quotas", "--ignore-not-found")

    def seed(self, quota=None):
        self.api("create", "-f", "-", value={
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "quotas", "namespace": "quota-proof",
                         "labels": {"preserve": "yes"}},
            "data": {"quotas.json": json.dumps({"neighbor": QUOTA}) if quota is None else quota,
                     "unrelated": "retain"},
        })

    def state(self):
        return json.loads(self.api("-n", "quota-proof", "get", "cm", "quotas", "-o", "json"))

    def start(self, tenant="alpha", operation="onboard_quota", fault=""):
        env = dict(self.env, TENANT=tenant, PROBE_DIR=str(self.probe), FAULT=fault)
        return subprocess.Popen(
            ["bash", str(self.root / "caller.sh"), operation], env=env,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )

    def finish(self, process, success=True):
        try:
            out, err = process.communicate(timeout=120)
        except subprocess.TimeoutExpired:
            process.kill()
            process.communicate()
            self.fail("quota caller exceeded bound")
        self.assertEqual(process.returncode == 0, success, out + err)
        return out + err

    def writes(self, tenant="alpha"):
        path = self.probe / (tenant + ".writes")
        return len(path.read_text().splitlines()) if path.exists() else 0

    def test_create_missing(self):
        self.finish(self.start())
        self.assertEqual(json.loads(self.state()["data"]["quotas.json"]), {"alpha": QUOTA})

    def test_preserve_neighbors_and_metadata(self):
        self.seed()
        before = self.state()
        self.finish(self.start())
        after = self.state()
        self.assertEqual(json.loads(after["data"]["quotas.json"]), {"alpha": QUOTA, "neighbor": QUOTA})
        self.assertEqual(after["data"]["unrelated"], "retain")
        self.assertEqual(after["metadata"]["labels"], before["metadata"]["labels"])
        self.assertEqual(after["metadata"]["uid"], before["metadata"]["uid"])

    def test_failed_reads_never_write(self):
        self.seed()
        before = self.state()
        for fault in ("denied", "unavailable"):
            with self.subTest(fault=fault):
                self.finish(self.start(fault=fault), success=False)
                self.assertEqual(self.writes(), 0)
                self.assertEqual(self.state(), before)

    def test_malformed_maps_never_write(self):
        for value in ("{", "null", "[]", '"text"', '{} {}'):
            with self.subTest(value=value):
                self.api("-n", "quota-proof", "delete", "cm", "quotas", "--ignore-not-found")
                self.seed(value)
                before = self.state()
                self.finish(self.start(), success=False)
                self.assertEqual(self.writes(), 0)
                self.assertEqual(self.state(), before)

    def test_missing_data_key_never_writes(self):
        self.seed()
        self.api("-n", "quota-proof", "patch", "cm", "quotas", "--type=json", "-p",
                 '[{"op":"remove","path":"/data/quotas.json"}]')
        self.finish(self.start(), success=False)
        self.assertEqual(self.writes(), 0)

    def compete(self):
        # Both first reads complete before either write. The API, not this
        # fixture, decides the create/replace conflict and increments versions.
        first = self.start("alpha", fault="race")
        second = self.start("beta", fault="race")
        try:
            self.finish(first)
            self.finish(second)
        finally:
            for process in (first, second):
                if process.poll() is None:
                    process.kill()
                    process.communicate()
        self.assertEqual(self.writes("alpha") + self.writes("beta"), 3)
        return json.loads(self.state()["data"]["quotas.json"])

    def test_competing_creates(self):
        self.assertEqual(self.compete(), {"alpha": QUOTA, "beta": QUOTA})

    def test_competing_replacements(self):
        self.seed()
        self.assertEqual(self.compete(), {"alpha": QUOTA, "beta": QUOTA, "neighbor": QUOTA})

    def test_conflicts_are_bounded(self):
        self.seed()
        output = self.finish(self.start(fault="conflicts"), success=False)
        self.assertIn("exhausted 5", output)
        self.assertEqual(self.writes(), 5)
        self.assertEqual(json.loads(self.state()["data"]["quotas.json"]), {"neighbor": QUOTA})

    def test_unknown_write_is_not_retried(self):
        self.seed()
        self.finish(self.start(fault="lost-ack"), success=False)
        self.assertEqual(self.writes(), 1)
        self.assertEqual(json.loads(self.state()["data"]["quotas.json"]), {"alpha": QUOTA, "neighbor": QUOTA})

    def test_offboard_only_removes_selected_entry(self):
        self.seed(json.dumps({"alpha": QUOTA, "neighbor": QUOTA}))
        self.finish(self.start(operation="offboard_quota"))
        self.assertEqual(json.loads(self.state()["data"]["quotas.json"]), {"neighbor": QUOTA})
        self.assertEqual(self.state()["data"]["unrelated"], "retain")

    def test_offboard_missing_does_not_create(self):
        self.finish(self.start(operation="offboard_quota"))
        self.assertEqual(self.writes(), 0)
        self.assertEqual(self.api("-n", "quota-proof", "get", "cm", "quotas", "--ignore-not-found", "-o", "json"), "")


def main():
    for tool in ("kind", "kubectl", "bash", "jq"):
        if not shutil.which(tool):
            raise SystemExit(f"required qualification dependency missing: {tool}")
    source = SOURCE.read_bytes()
    marker = '\ncase "${1:-}" in\n'
    prefix, dispatch = source.decode().replace("\r\n", "\n").rsplit(marker, 1)
    if "onboard_quota" not in dispatch or "offboard_quota" not in dispatch:
        raise SystemExit("tenantctl dispatch changed; review caller extraction")
    name = "quota-proof-" + uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix=name + "-") as directory:
        root = Path(directory)
        config = root / "kubeconfig"
        kubectl = [str(Path(shutil.which("kubectl")).resolve()), "--kubeconfig", str(config), "--context", "kind-" + name]
        (root / "bin").mkdir()
        wrapper = root / "bin/kubectl"
        wrapper.write_text(WRAPPER)
        wrapper.chmod(0o700)
        (root / "caller.sh").write_text(prefix + '\n"$1"\n')
        QuotaCases.root = root
        QuotaCases.kubectl = kubectl
        QuotaCases.env = {
            "PATH": str(root / "bin") + os.pathsep + os.environ["PATH"],
            "HOME": str(root), "TMPDIR": str(root), "LC_ALL": "C",
            "KUBECONFIG": str(config), "QUOTA_CONTEXT": "kind-" + name,
            "REAL_KUBECTL": kubectl[0], "SVC_NS": "quota-proof", "QUOTA_CM": "quotas",
        }
        try:
            subprocess.run(["kind", "create", "cluster", "--name", name, "--image", NODE,
                            "--kubeconfig", str(config), "--wait", "90s"], check=True, timeout=240)
            subprocess.run(kubectl + ["create", "namespace", "quota-proof"], check=True, timeout=20)
            result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(QuotaCases))
            if not result.wasSuccessful() or result.skipped or result.testsRun != 11:
                raise SystemExit("quota qualification incomplete or failed")
            if SOURCE.read_bytes() != source:
                raise SystemExit("source changed during qualification")
        finally:
            # Exact generated name only; a partial cluster creation is also ours.
            subprocess.run(["kind", "delete", "cluster", "--name", name], check=True, timeout=90)
    print(json.dumps({"tenantctl_sha256": hashlib.sha256(source).hexdigest(),
                      "node_image": NODE, "cases_passed": 11, "owned_cluster_removed": name}))


if __name__ == "__main__":
    main()

"""Provision and read back the local single-account JetStream fixture."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys

import yaml

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "tools" / "nats_gitops_proof"))
from proof import expected_streams, check_inventory


def verify(mode, endpoint):
    address = re.fullmatch(r"nats://127\.0\.0\.1:([1-9][0-9]{0,4})", endpoint)
    if not address or int(address[1]) > 65535:
        raise ValueError("expected local backing loopback endpoint")
    manifest = ROOT / "kanz/infra/nats/bootstrap-job.yaml"
    objects = list(yaml.safe_load_all(manifest.read_text(encoding="utf-8")))
    script, = (obj["data"]["bootstrap.sh"] for obj in objects if obj and obj["kind"] == "ConfigMap"
               and "bootstrap.sh" in obj.get("data", {}))
    job, = (obj for obj in objects if obj and obj["kind"] == "Job" and obj["metadata"]["name"] == "nats-bootstrap")
    image, = (item["image"] for item in job["spec"]["template"]["spec"]["containers"] if item["name"] == "bootstrap")
    expected = expected_streams(script, 1)
    client = subprocess.check_output(["docker", "create", "--network", "host", "-e", f"NATS_URL={endpoint}",
                                      "-e", "NATS_REPLICAS=1", image, "sleep", "600"], text=True).strip()
    try:
        subprocess.run(["docker", "start", client], check=True, stdout=subprocess.DEVNULL, timeout=30)
        if mode == "bootstrap":
            subprocess.run(["docker", "exec", "-i", client, "sh", "-s"], input=script.encode(),
                           stdout=subprocess.DEVNULL, check=True, timeout=180)

        def read(*args):
            return json.loads(subprocess.check_output(["docker", "exec", client, "nats", "--server", endpoint,
                                                       *args, "--json"], timeout=15))

        names = read("stream", "ls", "--all")
        if not isinstance(names, list) or len(names) != len(expected) or set(names) != set(expected):
            raise ValueError("live stream inventory differs from canonical declarations")
        actual = {name: read("stream", "info", name) for name in names}
        check_inventory(expected, actual)
        print(f"Verified {len(names)} live JetStream configurations on {endpoint}")
    finally:
        subprocess.run(["docker", "rm", "-fv", client], stdout=subprocess.DEVNULL, check=True, timeout=30)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("bootstrap", "check"))
    parser.add_argument("endpoint")
    args = parser.parse_args()
    try:
        verify(args.mode, args.endpoint)
    except ValueError as error:
        parser.exit(1, f"Backing JetStream refused: {error}\n")

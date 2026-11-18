"""Select the rig cache only on a new PVC or a verified reapplication."""
import json
from pathlib import Path
import re
import subprocess
import sys

import yaml

NAMESPACE = "kanz-messaging"
PVC = "data-redis-0"
ORIGINAL_CLAIM = "kanz.eighred.com/first-install-pvc-uid"
MANAGER = "--field-manager=kanz-rig-cache"


class Refused(RuntimeError):
    pass


def run(args, data=None):
    result = subprocess.run(args, input=data, capture_output=True, text=True, encoding="utf-8")
    if result.returncode:
        raise Refused("cache manifest render or Kubernetes API request failed")
    return result.stdout.strip()


def selection(k, desired):
    container, = desired["spec"]["template"]["spec"]["containers"]
    template, = desired["spec"]["volumeClaimTemplates"]
    if (desired["metadata"]["name"] != "redis" or desired["metadata"]["namespace"] != NAMESPACE
            or desired["spec"]["replicas"] != 1 or desired["spec"].get("ordinals", {}).get("start", 0) != 0
            or template["metadata"]["name"] != "data" or container["name"] != "redis"
            or not re.fullmatch(r"valkey/valkey:[^@]+@sha256:[0-9a-f]{64}", container["image"])
            or set(template["spec"]) != {"accessModes", "resources"}):
        raise Refused("unexpected first-install cache or claim selection")

    def get(resource):
        raw = k("get", resource, "--ignore-not-found=true", "-o", "json")
        if not raw:
            return None
        obj = json.loads(raw)
        if not isinstance(obj, dict) or not obj.get("metadata", {}).get("uid"):
            raise Refused("Kubernetes returned an unidentifiable cache resource")
        return obj

    current = get("statefulset/redis")
    claim = get("pvc/" + PVC)
    if current is None and claim is None:
        return None
    if current is None or claim is None:
        raise Refused("incomplete cache ownership; retained-state migration review required")

    metadata = current["metadata"]
    uid = claim["metadata"].get("uid")
    containers = current["spec"]["template"]["spec"]["containers"]
    claims = current["spec"]["volumeClaimTemplates"]
    if (not uid or metadata.get("annotations", {}).get(ORIGINAL_CLAIM) != uid
            or metadata.get("deletionTimestamp") or claim["metadata"].get("deletionTimestamp")
            or not metadata.get("uid") or not metadata.get("resourceVersion")
            or [(c["name"], c["image"]) for c in containers] != [("redis", container["image"])]
            or current["spec"].get("replicas") != 1 or current["spec"].get("ordinals", {}).get("start", 0) != 0
            or current["spec"]["serviceName"] != desired["spec"]["serviceName"]
            or current["spec"]["selector"] != desired["spec"]["selector"]
            or [t["metadata"]["name"] for t in claims] != ["data"]
            or any(claims[0]["spec"].get(key) != value for key, value in template["spec"].items())):
        raise Refused("cache image or original PVC identity differs; retained-state migration review required")
    return metadata


def install(k, resources, preflight=False):
    desired, = [doc for doc in resources if doc["kind"] == "StatefulSet"]
    owner = selection(k, desired)
    if preflight:
        return
    if owner is None:
        template, = desired["spec"]["volumeClaimTemplates"]
        # Reserve the exact ordinal claim with create. A competing or orphaned
        # PVC can never be adopted by this first-install invocation.
        claim = {"apiVersion": "v1", "kind": "PersistentVolumeClaim",
                 "metadata": {"name": PVC, "namespace": NAMESPACE}, "spec": template["spec"]}
        uid = k("create", MANAGER, "-f", "-", "-o", "jsonpath={.metadata.uid}", data=json.dumps(claim))
        if not uid:
            raise Refused("new PVC identity unavailable")
        desired["metadata"].setdefault("annotations", {})[ORIGINAL_CLAIM] = uid
        # A concurrent install/restore must fail on name collision, never patch
        # another engine. A partial installation requires explicit inspection.
        k("create", MANAGER, "-f", "-", data=json.dumps(desired))
    else:
        # A strategic patch reconciles only declared pod-template fields; the
        # API keeps defaulted immutable claim templates and unrelated fields.
        # UID and resourceVersion make replacement or concurrent edits fail.
        patch = {"metadata": {"uid": owner["uid"], "resourceVersion": owner["resourceVersion"]},
                 "spec": {"template": desired["spec"]["template"]}}
        k("patch", "statefulset/redis", "--type=strategic", MANAGER, "--patch", json.dumps(patch))
    for doc in resources:
        if doc["kind"] != "StatefulSet":
            k("apply", "-f", "-", data=json.dumps(doc))


def main():
    if len(sys.argv) != 3 or sys.argv[1] not in ("preflight", "install") or not sys.argv[2].startswith("kind-"):
        sys.exit("usage: rig_install.py preflight|install kind-CONTEXT")
    root = Path(__file__).resolve().parents[2]
    rendered = run([sys.executable, str(root / "tools/rig_dev_patch.py"), str(root / "kanz/infra/messaging/redis.yaml")])

    def k(*args, data=None):
        return run(["kubectl", "--context", sys.argv[2], "-n", NAMESPACE, *args], data)

    install(k, list(yaml.safe_load_all(rendered)), preflight=sys.argv[1] == "preflight")


if __name__ == "__main__":
    try:
        main()
    except (Refused, OSError, KeyError, TypeError, ValueError) as err:
        sys.exit("FATAL: " + (str(err) if isinstance(err, Refused) else "cache preflight or install failed"))

"""Synthetic API stub for test_tv_sync_preparation; never contacts Kubernetes."""
import json
import os
from pathlib import Path
import sys

root = Path(os.environ["TV_PREP_FIXTURE"])
state_path = root / "state.json"
state = json.loads(state_path.read_text())
scenario = state["scenario"]
a = sys.argv[1:]
assert a.pop(0) == "kubectl"
a = [x for x in a if not x.startswith("--request-timeout=")]
if "-n" in a:
    i = a.index("-n")
    assert a[i + 1] == "kanz-data"
    del a[i:i + 2]


def finish(value=None, code=0):
    state_path.write_text(json.dumps(state))
    if value is not None:
        print(value if isinstance(value, str) else json.dumps(value))
    sys.exit(code)


def key_for(value):
    return value["kind"].lower() + "/" + value["metadata"]["name"]


def uid(value):
    return value["metadata"]["uid"]


if a[0] == "get":
    target = a[1]
    if target == "namespace/kube-system":
        finish("wrong" if scenario == "wrong-cluster" else "86ea9839-5806-461d-b2d1-afd1ff5d87ff")
    if target == "serviceaccount/postgres-provisioner":
        finish({})
    if target in ("job/postgres-provisioner", "job/postgres-migrations"):
        finish({"metadata": {"uid": "11111111-1111-1111-1111-111111111112"}, "status": {"active": 1 if scenario == "busy-base" else 0, "conditions": [{"type": "Complete", "status": "True"}]}})
    if target == "pods":
        selector = a[a.index("-l") + 1].split("=", 1)[1]
        matches = [v for k, v in state["objects"].items() if k.startswith("job/") and uid(v) == selector]
        items = []
        for job in matches:
            image = job["spec"]["template"]["spec"]["containers"][0]["image"]
            container = job["spec"]["template"]["spec"]["containers"][0]["name"]
            owner = "foreign" if scenario in ("stale-pod", "foreign-replacement", "concurrent-configmap") else selector
            if scenario == "concurrent-configmap":
                state["objects"]["configmap/postgres-provisioner"]["data"] = {"provision.sh": "concurrent change"}
            items.append({"metadata": {"ownerReferences": [{"kind": "Job", "name": job["metadata"]["name"], "uid": owner, "controller": True}]}, "spec": {"containers": [{"name": container, "image": image}]}, "status": {"phase": "Succeeded", "containerStatuses": [{"name": container, "imageID": "docker-pullable://" + ("wrong" if scenario == "wrong-image" else image), "state": {"terminated": {"exitCode": 0}}}]}})
        finish({"items": items})
    if target == "job/tv-sync-provisioner" and scenario == "busy-existing" and target not in state["objects"]:
        finish({"metadata": {"uid": "11111111-1111-1111-1111-111111111113"}, "status": {"active": 1}})
    if target == "secretproviderclass/tv-sync-role-passwords" and scenario == "api-error":
        finish(code=1)
    value = state["objects"].get(target)
    if target == "job/tv-sync-provisioner" and value is not None and scenario == "wrong-job-uid":
        value["metadata"]["uid"] = "44444444-4444-4444-4444-444444444444"
    if value is None:
        if "--ignore-not-found=true" in a:
            finish()
        finish(code=1)
    finish(value)
if a[0] == "wait":
    finish()
if a[0] in ("create", "apply") and any(x.startswith("--dry-run=") for x in a):
    desired = json.loads((root / "desired.json").read_text())
    if scenario == "json-stream":
        for item in desired["items"]:
            print(json.dumps(item))
        finish()
    finish(desired)
if a[0] in ("create", "replace"):
    value = json.load(sys.stdin)
    key = key_for(value)
    existing = state["objects"].get(key)
    if a[0] == "create" and existing:
        finish(code=1)
    if a[0] == "replace" and (not existing or existing["metadata"]["resourceVersion"] != value["metadata"]["resourceVersion"]):
        finish(code=1)
    value["metadata"].setdefault("uid", "22222222-2222-2222-2222-" + str(len(state["mutations"])).zfill(12))
    value["metadata"]["resourceVersion"] = str(len(state["mutations"]) + 2)
    if value["kind"] == "Job":
        value["status"] = {"active": 0, "conditions": [{"type": "Failed" if scenario == "failed-migration" and key.endswith("migration") else "Complete", "status": "True"}]}
        if scenario == "foreign-replacement":
            value["metadata"]["annotations"]["kanz.io/preparation-owner"] = "another-owner"
    state["objects"][key] = value
    state["mutations"].append(a[0] + ":" + key)
    if scenario == "create-response-lost" and key == "job/tv-sync-provisioner":
        finish(code=1)
    finish(value)
if a[0] == "delete":
    path = a[a.index("--raw") + 1]
    collection, name = path.split("/")[-2:]
    kind = {"jobs": "job", "secretproviderclasses": "secretproviderclass"}[collection]
    key = kind + "/" + name
    options = json.load(sys.stdin)
    assert options["preconditions"]["uid"] == uid(state["objects"][key])
    state["mutations"].append("delete:" + key)
    del state["objects"][key]
    finish()
raise AssertionError(a)

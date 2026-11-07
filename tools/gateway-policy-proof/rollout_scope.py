"""Shared fixed Tokyo inventory checks; no mutation or historical inference."""
import json

import rollout_probe as probe
import transaction as tx

CLUSTER_UID = "86ea9839-5806-461d-b2d1-afd1ff5d87ff"
APPS = {"kanz-services": ("accounting", "audit", "compliance", "api-gateway", "web-bff"),
        "kanz-messaging": ("nats", "redis")}


def snapshot(k, governed):
    result = {}
    deployments = json.loads(k("-n", tx.NAMESPACE, "get", "deployments", "-o", "json"))["items"]
    replicasets = json.loads(k("-n", tx.NAMESPACE, "get", "replicasets", "-o", "json"))["items"]
    if len(deployments) > 128 or len(replicasets) > 256:
        raise tx.Refusal("controller inventory exceeds bound")
    for namespace, names in APPS.items():
        items = json.loads(k("-n", namespace, "get", "pods", "-o", "json"))["items"]
        if len(items) > 128:
            raise tx.Refusal("workload inventory exceeds bound")
        for app in names:
            pods = [p for p in items if p["metadata"].get("labels", {}).get("app") == app
                    and probe.OWNER not in p["metadata"].get("labels", {})]
            if len(pods) != 1:
                raise tx.Refusal("expected singleton workload differs")
            p = pods[0]
            if p["metadata"].get("deletionTimestamp") or p.get("status", {}).get("phase") != "Running":
                raise tx.Refusal("workload not running")
            if not any(c.get("type") == "Ready" and c.get("status") == "True" for c in p["status"].get("conditions", [])):
                raise tx.Refusal("workload not ready")
            result[(namespace, app)] = {"uid": p["metadata"]["uid"], "labels": p["metadata"].get("labels", {}),
                "ip": p["status"]["podIP"], "containers": [
                    {key: c.get(key) for key in ("name", "imageID", "ready", "restartCount")}
                    for c in p["status"].get("containerStatuses", [])]}
            if namespace == tx.NAMESPACE:
                lineage = probe.workload_lineage(p, deployments, replicasets, governed)
                if lineage["name"] != app:
                    raise tx.Refusal("workload application/controller mismatch")
                result[(namespace, app)]["lineage"] = lineage
            if not result[(namespace, app)]["containers"] or any(not c["ready"] or not c["imageID"] for c in result[(namespace, app)]["containers"]):
                raise tx.Refusal("workload container readiness unknown")
    return result


def outside_policies(k):
    items = json.loads(k("get", "networkpolicies", "-A", "-o", "json"))["items"]
    return tx.inventory([p for p in items if p["metadata"]["namespace"] != tx.NAMESPACE])


def service_snapshot(k):
    items = json.loads(k("get", "services", "-A", "-o", "json"))["items"]
    if len(items) > 256:
        raise tx.Refusal("service inventory exceeds bound")
    return {p["metadata"]["namespace"] + "/" + p["metadata"]["name"]:
            {"uid": p["metadata"]["uid"], "resourceVersion": p["metadata"]["resourceVersion"], "spec": p["spec"]}
            for p in items}



"""Shared fixed Tokyo inventory checks; no mutation or historical inference."""
import json
import ipaddress
import re

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
                    and OWNER not in p["metadata"].get("labels", {})]
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
                lineage = workload_lineage(p, deployments, replicasets, governed)
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



OWNER = "kanz.eighred.com/gateway-policy-probe"


def literal_address(value):
    """Compare IP spellings (including mapped IPv4); never resolve names."""
    try:
        address = ipaddress.ip_address(value)
    except ValueError:
        return value
    if isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped:
        address = address.ipv4_mapped
    return str(address)


def endpoint_boundary(slices, addresses, require_absent=False, owned_uids=()):
    if not isinstance(slices, list) or len(slices) > 512:
        raise ValueError("EndpointSlice inventory exceeds bound")
    observed = {literal_address(value) for value in addresses}
    for item in slices:
        if not isinstance(item, dict):
            raise ValueError("invalid EndpointSlice shape")
        # discovery/v1 optional nil Endpoints serializes as null (no omitempty).
        endpoints = item.get("endpoints")
        if endpoints is None:
            endpoints = []
        if not isinstance(endpoints, list) or len(endpoints) > 1000:
            raise ValueError("invalid EndpointSlice shape")
        for endpoint in endpoints:
            if not isinstance(endpoint, dict):
                raise ValueError("invalid EndpointSlice shape")
            values = endpoint.get("addresses")
            conditions = endpoint.get("conditions", {})
            if (not isinstance(values, list) or not 1 <= len(values) <= 100
                    or any(not isinstance(v, str) or not 1 <= len(v) <= 253 for v in values)
                    or not isinstance(conditions, dict)):
                raise ValueError("invalid EndpointSlice shape")
            target = endpoint.get("targetRef")
            if target is not None and not isinstance(target, dict):
                raise ValueError("invalid endpoint target reference")
            if require_absent and target is not None and target.get("uid") in owned_uids:
                raise ValueError("owned Pod still referenced by EndpointSlice")
            if {literal_address(value) for value in values} & observed:
                if require_absent or conditions.get("ready") is not False:
                    raise ValueError("probe may participate in Service routing")


def workload_lineage(pod, deployments, replicasets, expected):
    """Source specs/controller lineage, not private-image verification."""
    def images(spec):
        result = {}
        for key in ("containers", "initContainers"):
            entries = spec.get(key, [])
            values = {entry["name"]: entry["image"] for entry in entries}
            if len(values) != len(entries) or any(not re.fullmatch(r"[a-z0-9./:_-]+@sha256:[0-9a-f]{64}", v) for v in values.values()):
                raise ValueError("workload image references are not uniquely pinned")
            result[key] = values
        if not result["containers"] or spec.get("hostNetwork"):
            raise ValueError("workload network/image boundary differs")
        return result
    def owner(obj, kind, parents):
        refs = obj["metadata"].get("ownerReferences", [])
        if len(refs) != 1 or refs[0].get("kind") != kind or refs[0].get("controller") is not True:
            raise ValueError("workload controller ownership differs")
        ref = refs[0]
        matches = [v for v in parents if v["metadata"].get("name") == ref.get("name")
                   and v["metadata"].get("uid") == ref.get("uid")
                   and v["metadata"].get("namespace") == pod["metadata"]["namespace"]]
        if len(matches) != 1 or matches[0]["metadata"].get("deletionTimestamp"):
            raise ValueError("workload controller UID is unresolved")
        return matches[0]
    rs = owner(pod, "ReplicaSet", replicasets)
    deployment = owner(rs, "Deployment", deployments)
    name = deployment["metadata"]["name"]
    if name not in expected:
        raise ValueError("workload deployment not governed by source")
    wanted = expected[name]
    template = deployment["spec"]["template"]
    if (template["metadata"].get("labels") != wanted["labels"]
            or deployment["spec"]["selector"] != wanted["selector"]
            or images(template["spec"]) != wanted["images"]):
        raise ValueError("live Deployment differs from rendered source")
    labels = rs["spec"]["template"]["metadata"].get("labels", {})
    hashed = labels.get("pod-template-hash")
    if (not isinstance(hashed, str) or not re.fullmatch(r"[a-z0-9]{1,63}", hashed)
            or labels != {**wanted["labels"], "pod-template-hash": hashed}
            or rs["spec"]["selector"] != {"matchLabels": {**wanted["selector"]["matchLabels"], "pod-template-hash": hashed}}
            or pod["metadata"].get("labels") != labels
            or images(rs["spec"]["template"]["spec"]) != wanted["images"]
            or images(pod["spec"]) != wanted["images"]):
        raise ValueError("Pod/ReplicaSet differs from rendered deployment")
    return {"name": name, "deployment": deployment["metadata"]["uid"], "replicaSet": rs["metadata"]["uid"],
            "images": wanted["images"], "podOwners": pod["metadata"]["ownerReferences"],
            "replicaSetOwners": rs["metadata"]["ownerReferences"]}

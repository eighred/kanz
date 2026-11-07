"""Network-only probes excluded from ready Service endpoints."""
import ipaddress
import re

IMAGE = "python:3.13.18-alpine3.24@sha256:7712cfd23e0501f5cb58faa5e6e0d515c9939bdbce866921799b31e696a1d13c"
GATE = "kanz.eighred.com/probe-not-serving"
from rollout_scope import OWNER, literal_address, endpoint_boundary, workload_lineage


def caller_pod(namespace, app, nonce):
    if not re.fullmatch(r"[0-9a-f]{32}", nonce) or app not in ("api-gateway", "web-bff", "policy-untrusted", "policy-observer"):
        raise ValueError("invalid probe identity")
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,62}", namespace):
        raise ValueError("invalid probe namespace")
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {
        "name": "gateway-policy-" + app + "-" + nonce[:12], "namespace": namespace,
        "labels": {"app": app, OWNER: nonce}},
        "spec": {"automountServiceAccountToken": False, "restartPolicy": "Never",
                 "activeDeadlineSeconds": 480, "readinessGates": [{"conditionType": GATE}],
                 "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                                     "seccompProfile": {"type": "RuntimeDefault"}},
                 "containers": [{"name": "probe", "image": IMAGE, "imagePullPolicy": "IfNotPresent",
                                 "command": ["python3", "-I", "-B", "-c", "import time; time.sleep(450)"],
                                 "resources": {"requests": {"cpu": "10m", "memory": "16Mi"},
                                               "limits": {"cpu": "100m", "memory": "48Mi"}},
                                 "securityContext": {"readOnlyRootFilesystem": True,
                                                     "allowPrivilegeEscalation": False,
                                                     "capabilities": {"drop": ["ALL"]}}}]}}


def service_boundary(services, namespace, labels):
    if not isinstance(services, list) or len(services) > 256:
        raise ValueError("service inventory exceeds bound")
    for service in services:
        if service.get("metadata", {}).get("namespace") != namespace:
            continue
        spec = service.get("spec", {})
        selector = spec.get("selector", {})
        if selector and all(labels.get(k) == v for k, v in selector.items()) and spec.get("publishNotReadyAddresses", False):
            raise ValueError("matching Service publishes unready addresses")


def equivalent_source_selectors(policies, actual_labels, probe_labels):
    def matches(selector, labels):
        if not isinstance(selector, dict) or set(selector) - {"matchLabels"}:
            raise ValueError("source selector shape is not qualified")
        return all(labels.get(k) == v for k, v in selector.get("matchLabels", {}).items())
    for policy in policies:
        selectors = [policy["spec"]["podSelector"]]
        for rule in policy["spec"].get("ingress", []):
            selectors.extend(peer["podSelector"] for peer in rule.get("from", []) if "podSelector" in peer)
        for selector in selectors:
            if matches(selector, actual_labels) != matches(selector, probe_labels):
                raise ValueError("minimal probe labels do not represent actual policy selection")


def controller_boundary(controllers, namespace, labels):
    if not isinstance(controllers, list) or len(controllers) > 512:
        raise ValueError("controller inventory exceeds bound")
    for controller in controllers:
        if controller.get("metadata", {}).get("namespace") != namespace:
            continue
        kind = controller.get("kind")
        if kind not in ("ReplicaSet", "ReplicationController", "StatefulSet", "DaemonSet", "Job"):
            raise ValueError("unexpected Pod-owning controller")
        selector = controller.get("spec", {}).get("selector")
        if not isinstance(selector, dict):
            raise ValueError("controller selector unavailable")
        if kind == "ReplicationController":
            selector = {"matchLabels": selector}
        if set(selector) - {"matchLabels", "matchExpressions"}:
            raise ValueError("unknown controller selector")
        match = all(labels.get(k) == v for k, v in selector.get("matchLabels", {}).items())
        for expression in selector.get("matchExpressions", []):
            key, op, values = expression.get("key"), expression.get("operator"), expression.get("values", [])
            if not isinstance(key, str) or not isinstance(values, list):
                raise ValueError("invalid controller selector")
            if op == "In": match &= key in labels and labels[key] in values
            elif op == "NotIn": match &= key not in labels or labels[key] not in values
            elif op == "Exists" and not values: match &= key in labels
            elif op == "DoesNotExist" and not values: match &= key not in labels
            else: raise ValueError("unsupported controller selector")
        if match:
            raise ValueError("existing controller could adopt the probe")


def running_unready(actual, desired, verified_image_id):
    if not isinstance(verified_image_id, str) or not 1 <= len(verified_image_id) <= 256:
        raise ValueError("independently verified runtime image identity required")
    meta, spec, status = actual.get("metadata", {}), actual.get("spec", {}), actual.get("status", {})
    expected_meta = desired["metadata"]
    if (meta.get("name") != expected_meta["name"] or meta.get("namespace") != expected_meta["namespace"]
            or meta.get("labels") != expected_meta["labels"] or meta.get("ownerReferences")
            or meta.get("deletionTimestamp")
            or not isinstance(meta.get("uid"), str) or not re.fullmatch(r"[0-9a-f-]{36}", meta["uid"])
            or spec.get("readinessGates") != [{"conditionType": GATE}]
            or spec.get("automountServiceAccountToken") is not False
            or any(spec.get(k) for k in ("hostNetwork", "hostPID", "hostIPC", "volumes", "initContainers", "ephemeralContainers"))
            or len(spec.get("containers", [])) != 1 or status.get("phase") != "Running"):
        raise ValueError("probe pod boundary changed")
    if any(spec.get(key) != desired["spec"].get(key) for key in ("securityContext", "activeDeadlineSeconds", "restartPolicy")):
        raise ValueError("probe pod security/lifetime changed")
    container = spec["containers"][0]
    expected = desired["spec"]["containers"][0]
    if any(container.get(k) != expected.get(k) for k in ("name", "image", "command", "securityContext", "resources")):
        raise ValueError("probe container differs")
    if any(container.get(k) for k in ("env", "envFrom", "volumeMounts", "ports", "args")):
        raise ValueError("probe gained an input or listener")
    conditions = status.get("conditions", [])
    if len([c for c in conditions if c.get("type") == "Ready" and c.get("status") == "False"]) != 1:
        raise ValueError("probe is not explicitly unready")
    if any(c.get("type") == GATE and c.get("status") != "False" for c in conditions):
        raise ValueError("probe readiness gate unexpectedly satisfied")
    states = status.get("containerStatuses", [])
    if len(states) != 1 or states[0].get("name") != "probe" or "running" not in states[0].get("state", {}):
        raise ValueError("probe container is not running")
    address = str(ipaddress.ip_address(status["podIP"]))
    image_id = states[0].get("imageID", "")
    if image_id != verified_image_id:
        raise ValueError("probe image digest differs")
    return address


def pod_addresses(actual):
    status = actual.get("status")
    if not isinstance(status, dict):
        return None
    primary = status.get("podIP", "")
    entries = status.get("podIPs")
    if entries is None:
        entries = []
    if not isinstance(primary, str) or not isinstance(entries, list) or len(entries) > 2:
        raise ValueError("invalid Pod address inventory")
    values = []
    for item in entries:
        if not isinstance(item, dict) or set(item) != {"ip"}:
            raise ValueError("invalid Pod address inventory")
        values.append(item["ip"])
    if primary:
        values.append(primary)
    elif values:
        raise ValueError("invalid Pod address inventory")
    if any(not isinstance(v, str) or not 1 <= len(v) <= 45 or "%" in v for v in values):
        raise ValueError("invalid Pod address inventory")
    addresses = sorted({str(ipaddress.ip_address(v)) for v in values})
    if entries and primary not in [item["ip"] for item in entries]:
        raise ValueError("invalid Pod address inventory")
    if not addresses and status.get("phase") != "Pending":
        return None
    return addresses








def verify_cri(image_id, inspection, identity):
    """Bind inspected CRI config/repo digests; never infer type from image_id."""
    if not isinstance(image_id, str) or not re.fullmatch(
            r"(?:(?:docker-pullable://)?(?:docker.io/library/)?python@)?sha256:[0-9a-f]{64}", image_id):
        raise ValueError("unexpected pod image identity format")
    if identity.get("root") != IMAGE.split("@", 1)[1] or identity.get("platform") != "linux/amd64":
        raise ValueError("unqualified image identity")
    for key in ("manifest", "config"):
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", identity.get(key, "")):
            raise ValueError("unqualified image digest")
    status = inspection.get("status", {})
    digests = status.get("repoDigests")
    allowed = {"docker.io/library/python@" + identity[key] for key in ("root", "manifest")}
    if status.get("id") != identity["config"] or not isinstance(digests, list) or not allowed.intersection(digests):
        raise ValueError("running image config/repository differs")
    return image_id

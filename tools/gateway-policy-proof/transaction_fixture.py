"""Real Kubernetes transaction oracle in a fresh owned kind cluster only."""
import copy
import base64
import lzma
import transport_codec as codec
import json
import os
from pathlib import Path
import re
import tempfile
import time
import uuid

import proof
import transaction as tx
from policy_client import KubernetesClient
from image_identity import public_identity
from rollout_engine import Probes
import rollout_probe
import restore_fixture
import network_evidence
from bounded_process import run as bounded_run

CASES = ["bounded_xz_transport"] + ["response_loss_" + str(at) for at in range(1, 9)] + [
    "dry_run_no_write", "four_policy_success", "annotation_transport_preserved", "concurrent_change_refuses_rollback", "empty_endpoint_slice_null",
    "ipv6_endpoint_metadata", "deployment_uid_lineage", "controller_adoption_refusal", "never_ready_endpoint_nonparticipation", "publish_unready_refusal", "actual_tcp_policy_controls", "probe_uid_cleanup"] + list(restore_fixture.CASES)

LAST_APPLIED = "kubectl.kubernetes.io/last-applied-configuration"
FIXTURE_ANNOTATIONS = {LAST_APPLIED: '{"fixture":"previous-configuration"}', "fixture.example/custom": "preserved"}


def annotation_transport_control(k, client):
    """Real old-client mutation, then exact raw preservation and CAS controls."""
    key = (tx.NAMESPACE, tx.NAMES[-1])
    obj = tx.inventory(client.list_policies())[key]
    obj["metadata"].setdefault("annotations", {}).update(FIXTURE_ANNOTATIONS)
    obj["metadata"].setdefault("labels", {})["fixture.example/custom"] = "preserved"
    client.replace(obj)
    baseline = tx.inventory(client.list_policies())
    original = baseline[key]
    if any(original["metadata"]["annotations"].get(k) != v for k, v in FIXTURE_ANNOTATIONS.items()):
        raise AssertionError("raw baseline metadata not preserved")
    client.dry_run(original, True)
    if not tx.same_inventory(baseline, tx.inventory(client.list_policies()), exact_version=True):
        raise AssertionError("raw dry-run mutated API")
    # Deliberately invoke the old adapter against this disposable object only.
    k("replace", "-f", "-", value=original)
    changed = tx.inventory(client.list_policies())[key]
    if changed["metadata"]["annotations"].get(LAST_APPLIED) == original["metadata"]["annotations"][LAST_APPLIED]:
        raise AssertionError("old kubectl mutation control did not discriminate")
    expected_annotation = copy.deepcopy(original)
    del expected_annotation["metadata"]["annotations"][LAST_APPLIED]
    if json.loads(changed["metadata"]["annotations"][LAST_APPLIED]) != expected_annotation:
        raise AssertionError("old kubectl mutation is not the expected transformation")
    restored = copy.deepcopy(original)
    restored["metadata"]["resourceVersion"] = changed["metadata"]["resourceVersion"]
    client.replace(restored)
    baseline = tx.inventory(client.list_policies())
    if tx.semantic(baseline[key]) != tx.semantic(original):
        raise AssertionError("raw metadata restoration differs")
    for field, value in (("resourceVersion", original["metadata"]["resourceVersion"]),
                         ("uid", "00000000-0000-0000-0000-000000000000")):
        stale = copy.deepcopy(baseline[key])
        stale["metadata"][field] = value
        try:
            client.replace(stale)
        except RuntimeError:
            pass
        else:
            raise AssertionError("raw CAS accepted stale identity")
        if not tx.same_inventory(baseline, tx.inventory(client.list_policies()), exact_version=True):
            raise AssertionError("refused CAS changed inventory")
    # A valid request after the negatives prevents transport failure being credited.
    client.dry_run(baseline[key], True)
    return {"oldClientMutation": True, "rawMetadataPreserved": True, "dryRunUnchanged": True,
            "staleRVRefused": True, "wrongUIDRefused": True, "rollbackPreserved": False, "finalizePreserved": False}


def validate_annotation_control(control):
    keys = {"oldClientMutation", "rawMetadataPreserved", "dryRunUnchanged", "staleRVRefused",
            "wrongUIDRefused", "rollbackPreserved", "finalizePreserved"}
    if not isinstance(control, dict) or set(control) != keys or any(control[k] is not True for k in keys):
        raise ValueError("annotation preservation controls incomplete")


IPV6_CANONICAL = "2001:db8::1"
IPV6_EXPANDED = "2001:0db8:0000:0000:0000:0000:0000:0001"


def validate_ipv6_slice(control):
    if (not isinstance(control, dict)
            or set(control) != {"sliceUID", "serviceName", "serviceAbsent", "shape", "observedAddress", "literalComparisonMiss", "absenceRefused"}
            or not isinstance(control.get("sliceUID"), str) or not re.fullmatch(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", control["sliceUID"])
            or not isinstance(control.get("serviceName"), str) or not re.fullmatch(r"ipv6-control-[0-9a-f]{32}", control["serviceName"])
            or control["serviceAbsent"] is not True or control["absenceRefused"] is not True
            or control["literalComparisonMiss"] is not True
            or control["observedAddress"] != IPV6_EXPANDED
            or control["shape"] != {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "addressType": "IPv6",
                                    "endpoints": [{"addresses": [IPV6_CANONICAL], "conditions": {"ready": False}}]}):
        raise ValueError("actual IPv6 metadata control missing")


def ipv6_slice_control(k):
    """API metadata only: no Service, targetRef, Pod or IPv6 data-plane traffic."""
    name = "ipv6-control-" + uuid.uuid4().hex
    def absent():
        if k("-n", tx.NAMESPACE, "get", "service", name, "--ignore-not-found", "-o", "name").strip():
            raise AssertionError("IPv6 control Service exists")
    absent()
    desired = {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "addressType": "IPv6",
               "metadata": {"name": name, "namespace": tx.NAMESPACE, "labels": {"kubernetes.io/service-name": name}},
               "endpoints": [{"addresses": [IPV6_CANONICAL], "conditions": {"ready": False}}], "ports": []}
    k("create", "-f", "-", value=desired)
    actual = json.loads(k("-n", tx.NAMESPACE, "get", "endpointslice", name, "-o", "json"))
    metadata = actual.get("metadata", {})
    if (metadata.get("name") != name or metadata.get("namespace") != tx.NAMESPACE
            or metadata.get("labels", {}).get("kubernetes.io/service-name") != name
            or metadata.get("ownerReferences")):
        raise AssertionError("IPv6 metadata custody differs")
    absent()
    control = {"sliceUID": metadata.get("uid"), "serviceName": name, "serviceAbsent": True,
               "shape": {key: actual.get(key) for key in ("apiVersion", "kind", "addressType", "endpoints")},
               "observedAddress": IPV6_EXPANDED, "literalComparisonMiss": False, "absenceRefused": False}
    # Establish the exact shape before a refusal could be credited to malformed data.
    validate_ipv6_slice({**control, "absenceRefused": True, "literalComparisonMiss": True})
    # The old literal intersection misses this actual canonical API readback.
    if set(actual["endpoints"][0]["addresses"]) & {IPV6_EXPANDED}:
        raise AssertionError("IPv6 metadata control did not discriminate literal comparison")
    control["literalComparisonMiss"] = True
    rollout_probe.endpoint_boundary([actual], [IPV6_EXPANDED])
    try:
        rollout_probe.endpoint_boundary([actual], [IPV6_EXPANDED], require_absent=True)
    except ValueError:
        control["absenceRefused"] = True
    validate_ipv6_slice(control)
    return control


def qualify_codec():
    """Same checked codec on actual Linux; no alternate decoder in the oracle."""
    files = {name: (Path(__file__).parent / name).read_text(encoding="utf-8") for name in
             ("policy_state.py", "transaction.py", "policy_client.py", "rollout_probe.py", "rollout_scope.py", "rollout_engine.py", "bounded_process.py", "source_loader.py")}
    raw = json.dumps({"files": files, "payload": {"synthetic": True}}, ensure_ascii=False).encode()
    encoded = codec.encode(raw)
    if codec.decode(encoded) != raw:
        raise ValueError("XZ source roundtrip differs")
    packed = base64.b64decode(encoded)
    controls = [packed[:-1], packed + b"x", packed + packed, b"invalid",
                lzma.compress(b"x" * 524289),
                lzma.compress(b"control", check=lzma.CHECK_NONE),
                lzma.compress(b"control", filters=[{"id": lzma.FILTER_LZMA2, "dict_size": 67108864}])]
    for value in controls:
        try:
            codec.decode(base64.b64encode(value).decode("ascii"))
        except (ValueError, lzma.LZMAError):
            continue
        raise ValueError("invalid XZ control accepted")
    return True



def runtime_failure_category(message):
    """Classify bounded runtime text without retaining paths or raw diagnostics."""
    if not isinstance(message, str) or len(message) > 4096:
        return "unknown"
    text = message.lower()
    for category, markers in (
        ("disk_exhausted", ("no space left on device", "disk quota exceeded")),
        ("missing_executable", ("executable file not found", "exec format error")),
        ("missing_image_content", ("content digest", "failed to get reader", "failed to extract layer")),
        ("mount_setup", ("failed to mount", "error mounting", "not a shared mount")),
        ("security_setup", ("seccomp", "apparmor", "selinux")),
        ("read_only", ("read-only file system",)),
        ("permission", ("permission denied", "operation not permitted")),
        ("missing_path", ("no such file or directory",)),
    ):
        if any(marker in text for marker in markers):
            return category
    return "unknown"


def synthetic_runtime_detail(message):
    """Only the credential-free synthetic container's creation error template.

    Never used by the live operator engine. Variable paths, quoted values,
    addresses and opaque identifiers are removed before bounded projection.
    """
    if not isinstance(message, str) or len(message) > 4096:
        return "unavailable"
    text = re.sub(r"https?://\S+|\b\w+://\S+", "<address>", message)
    text = re.sub(r'''"[^"\n]*"|'[^'\n]*'|`[^`\n]*`''', "<value>", text)
    text = re.sub(r"(?:[A-Za-z]:)?/[^\s,;:]*", "<path>", text)
    text = re.sub(r"\b(?:sha256:)?[0-9a-fA-F]{12,}\b|\b[0-9a-fA-F]{8}(?:-[0-9a-fA-F-]+)\b", "<id>", text)
    text = re.sub(r"\b[\w.-]+=[^\s,;]+", "<assignment>", text)
    text = re.sub(r"\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b", "<address>", text)
    text = re.sub(r"[\x00-\x1f\x7f-\uffff]", " ", text)
    return text[:512]


def deployment_diagnostic(deployment, pods):
    """Closed status projection only; never messages, events, specs or logs."""
    if not isinstance(pods, list) or len(pods) > 16:
        raise ValueError("diagnostic pod bound")
    def number(value):
        return value if type(value) is int and 0 <= value <= 100000 else None
    def enum(value, allowed):
        return value if isinstance(value, str) and value in allowed else "other"
    status = deployment.get("status", {})
    result = {"replicas": {key: number(status.get(key)) for key in
              ("replicas", "updatedReplicas", "readyReplicas", "availableReplicas", "observedGeneration")}, "pods": []}
    conditions = status.get("conditions", [])
    if not isinstance(conditions, list) or len(conditions) > 16:
        raise ValueError("diagnostic condition bound")
    result["conditions"] = [{"type": enum(c.get("type"), {"Available", "Progressing", "ReplicaFailure"}),
                             "status": enum(c.get("status"), {"True", "False", "Unknown"}),
                             "reason": enum(c.get("reason"), {"MinimumReplicasUnavailable", "MinimumReplicasAvailable", "ReplicaSetUpdated", "NewReplicaSetAvailable", "ProgressDeadlineExceeded", "FailedCreate"})} for c in conditions]
    for pod in pods:
        status = pod.get("status", {})
        states = status.get("containerStatuses", [])
        conditions = status.get("conditions", [])
        if not isinstance(states, list) or len(states) > 4 or not isinstance(conditions, list) or len(conditions) > 16:
            raise ValueError("diagnostic container bound")
        result["pods"].append({"phase": enum(status.get("phase"), {"Pending", "Running", "Succeeded", "Failed", "Unknown"}),
            "conditions": [{"type": enum(c.get("type"), {"PodScheduled", "Initialized", "ContainersReady", "Ready"}),
                            "status": enum(c.get("status"), {"True", "False", "Unknown"}),
                            "reason": enum(c.get("reason"), {"Unschedulable", "ContainersNotReady", "PodInitializing", "SchedulingGated", "SchedulerError"})} for c in conditions],
            "containers": [{"ready": c.get("ready") is True, "restarts": number(c.get("restartCount")),
                "running": isinstance(c.get("state", {}).get("running"), dict),
                "waiting": enum(c.get("state", {}).get("waiting", {}).get("reason"),
                    {"ContainerCreating", "ImagePullBackOff", "ErrImagePull", "CrashLoopBackOff", "CreateContainerConfigError", "CreateContainerError", "RunContainerError"}),
                "runtimeFailure": runtime_failure_category(c.get("state", {}).get("waiting", {}).get("message")),
                "syntheticRuntimeDetail": synthetic_runtime_detail(c.get("state", {}).get("waiting", {}).get("message")),
                "exitCode": number(c.get("state", {}).get("terminated", {}).get("exitCode"))} for c in states]})
    return result


def validate_empty_slice(control):
    if (not isinstance(control, dict) or set(control) != {"serviceUID", "sliceUID", "shape"}
            or any(not isinstance(control.get(k), str) or not re.fullmatch(r"[0-9a-f-]{36}", control[k]) for k in ("serviceUID", "sliceUID"))
            or control["serviceUID"] == control["sliceUID"]
            or control["shape"] != {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "addressType": "IPv4", "endpoints": None}):
        raise ValueError("actual null EndpointSlice evidence missing")


def empty_slice_control(k):
    name = "empty-endpoint-control"
    selector = "empty-endpoint-control=" + uuid.uuid4().hex
    if json.loads(k("-n", tx.NAMESPACE, "get", "pods", "-l", selector, "-o", "json"))["items"]:
        raise AssertionError("empty Service selector has Pods")
    key, value = selector.split("=")
    service = {"apiVersion": "v1", "kind": "Service", "metadata": {"name": name, "namespace": tx.NAMESPACE},
               "spec": {"selector": {key: value}, "ports": [{"port": 8080, "targetPort": 8080}]}}
    k("create", "-f", "-", value=service)
    actual = json.loads(k("-n", tx.NAMESPACE, "get", "service", name, "-o", "json"))
    if actual["spec"]["selector"] != service["spec"]["selector"]:
        raise AssertionError("empty Service selector changed")
    end = time.monotonic() + 30
    while True:
        items = json.loads(k("-n", tx.NAMESPACE, "get", "endpointslices", "-l", "kubernetes.io/service-name=" + name, "-o", "json"))["items"]
        if not isinstance(items, list) or len(items) > 1:
            raise AssertionError("empty Service slice inventory differs")
        if items:
            item = items[0]
            owners = item.get("metadata", {}).get("ownerReferences", [])
            if (item["metadata"].get("namespace") != tx.NAMESPACE or len(owners) != 1
                    or owners[0].get("kind") != "Service" or owners[0].get("name") != name
                    or owners[0].get("uid") != actual["metadata"]["uid"] or owners[0].get("controller") is not True):
                raise AssertionError("empty slice controller ownership differs")
            control = {"serviceUID": actual["metadata"]["uid"], "sliceUID": item["metadata"]["uid"],
                       "shape": {key: item[key] for key in ("apiVersion", "kind", "addressType", "endpoints")}}
            validate_empty_slice(control)
            rollout_probe.endpoint_boundary(items, [], require_absent=True)
            return control
        if time.monotonic() >= end:
            raise AssertionError("controller null EndpointSlice not observed")
        time.sleep(1)


def validate_network_receipt(value):
    if not isinstance(value, dict) or set(value) != {"record", "sha256", "durableSequence", "receiptCount"}:
        raise ValueError("network receipt evidence missing")
    network_evidence.validate(value["record"], value["record"].get("nonce"))
    if (value["record"]["isolated"] is not True or value["record"]["phase"] != "network_verified"
            or value["sha256"] != tx.fingerprint(value["record"])
            or type(value["durableSequence"]) is not int or type(value["receiptCount"]) is not int
            or not 1 <= value["durableSequence"] < value["receiptCount"] <= 64):
        raise ValueError("network durable receipt evidence differs")


def validate_report(report, source, tree, run_id, attempt, render_hash):
    if (report.get("format") != "kanz-gateway-policy-transaction-proof-v1"
            or report.get("source") != source or report.get("tree") != tree
            or report.get("runID") != run_id or report.get("runAttempt") != attempt
            or report.get("renderSHA256") != render_hash
            or report.get("nodeImage") != proof.feed.NODE
            or not isinstance(report.get("cluster"), str)
            or not re.fullmatch(r"cni-proof-[0-9a-f]{12}", report["cluster"])
            or report.get("cases") != CASES or report.get("passed") is not True
            or report.get("cleanupPassed") is not True):
        raise ValueError("transaction evidence incomplete or from another execution")
    validate_network_receipt(report.get("networkReceipt"))
    restore_fixture.validate(report.get("retainedRestore"))
    validate_annotation_control(report.get("annotationTransport"))
    validate_empty_slice(report.get("emptyEndpointSlice"))
    validate_ipv6_slice(report.get("ipv6EndpointSlice"))
    identity = report.get("imageIdentity", {})
    if set(identity) != {"root", "rootMediaType", "manifest", "config", "platform"} or identity.get("root") != rollout_probe.IMAGE.split("@", 1)[1] or identity.get("platform") != "linux/amd64":
        raise ValueError("transaction image identity missing")


class LostResponse:
    """Fixture-only fault: real API mutation completes before observer failure."""
    def __init__(self, client, at):
        self.client, self.at, self.count = client, at, 0
        self.fired = False

    def __getattr__(self, name):
        return getattr(self.client, name)

    def write(self, name, obj):
        getattr(self.client, name)(obj)
        self.count += 1
        if self.count == self.at:
            self.fired = True
            raise RuntimeError("fixture response lost after real mutation")

    def create(self, obj):
        self.write("create", obj)

    def replace(self, obj):
        self.write("replace", obj)


def run_fixture(artifact):
    report = {"format": "kanz-gateway-policy-transaction-proof-v1", "passed": False,
              "cleanupPassed": False, "cases": []}
    cluster = "cni-proof-" + uuid.uuid4().hex[:12]
    report.update(cluster=cluster, nodeImage=proof.feed.NODE)
    owned = False
    with tempfile.TemporaryDirectory(prefix="kanz-policy-transaction-") as tmp:
        scratch = Path(tmp)
        env = {key: os.environ[key] for key in ("PATH", "SYSTEMROOT") if key in os.environ}
        env.update(HOME=str(scratch), TMPDIR=str(scratch), LANG="C.UTF-8")

        def run(argv, data=None, timeout=180):
            return bounded_run(argv, cwd=proof.ROOT, env=env, data=data, timeout=timeout)

        def k(*args, value=None, timeout=180):
            return run(["kubectl", "--kubeconfig", str(scratch / "kubeconfig"), "--context", "kind-" + cluster, *args],
                       None if value is None else json.dumps(value), timeout=timeout)

        try:
            report["source"] = run(["git", "rev-parse", "HEAD"]).strip()
            report["tree"] = run(["git", "rev-parse", "HEAD^{tree}"]).strip()
            report["runID"] = os.environ["GITHUB_RUN_ID"]
            report["runAttempt"] = os.environ["GITHUB_RUN_ATTEMPT"]
            qualify_codec()
            report["cases"].append("bounded_xz_transport")
            rendered, _, report["renderSHA256"] = proof.render(run)
            desired = [p for p in rendered if p["metadata"]["name"] in tx.NAMES]
            desired = copy.deepcopy(desired)
            for obj in desired:
                obj["metadata"].setdefault("annotations", {}).update(FIXTURE_ANNOTATIONS)
                obj["metadata"].setdefault("labels", {})["fixture.example/custom"] = "preserved"
            baseline = [copy.deepcopy(p) for p in rendered if p["metadata"]["name"] not in tx.NAMES[:3]]
            egress = next(p for p in baseline if p["metadata"]["name"] == tx.NAMES[-1])
            for app, port in (("accounting", 8101), ("audit", 8102)):
                egress["spec"]["egress"].remove({"to": [{"podSelector": {"matchLabels": {"app": app}}}],
                                                "ports": [{"protocol": "TCP", "port": port}]})
            proof.claim_cluster(cluster, scratch, artifact, run)
            owned = True
            proof.feed.command = run
            proof.feed.create_enforcing_cluster(cluster, scratch)
            k("create", "namespace", tx.NAMESPACE)
            for obj in baseline:
                k("create", "-f", "-", value=obj)
            cluster_uid = json.loads(k("get", "namespace", "kube-system", "-o", "json"))["metadata"]["uid"]
            client = KubernetesClient(k, cluster_uid)
            report["annotationTransport"] = annotation_transport_control(k, client)
            original = tx.inventory(client.list_policies())

            def qualifier(actual, target):
                tx.qualify_rendered_baseline(actual, target, rendered)

            for at in range(1, 9):
                receipts = []
                fault = LostResponse(client, at)
                transaction = tx.Transaction(fault, desired, uuid.uuid4().hex, receipts.append)
                transaction.prepare(qualifier)
                try:
                    transaction.apply(lambda: None)
                except RuntimeError:
                    pass
                else:
                    raise AssertionError("response-loss mutation did not discriminate")
                if not fault.fired or receipts[-1]["rollback"] != "verified" or not tx.same_inventory(original, tx.inventory(client.list_policies())):
                    raise AssertionError("real rollback differs from baseline")
                report["cases"].append("response_loss_" + str(at))
            report["annotationTransport"]["rollbackPreserved"] = True
            receipts = []
            transaction = tx.Transaction(client, desired, uuid.uuid4().hex, receipts.append)
            dry_baseline = tx.inventory(client.list_policies())
            transaction.prepare(qualifier)
            if not tx.same_inventory(dry_baseline, tx.inventory(client.list_policies()), exact_version=True):
                raise AssertionError("dry-run mutated policy inventory")
            report["cases"].append("dry_run_no_write")
            transaction.apply(lambda: proof.same_policy_union(client.list_policies(), rendered))
            proof.same_policy_union(client.list_policies(), rendered)
            if receipts[-1]["phase"] != "verified":
                raise AssertionError("transaction did not complete")
            report["cases"].append("four_policy_success")
            final = tx.inventory(client.list_policies())[(tx.NAMESPACE, tx.NAMES[-1])]
            if tx.semantic(final)["metadata"] != tx.semantic(original[(tx.NAMESPACE, tx.NAMES[-1])])["metadata"]:
                raise AssertionError("finalization changed user metadata")
            installed = tx.inventory(client.list_policies())
            for obj in desired:
                key = (tx.NAMESPACE, obj["metadata"]["name"])
                expected_metadata = tx.semantic(original.get(key, obj))["metadata"]
                if tx.semantic(installed[key])["metadata"] != expected_metadata:
                    raise AssertionError("create/finalization changed user metadata")
            report["annotationTransport"]["finalizePreserved"] = True
            validate_annotation_control(report["annotationTransport"])
            report["cases"].append("annotation_transport_preserved")
            # A real API edit with an external field must not be overwritten by
            # automatic rollback. The whole fresh cluster remains disposable.
            receipts = []
            transaction = tx.Transaction(client, desired, uuid.uuid4().hex, receipts.append)
            transaction.prepare(qualifier)
            foreign = None
            def concurrent_change():
                nonlocal foreign
                obj = next(p for p in client.list_policies() if p["metadata"]["name"] == tx.NAMES[0])
                obj["metadata"].setdefault("annotations", {})["fixture.example/concurrent"] = "retained"
                client.replace(obj)
                foreign = tx.inventory(client.list_policies())
            try:
                transaction.apply(concurrent_change)
            except tx.Refusal:
                pass
            else:
                raise AssertionError("concurrent edit was silently accepted")
            if receipts[-1]["rollback"] != "unresolved" or not tx.same_inventory(foreign, tx.inventory(client.list_policies()), exact_version=True):
                raise AssertionError("rollback modified another owner's policy")
            report["cases"].append("concurrent_change_refuses_rollback")
            report["emptyEndpointSlice"] = empty_slice_control(k)
            report["cases"].append("empty_endpoint_slice_null")
            report["ipv6EndpointSlice"] = ipv6_slice_control(k)
            report["cases"].append("ipv6_endpoint_metadata")
            # Exercise the exact live-probe constructor on the disposable API.
            # No live target or domain workload is contacted by this fixture.
            identity = public_identity()
            # Use the node's CRI pull path, as the preceding policy proof does.
            # Host archive import can leave a CRI image record whose containerd
            # checkpoint lookup fails. The immutable image and subsequent typed
            # registry/config/CRI identity checks remain mandatory.
            k("create", "namespace", "kanz-observability")
            k("create", "-f", "-", value={"apiVersion": "v1", "kind": "ConfigMap",
              "metadata": {"name": "scripts", "namespace": tx.NAMESPACE},
              "data": {"listener.py": (proof.HERE / "listener.py").read_text()}})
            endpoints = {}
            governed = {}
            for name, ports in {**proof.TARGETS, "api-gateway": (8080,), "web-bff": (8080,)}.items():
                template = proof.pod("target-" + name, tx.NAMESPACE, {"app": name}, ports)
                template["spec"]["restartPolicy"] = "Always"
                template["spec"].pop("activeDeadlineSeconds", None)
                deployment = {"apiVersion": "apps/v1", "kind": "Deployment",
                    "metadata": {"name": name, "namespace": tx.NAMESPACE},
                    "spec": {"replicas": 1, "selector": {"matchLabels": {"app": name}},
                             "template": {"metadata": {"labels": {"app": name}}, "spec": template["spec"]}}}
                k("create", "-f", "-", value=deployment)
                try:
                    k("-n", tx.NAMESPACE, "rollout", "status", "deployment/" + name, "--timeout=90s")
                except Exception:
                    report["failedTarget"] = name
                    try:
                        d = json.loads(k("-n", tx.NAMESPACE, "get", "deployment", name, "-o", "json"))
                        p = json.loads(k("-n", tx.NAMESPACE, "get", "pods", "-l", "app=" + name, "-o", "json"))["items"]
                        report["deploymentDiagnostic"] = deployment_diagnostic(d, p)
                    except Exception:
                        report["deploymentDiagnostic"] = {"unavailable": True}
                    raise
                pods = json.loads(k("-n", tx.NAMESPACE, "get", "pods", "-l", "app=" + name, "-o", "json"))["items"]
                if len(pods) != 1:
                    raise AssertionError("fixture deployment is not a singleton")
                actual = pods[0]
                expected = {name: {"labels": {"app": name}, "selector": deployment["spec"]["selector"],
                            "images": {key: {c["name"]: c["image"] for c in template["spec"].get(key, [])}
                                       for key in ("containers", "initContainers")}}}
                governed.update(expected)
                deployments = json.loads(k("-n", tx.NAMESPACE, "get", "deployments", "-o", "json"))["items"]
                replicasets = json.loads(k("-n", tx.NAMESPACE, "get", "replicasets", "-o", "json"))["items"]
                if rollout_probe.workload_lineage(actual, deployments, replicasets, expected)["name"] != name:
                    raise AssertionError("actual deployment lineage not bound")
                endpoints[(tx.NAMESPACE, name)] = {"ip": actual["status"]["podIP"]}
            report["cases"].append("deployment_uid_lineage")
            service = {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "probe-endpoint-control", "namespace": tx.NAMESPACE},
                       "spec": {"selector": {"app": "api-gateway"}, "ports": [{"name": "api", "port": 8080, "targetPort": 8080}]}}
            k("create", "-f", "-", value=service)
            # A real zero-replica controller exists before any matching probe.
            # Its selector would adopt a bare probe; preflight must refuse it.
            control_nonce = uuid.uuid4().hex
            template = rollout_probe.caller_pod(tx.NAMESPACE, "policy-untrusted", control_nonce)
            template["spec"]["restartPolicy"] = "Always"
            template["spec"].pop("activeDeadlineSeconds")
            controller = {"apiVersion": "apps/v1", "kind": "ReplicaSet",
                          "metadata": {"name": "probe-adoption-control", "namespace": tx.NAMESPACE},
                          "spec": {"replicas": 0, "selector": {"matchLabels": {"app": "policy-untrusted"}},
                                   "template": {"metadata": {"labels": template["metadata"]["labels"]}, "spec": template["spec"]}}}
            if json.loads(k("-n", tx.NAMESPACE, "get", "pods", "-l", "app=policy-untrusted", "-o", "json"))["items"]:
                raise AssertionError("controller control has matching preexisting Pods")
            k("create", "-f", "-", value=controller)
            actual_controller = json.loads(k("-n", tx.NAMESPACE, "get", "replicaset", "probe-adoption-control", "-o", "json"))
            try:
                rollout_probe.controller_boundary([actual_controller], tx.NAMESPACE, template["metadata"]["labels"])
            except ValueError as error:
                if str(error) != "existing controller could adopt the probe":
                    raise
            else:
                raise AssertionError("actual adopting controller was accepted")
            metadata = actual_controller["metadata"]
            k("delete", "--raw", "/apis/apps/v1/namespaces/" + tx.NAMESPACE + "/replicasets/probe-adoption-control", "-f", "-",
              value={"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {"uid": metadata["uid"], "resourceVersion": metadata["resourceVersion"]}})
            k("-n", tx.NAMESPACE, "wait", "--for=delete", "replicaset/probe-adoption-control", "--timeout=30s")
            if json.loads(k("-n", tx.NAMESPACE, "get", "pods", "-l", "app=policy-untrusted", "-o", "json"))["items"]:
                raise AssertionError("zero-replica controller created a Pod")
            report["cases"].append("controller_adoption_refusal")
            probe_history = []
            receipt_directory = scratch / "network-receipts"
            receipt_directory.mkdir(mode=0o700)
            durable = tx.Receipts(receipt_directory)
            def save_probe(value):
                durable(value)  # Actual file and directory fsync precede observation.
                if value["format"] == "kanz-gateway-network-v1":
                    raw = (receipt_directory / ("receipt-%04d.json" % durable.count)).read_bytes()
                    if raw != tx.encoded(value) + b"\n":
                        raise AssertionError("durable network aggregate differs")
                    report["networkReceipt"] = {"record": json.loads(raw), "sha256": tx.fingerprint(value),
                                                "durableSequence": durable.count, "receiptCount": durable.count}
                else:
                    # The legacy fixture constructs its original 39-record history
                    # solely from probe custody records, never new network records.
                    probe_history.append(copy.deepcopy(value))
            probes = Probes(k, lambda image_id: json.loads(run(["docker", "exec", cluster + "-control-plane", "crictl", "inspecti", image_id])), identity, uuid.uuid4().hex, save_probe)
            try:
                probes.create()
                end = time.monotonic() + 30
                while True:
                    slices = probes.endpoints()
                    rollout_probe.endpoint_boundary(slices, probes.addresses)
                    matching = [e for item in slices for e in (item.get("endpoints") or [])
                                if set(e.get("addresses", [])) & set(probes.addresses)]
                    if matching:
                        rollout_probe.endpoint_boundary(slices, probes.addresses)
                        break
                    if time.monotonic() >= end:
                        raise AssertionError("EndpointSlice never observed the unready probe")
                    time.sleep(1)
                report["cases"].append("never_ready_endpoint_nonparticipation")
                k("-n", tx.NAMESPACE, "patch", "service", "probe-endpoint-control", "--type=merge", "-p", '{"spec":{"publishNotReadyAddresses":true}}')
                try:
                    probes.check()
                except ValueError as error:
                    if str(error) != "matching Service publishes unready addresses":
                        raise
                else:
                    raise AssertionError("real publishNotReady Service was accepted")
                report["cases"].append("publish_unready_refusal")
                k("-n", tx.NAMESPACE, "patch", "service", "probe-endpoint-control", "--type=merge", "-p", '{"spec":{"publishNotReadyAddresses":false}}')
                probes.verify(endpoints, True)
                report["cases"].append("actual_tcp_policy_controls")
            finally:
                try:
                    probes.close()
                    if "networkReceipt" in report:
                        report["networkReceipt"]["receiptCount"] = durable.count
                finally:
                    durable.close()
            validate_network_receipt(report["networkReceipt"])
            report["cases"].append("probe_uid_cleanup")
            # Additional synthetic workloads supply the same seven-workload
            # health snapshot, without running NATS/Redis or contacting domains.
            k("create", "namespace", "kanz-messaging")
            k("create", "-f", "-", value={"apiVersion": "v1", "kind": "ConfigMap",
                "metadata": {"name": "scripts", "namespace": "kanz-messaging"},
                "data": {"listener.py": (proof.HERE / "listener.py").read_text(encoding="utf-8")}})
            for name, port in (("nats", 4222), ("redis", 6379)):
                k("create", "-f", "-", value=proof.pod("synthetic-" + name, "kanz-messaging", {"app": name}, (port,)))
                k("-n", "kanz-messaging", "wait", "--for=condition=Ready", "pod/synthetic-" + name, "--timeout=90s")
            restore_progress = {}
            try:
                report["retainedRestore"] = restore_fixture.qualify(k, cluster_uid, report["source"], rendered,
                                                                     governed, probe_history, scratch, restore_progress)
            except Exception as error:
                report["retainedRestoreFailure"] = restore_fixture.failure_projection(error, restore_progress)
                raise
            restore_fixture.validate(report["retainedRestore"])
            report["cases"].extend(report["retainedRestore"]["cases"])
            k("delete", "namespace", "kanz-messaging", "--wait=true", "--timeout=60s")
            if k("get", "namespace", "kanz-messaging", "--ignore-not-found", "-o", "name").strip():
                raise AssertionError("synthetic messaging namespace remained")
            report["imageIdentity"] = identity
            report["passed"] = True
        finally:
            if owned:
                try:
                    run(["kind", "delete", "cluster", "--name", cluster])
                    if cluster in run(["kind", "get", "clusters"]).splitlines():
                        raise RuntimeError("cluster cleanup not observed")
                    report["cleanupPassed"] = True
                except Exception:
                    report["passed"] = False
                    raise
                finally:
                    try:
                        if report["passed"]:
                            validate_report(report, report["source"], report["tree"], os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"], report["renderSHA256"])
                    except Exception:
                        report["passed"] = False
                        raise
                    finally:
                        artifact.write_text(json.dumps(report, sort_keys=True) + "\n")
    if not report["cleanupPassed"]:
        raise RuntimeError("transaction cleanup missing")
    validate_report(report, report["source"], report["tree"], os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"], report["renderSHA256"])
    return report


if __name__ == "__main__":
    run_fixture(Path(os.environ["RUNNER_TEMP"]) / "gateway-policy-transaction.json")

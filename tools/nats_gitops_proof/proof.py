"""Source-bound Argo PostSync and JetStream PVC proof in a new disposable cluster."""
import base64
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import time
import uuid

import yaml

ROOT = Path(__file__).resolve().parents[2]
NAMESPACE = "kanz-messaging"
APP = "nats-gitops-verification"
ARGO_VERSION = "v3.6.1"
ARGO_SHA = "0b38189d6e1fd76ef5c3db10355026c48a02020d82bf21a1a1b5ab16dffe4f3c"
ARGO_IMAGES = {
    "quay.io/argoproj/argocd:v3.6.1": "quay.io/argoproj/argocd:v3.6.1@sha256:296e3d74f449b2ce45265613fcb68c18ed7366488360c8ea32f5f2a93d2e5c54",
    "public.ecr.aws/docker/library/redis:8.10.1-alpine": "docker.io/library/redis:8.10.1-alpine@sha256:ef216d85ec44f4fdf7c0e6df2d1757a4734ed32fbb278cabd3181b5a27353b79",
}
CASES = ["canonical-source", "first-postsync", "canonical-runtime", "stream-inventory", "canonical-fact",
         "empty-stream-removed", "same-revision-postsync", "stream-restored",
         "pod-replaced", "pvc-preserved", "fact-preserved", "cleanup"]
DELETE_STREAM = "OBSERVABILITY"


def diagnostic_category(text):
    """Return only a closed category; subprocess diagnostics are never retained."""
    value = text.lower()
    categories = (("no such host", "dns-unavailable"), ("manifest unknown", "registry-manifest-unavailable"),
                  ("unauthorized", "authentication-refused"), ("permission violation", "broker-grant-refused"),
                  ("unknown flag", "unsupported-cli-flag"), ("x509:", "certificate-refused"),
                  ("certificate", "certificate-refused"), ("failedscheduling", "pod-unschedulable"),
                  ("failedmount", "volume-mount-refused"), ("imagepullbackoff", "image-pull-unavailable"),
                  ("errimagepull", "image-pull-unavailable"), ("context deadline exceeded", "dependency-timeout"),
                  ("timed out", "dependency-timeout"), ("connection refused", "listener-unavailable"))
    return next((category for fragment, category in categories if fragment in value), "unclassified-refusal")


def load_module(name, relative):
    spec = importlib.util.spec_from_file_location(name, ROOT / relative)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def documents(relative):
    return [d for d in yaml.safe_load_all((ROOT / relative).read_text()) if d]


def expected_streams(script, replicas):
    """Derive the deployed script's complete ensure_stream calls, never a copied list."""
    result = {}
    for line in script.splitlines():
        if not re.match(r"^\s*ensure_stream\s+", line):
            continue
        args = shlex.split(line, comments=True)
        if not args or args[0] != "ensure_stream":
            continue
        if len(args) < 4 or not re.fullmatch(r"[A-Z_]+", args[1]) or args[1] in result:
            raise ValueError("invalid or duplicate canonical stream declaration")
        age = args[3]
        if age == "0":
            nanos = 0
        elif re.fullmatch(r"[1-9][0-9]*h", age):
            nanos = int(age[:-1]) * 3600 * 10**9
        else:
            raise ValueError("unresolved canonical stream age")
        config = {"name": args[1], "subjects": sorted(args[2].split(",")), "storage": "file",
                  "retention": "limits", "num_replicas": replicas, "max_age": nanos,
                  "discard": "old", "duplicate_window": 120 * 10**9}
        for flag in args[4:]:
            key, value = flag.split("=", 1)
            keys = {"--discard": "discard", "--max-msgs": "max_msgs", "--max-bytes": "max_bytes",
                    "--max-msgs-per-subject": "max_msgs_per_subject"}
            if key not in keys:
                raise ValueError("unresolved canonical stream option")
            config[keys[key]] = value if key == "--discard" else int(value)
        result[args[1]] = config
    if len(result) != 18 or DELETE_STREAM not in result or "MARKET" not in result:
        raise ValueError("canonical stream inventory changed; review full acceptance")
    return result


def check_inventory(expected, actual):
    if set(actual) != set(expected):
        raise ValueError("observed stream inventory differs from canonical source")
    for name, desired in expected.items():
        found = actual[name]["config"]
        for key, value in desired.items():
            observed = sorted(found[key]) if key == "subjects" else found[key]
            if observed != value:
                raise ValueError(f"stream {name} differs at {key}")


def check_operation(app, source, token):
    state = app.get("status", {}).get("operationState", {})
    if state.get("operation", {}).get("info") != token or not state.get("finishedAt") or app.get("operation"):
        return None
    result = state.get("syncResult", {})
    hooks = [r for r in result.get("resources", []) if r.get("kind") == "Job" and r.get("name") == "nats-bootstrap"]
    if (state.get("phase") != "Succeeded" or result.get("revision") != source or len(hooks) != 1
            or hooks[0].get("namespace") != NAMESPACE or hooks[0].get("syncPhase") != "PostSync"
            or hooks[0].get("hookPhase") != "Succeeded"):
        raise ValueError("Argo did not attest the exact-revision successful PostSync hook")
    return {"revision": source, "phase": state["phase"], "hookPhase": hooks[0]["hookPhase"],
            "syncPhase": hooks[0]["syncPhase"], "startedAt": state.get("startedAt"),
            "finishedAt": state["finishedAt"], "operation": token}


def source_ref(event_name, event, source):
    if event_name == "pull_request":
        number = event["number"]
        if type(number) is not int or number <= 0:
            raise ValueError("invalid pull request source identity")
        return f"refs/pull/{number}/merge"
    if event_name == "push" and event.get("ref") == "refs/heads/main" and event.get("after") == source:
        return source
    raise ValueError("only exact PR merge or actual main push is eligible")


def checked_record(value, fact):
    if (not isinstance(value, dict) or set(value) - {"subject", "seq", "data", "time", "hdrs"}
            or value.get("subject") != fact["subject"] or type(value.get("seq")) is not int or value["seq"] != 1
            or not isinstance(value.get("time"), str) or not value["time"]
            or base64.b64decode(value["data"], validate=True) != base64.b64decode(fact["frame"], validate=True)):
        raise ValueError("stored record differs from the canonical synthetic FACT")
    headers = base64.b64decode(value["hdrs"], validate=True).decode("ascii")
    if not headers.startswith("NATS/1.0\r\n") or not headers.endswith("\r\n\r\n"):
        raise ValueError("stored NATS header frame is missing")
    parsed = {}
    for line in headers.split("\r\n")[1:-2]:
        key, separator, text = line.partition(":")
        if not separator or key.lower() in parsed:
            raise ValueError("invalid stored NATS header")
        parsed[key.lower()] = text.strip()
    if parsed != {key.lower(): text for key, text in fact["headers"].items()}:
        raise ValueError("stored canonical FACT headers differ")
    return value


def check_report(report):
    if report.get("passed") is not True or report.get("cleanupPassed") is not True or report.get("cases") != CASES:
        raise ValueError("incomplete GitOps/PVC proof")
    source = report["source"]
    if not re.fullmatch(r"[0-9a-f]{40}", source) or not re.fullmatch(r"[0-9a-f]{40}", report["tree"]):
        raise ValueError("invalid source binding")
    first, second = report["syncs"]
    if any(s["revision"] != source or s["phase"] != "Succeeded" or s["hookPhase"] != "Succeeded"
           or s["syncPhase"] != "PostSync" for s in (first, second)):
        raise ValueError("source-bound hook evidence differs")
    if first["jobUID"] == second["jobUID"] or first["operation"] == second["operation"]:
        raise ValueError("re-sync reused the previous hook or operation")
    before, after = report["storage"]
    if before["podUID"] == after["podUID"] or any(before[key] != after[key] for key in ("pvc", "pvcUID", "pv", "pvUID")):
        raise ValueError("Pod replacement or persistent claim continuity differs")
    if report["factBefore"] != report["factAfter"] or not report["canonicalValidated"] or not report["negativeControlPassed"]:
        raise ValueError("canonical FACT durability proof differs")


class Fixture:
    def __init__(self, scratch, name):
        self.scratch, self.name = scratch, name
        self.kubeconfig = scratch / "kubeconfig"
        self.environment = {key: os.environ[key] for key in ("PATH", "RUNNER_TEMP") if key in os.environ}
        for key, path in (("HOME", scratch / "home"), ("TMPDIR", scratch / "tmp")):
            path.mkdir()
            self.environment[key] = str(path)
        self.environment["KUBECONFIG"] = str(self.kubeconfig)
        self.substage = "initialize"
        self.last_failure = None

    def command(self, args, data=None, timeout=300):
        try:
            result = subprocess.run(args, input=data, text=True, capture_output=True, timeout=timeout,
                                    cwd=ROOT, env=self.environment)
        except subprocess.TimeoutExpired:
            self.last_failure = {"substage": self.substage, "executable": Path(args[0]).name,
                                 "category": "command-timeout", "timeoutSeconds": timeout}
            raise RuntimeError("fixture command exceeded its bounded deadline") from None
        if result.returncode:
            # Pod and CLI diagnostics may contain certificate paths or payloads.
            # Preserve the failed stage/type, never raw output or ambient env.
            self.last_failure = {"substage": self.substage, "executable": Path(args[0]).name,
                                 "exitCode": result.returncode,
                                 "category": diagnostic_category(result.stdout[-32768:] + result.stderr[-32768:])}
            raise RuntimeError(f"fixture command refused: {Path(args[0]).name} exit={result.returncode}")
        return result.stdout.strip()

    def k(self, *args, data=None, timeout=300):
        return self.command(["kubectl", "--kubeconfig", str(self.kubeconfig), "--context", "kind-" + self.name,
                             *args], data=data, timeout=timeout)

    def apply(self, objects):
        self.k("apply", "--server-side", "-f", "-", data=yaml.safe_dump_all(objects))

    def install(self, target):
        feed = load_module("feed_identity_fixture", "tools/feed-identity/proof.py")
        # The shared cluster constructor accepts a bounded existing prefix. Its
        # command seam is scoped to this disposable fixture's empty environment.
        feed.command = self.command
        self.substage = "create-kind-and-enforce-Cilium"
        feed.create_enforcing_cluster(self.name, self.scratch)
        crds = []
        for path in sorted((ROOT / "kanz/infra/security/spire/crds").glob("*.yaml")):
            crds += documents(path.relative_to(ROOT))
        self.substage = "establish-SPIRE-CRDs"
        self.apply(crds)
        for item in crds:
            self.k("wait", "--for=condition=Established", "crd/" + item["metadata"]["name"], "--timeout=90s")
        self.substage = "install-SPIRE"
        self.apply(feed.spire_objects())
        self.substage = "wait-SPIRE-server"
        self.k("-n", "spire-system", "rollout", "status", "statefulset/spire-server", "--timeout=240s")
        self.substage = "wait-SPIRE-agent-and-CSI"
        self.k("-n", "spire-system", "rollout", "status", "daemonset/spire-agent", "--timeout=240s")
        self.substage = "install-canonical-SPIRE-registration-and-NATS-prerequisites"
        self.apply(documents("kanz/infra/security/spire/registration.yaml"))
        self.apply(documents("kanz/infra/nats/namespace.yaml"))
        # Production GitOps excludes tenancy.yaml because tenant provisioning
        # owns this ConfigMap. Load the exact source prerequisite without running
        # the stream bootstrap or changing any credential/ACL/routing contract.
        self.apply(documents("kanz/infra/nats/tenancy.yaml"))
        path = self.scratch / "argo-core.yaml"
        self.substage = "download-and-verify-pinned-Argo-Core"
        self.command(["curl", "--fail", "--silent", "--show-error", "--location", "--max-time", "90",
                      "-o", str(path), f"https://raw.githubusercontent.com/argoproj/argo-cd/{ARGO_VERSION}/manifests/core-install.yaml"], timeout=95)
        raw = path.read_bytes()
        if hashlib.sha256(raw).hexdigest() != ARGO_SHA:
            raise ValueError("Argo Core manifest digest differs")
        core = list(yaml.safe_load_all(raw))
        for obj in core:
            if obj.get("kind") in ("Deployment", "StatefulSet"):
                pod = obj["spec"]["template"]["spec"]
                for container in pod.get("containers", []) + pod.get("initContainers", []):
                    container["image"] = ARGO_IMAGES[container["image"]]
        self.apply([{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "argocd"}}])
        self.substage = "install-pinned-Argo-Core"
        self.k("-n", "argocd", "apply", "--server-side", "-f", "-", data=yaml.safe_dump_all(core))
        for name in ("deploy/argocd-redis", "deploy/argocd-repo-server", "statefulset/argocd-application-controller"):
            self.substage = "wait-" + name.rsplit("/", 1)[-1]
            self.k("-n", "argocd", "rollout", "status", name, "--timeout=240s")
        appset = documents("kanz/infra/gitops/applicationset.yaml")[0]
        directory = copy.deepcopy(appset["spec"]["template"]["spec"]["source"]["directory"])
        self.substage = "register-source-bound-NATS-Application"
        self.apply([
            {"apiVersion": "argoproj.io/v1alpha1", "kind": "AppProject", "metadata": {"name": APP, "namespace": "argocd"},
             "spec": {"sourceRepos": ["https://github.com/eighred/kanz.git"],
                      "destinations": [{"server": "https://kubernetes.default.svc", "namespace": NAMESPACE}],
                      "clusterResourceWhitelist": [{"group": "", "kind": "Namespace"}],
                      "namespaceResourceWhitelist": [{"group": "", "kind": "ConfigMap"}, {"group": "", "kind": "ServiceAccount"},
                          {"group": "", "kind": "Service"}, {"group": "apps", "kind": "StatefulSet"},
                          {"group": "policy", "kind": "PodDisruptionBudget"}, {"group": "batch", "kind": "Job"}]}},
            {"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": {"name": APP, "namespace": "argocd"},
             "spec": {"project": APP, "source": {"repoURL": "https://github.com/eighred/kanz.git", "targetRevision": target,
                        "path": "kanz/infra/nats", "directory": directory},
                      "destination": {"server": "https://kubernetes.default.svc", "namespace": NAMESPACE},
                      "syncPolicy": {"syncOptions": ["CreateNamespace=true", "ApplyOutOfSyncOnly=true"]}}},
        ])

    def sync(self, source, case):
        self.substage = "Argo-" + case
        token = [{"name": "nats-gitops-case", "value": case + "-" + uuid.uuid4().hex}]
        self.k("-n", "argocd", "patch", "application", APP, "--type=merge", "-p", json.dumps({
            "metadata": {"annotations": {"argocd.argoproj.io/refresh": "normal"}}}))
        # PR test-merge commits are advertised under refs/pull/N/merge. The
        # configured ref obtains that object; comparison must resolve exactly
        # the checkout before an immutable-SHA sync is requested. A moving ref
        # fails, never substitutes current main or the contributor head.
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            app = json.loads(self.k("-n", "argocd", "get", "application", APP, "-o", "json", timeout=15))
            status = app.get("status", {}).get("sync", {})
            if (status.get("revision") == source and status.get("status") in ("Synced", "OutOfSync")
                    and not app.get("operation") and not app["metadata"].get("annotations", {}).get("argocd.argoproj.io/refresh")):
                break
            time.sleep(2)
        else:
            raise TimeoutError("Argo source comparison did not resolve the exact workflow commit")
        self.k("-n", "argocd", "patch", "application", APP, "--type=merge", "-p", json.dumps({
            "operation": {"info": token, "sync": {"revision": source}}}))
        deadline = time.monotonic() + 600
        while time.monotonic() < deadline:
            app = json.loads(self.k("-n", "argocd", "get", "application", APP, "-o", "json", timeout=15))
            row = check_operation(app, source, token)
            if row:
                job = json.loads(self.k("-n", NAMESPACE, "get", "job/nats-bootstrap", "-o", "json"))
                if not job.get("status", {}).get("succeeded") or job["metadata"]["annotations"].get("argocd.argoproj.io/hook") != "PostSync":
                    raise ValueError("Argo reported success without a completed actual hook")
                row["jobUID"] = job["metadata"]["uid"]
                return row
            time.sleep(2)
        raise TimeoutError("source-bound Argo PostSync operation expired")

    def client(self, name, namespace, service_account):
        self.substage = "issue-client-SVID-" + name
        bootstrap = documents("kanz/infra/nats/bootstrap-job.yaml")
        job = next(d for d in bootstrap if d["kind"] == "Job")
        spec = copy.deepcopy(job["spec"]["template"]["spec"])
        spec["serviceAccountName"] = service_account
        spec["restartPolicy"] = "Never"
        spec["containers"][0]["command"] = ["sleep", "1800"]
        spec["containers"][0]["args"] = []
        spec["volumes"] = [v for v in spec["volumes"] if v["name"] != "scripts"]
        spec["containers"][0]["volumeMounts"] = [v for v in spec["containers"][0]["volumeMounts"] if v["name"] != "scripts"]
        helper = copy.deepcopy(next(d for d in bootstrap if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "nats-bootstrap-spiffe-helper"))
        helper["metadata"]["namespace"] = namespace
        self.apply([{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": namespace, "labels": {"kanz.internal/spiffe": "enabled"}}},
                    {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": service_account, "namespace": namespace}}, helper,
                    {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": namespace}, "spec": spec}])
        self.k("-n", namespace, "wait", "--for=condition=Ready", "pod/" + name, "--timeout=180s")

    def nats(self, *args, name="nats-inspector", namespace=NAMESPACE, data=None):
        return self.k("-n", namespace, "exec", "-i", name, "-c", "bootstrap", "--", "nats", *args, data=data)

    def inventory(self):
        names = json.loads(self.nats("stream", "ls", "--json", "--all"))
        if not isinstance(names, list) or len(names) != len(set(names)) or not all(re.fullmatch(r"[A-Z_]+", x) for x in names):
            raise ValueError("invalid observed stream names")
        return {name: json.loads(self.nats("stream", "info", name, "--json")) for name in names}

    def storage(self):
        pod = json.loads(self.k("-n", NAMESPACE, "get", "pod/nats-0", "-o", "json"))
        data = next(v for v in pod["spec"]["volumes"] if v["name"] == "data")
        claim = data["persistentVolumeClaim"]["claimName"]
        pvc = json.loads(self.k("-n", NAMESPACE, "get", "pvc", claim, "-o", "json"))
        pv = json.loads(self.k("get", "pv", pvc["spec"]["volumeName"], "-o", "json"))
        if pvc["status"]["phase"] != "Bound" or pv["spec"]["claimRef"]["uid"] != pvc["metadata"]["uid"]:
            raise ValueError("NATS data claim is not bound to the observed PV")
        return {"podUID": pod["metadata"]["uid"], "pvc": claim, "pvcUID": pvc["metadata"]["uid"],
                "pv": pv["metadata"]["name"], "pvUID": pv["metadata"]["uid"]}

    def canonical_runtime(self):
        desired = documents("kanz/infra/nats/nats.yaml")
        configs = [d for d in desired + documents("kanz/infra/nats/tenancy.yaml")
                   + documents("kanz/infra/nats/bootstrap-job.yaml") if d["kind"] == "ConfigMap"]
        hashes = {}
        for wanted in configs:
            name = wanted["metadata"]["name"]
            actual = json.loads(self.k("-n", NAMESPACE, "get", "configmap", name, "-o", "json"))
            if actual.get("data") != wanted["data"]:
                raise ValueError("runtime NATS TLS/identity/tenancy/bootstrap config differs from source")
            hashes[name] = hashlib.sha256(json.dumps(actual["data"], sort_keys=True).encode()).hexdigest()
        wanted = next(d for d in desired if d["kind"] == "StatefulSet")["spec"]
        actual = json.loads(self.k("-n", NAMESPACE, "get", "statefulset/nats", "-o", "json"))["spec"]
        if actual["replicas"] != wanted["replicas"] or actual["replicas"] != 3:
            raise ValueError("canonical broker replica count differs")
        claim = next(d for d in actual["volumeClaimTemplates"] if d["metadata"]["name"] == "data")
        if claim["spec"]["resources"]["requests"] != wanted["volumeClaimTemplates"][0]["spec"]["resources"]["requests"]:
            raise ValueError("canonical data PVC request differs")
        for ordinal in range(3):
            pod = json.loads(self.k("-n", NAMESPACE, "get", f"pod/nats-{ordinal}", "-o", "json"))["spec"]
            volumes = {v["name"]: v for v in pod["volumes"]}
            broker = next(c for c in pod["containers"] if c["name"] == "nats")
            if ("persistentVolumeClaim" not in volumes["data"] or pod["serviceAccountName"] != "nats"
                    or volumes["spiffe"].get("csi", {}).get("driver") != "csi.spiffe.io"
                    or not any(m["name"] == "data" and m["mountPath"] == "/data" for m in broker["volumeMounts"])):
                raise ValueError("canonical broker identity or persistent data mount differs")
        return hashes

    def replace_broker(self, previous_uid):
        self.substage = "replace-broker-Pod-preserving-claim"
        self.k("-n", NAMESPACE, "delete", "pod/nats-0", "--wait=true", "--timeout=120s")
        deadline = time.monotonic() + 240
        while time.monotonic() < deadline:
            raw = self.k("-n", NAMESPACE, "get", "pod/nats-0", "--ignore-not-found", "-o", "json")
            if raw and json.loads(raw)["metadata"]["uid"] != previous_uid:
                break
            time.sleep(2)
        else:
            raise TimeoutError("StatefulSet did not create a replacement broker Pod")
        self.k("-n", NAMESPACE, "wait", "--for=condition=Ready", "pod/nats-0", "--timeout=240s")
        self.k("-n", NAMESPACE, "rollout", "status", "statefulset/nats", "--timeout=240s")

    def status_diagnostics(self):
        """Project bounded object statuses, never Pod specs, Secrets or raw logs."""
        args = ["kubectl", "--kubeconfig", str(self.kubeconfig), "--context", "kind-" + self.name,
                "get", "pods", "-A", "-o", "json", "--request-timeout=10s"]
        def word(value):
            return value if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", value) else "UNKNOWN"
        try:
            result = subprocess.run(args, text=True, capture_output=True, timeout=15, cwd=ROOT, env=self.environment)
            if result.returncode:
                return {"available": False, "category": diagnostic_category(result.stderr[-32768:])}
            pods = json.loads(result.stdout)["items"]
            rows = []
            for pod in pods[:32]:
                state = pod.get("status", {})
                containers = []
                for item in state.get("initContainerStatuses", []) + state.get("containerStatuses", []):
                    status = item.get("state", {}).get("waiting", item.get("state", {}).get("terminated", {}))
                    containers.append({"name": word(item.get("name")), "ready": item.get("ready") is True,
                                       "reason": word(status.get("reason")), "exitCode": status.get("exitCode"),
                                       "messageCategory": diagnostic_category(status.get("message", ""))})
                rows.append({"namespace": word(pod["metadata"].get("namespace")), "pod": word(pod["metadata"].get("name")),
                             "phase": word(state.get("phase")), "conditions": [{"type": word(c.get("type")),
                                 "status": word(c.get("status")), "reason": word(c.get("reason")),
                                 "messageCategory": diagnostic_category(c.get("message", ""))} for c in state.get("conditions", [])],
                             "containers": containers})
            return {"available": True, "totalPods": len(pods), "includedPods": len(rows), "pods": rows}
        except Exception as error:
            return {"available": False, "errorType": type(error).__name__}


def main():
    if os.name != "posix" or os.environ.get("GITHUB_ACTIONS") != "true":
        raise RuntimeError("this proof runs only in its disposable Linux CI job")
    source = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    tree = subprocess.check_output(["git", "rev-parse", "HEAD^{tree}"], cwd=ROOT, text=True).strip()
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("invalid source commit")
    if source != os.environ.get("GITHUB_SHA"):
        raise ValueError("checkout does not match the workflow commit")
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    target = source_ref(os.environ["GITHUB_EVENT_NAME"], event, source)
    artifact = Path(os.environ["RUNNER_TEMP"]) / "nats-gitops.json"
    report = {"source": source, "tree": tree, "runID": os.environ.get("GITHUB_RUN_ID"), "runAttempt": os.environ.get("GITHUB_RUN_ATTEMPT"),
              "sourceRef": target, "cases": [], "syncs": [], "passed": False, "cleanupPassed": False, "stage": "initialize"}
    with tempfile.TemporaryDirectory(prefix="nats-gitops-", dir=os.environ["RUNNER_TEMP"]) as temporary:
        fixture = Fixture(Path(temporary), "messaging-proof-" + uuid.uuid4().hex[:12])
        report["cluster"] = fixture.name
        owner = Path(os.environ["RUNNER_TEMP"]) / "nats-gitops-owner.json"
        with owner.open("x", encoding="utf-8") as output:
            json.dump({"cluster": fixture.name, "source": source, "runID": report["runID"], "runAttempt": report["runAttempt"]}, output)
        try:
            job_docs = documents("kanz/infra/nats/bootstrap-job.yaml")
            job = next(d for d in job_docs if d["kind"] == "Job")
            script = next(d for d in job_docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "nats-bootstrap")["data"]["bootstrap.sh"]
            expected = expected_streams(script, 3)
            report["sourceHashes"] = {path: hashlib.sha256((ROOT / path).read_bytes()).hexdigest() for path in
                ("kanz/infra/nats/nats.yaml", "kanz/infra/nats/bootstrap-job.yaml", "kanz/infra/nats/tenancy.yaml",
                 "kanz/infra/security/spire/registration.yaml", "kanz/infra/gitops/applicationset.yaml")}
            if job["metadata"]["annotations"]["argocd.argoproj.io/hook"] != "PostSync":
                raise ValueError("canonical hook is absent")
            report["cases"].append("canonical-source")
            report["stage"] = "install disposable SPIRE and Argo"
            fixture.install(target)
            report["stage"] = "first Argo sync"
            report["syncs"].append(fixture.sync(source, "first"))
            report["cases"].append("first-postsync")
            report["runtimeConfigHashes"] = fixture.canonical_runtime()
            report["cases"].append("canonical-runtime")
            fixture.client("nats-inspector", NAMESPACE, "nats-bootstrap")
            report["versions"] = {"argoCD": ARGO_VERSION}
            for name, text in (("natsServer", fixture.k("-n", NAMESPACE, "exec", "nats-0", "-c", "nats", "--", "nats-server", "--version")),
                               ("natsCLI", fixture.nats("--version"))):
                match = re.search(r"\bv?[0-9]+\.[0-9]+\.[0-9]+\b", text)
                if not match:
                    raise ValueError("runtime dependency version is unresolved")
                report["versions"][name] = match.group(0)
            before = fixture.inventory()
            check_inventory(expected, before)
            report["streams"] = {name: row["config"] for name, row in before.items()}
            report["cases"].append("stream-inventory")
            fact = json.loads((Path(os.environ["RUNNER_TEMP"]) / "nats-gitops-fact.json").read_text())
            if fact["source"] != source or not fact["canonicalValidated"] or not fact["negativeControlPassed"]:
                raise ValueError("canonical FACT helper source or controls differ")
            frame = base64.b64decode(fact["frame"], validate=True)
            if hashlib.sha256(frame).hexdigest() != fact["sha256"]:
                raise ValueError("canonical FACT frame digest differs")
            fixture.client("nats-fact-writer", "kanz-services", "market-ingest")
            # Binary stdin is decoded inside the owned client Pod, not interpolated
            # as a shell expression or published through a privileged account.
            headers = [arg for key, value in fact["headers"].items() for arg in ("--header", key + ":" + value)]
            fixture.k("-n", "kanz-services", "exec", "-i", "nats-fact-writer", "-c", "bootstrap", "--", "sh", "-ceu",
                      "base64 -d > /tmp/fixture.frame; nats pub --jetstream \"$@\" < /tmp/fixture.frame", "fixture", *headers, fact["subject"], data=fact["frame"])
            record = checked_record(json.loads(fixture.nats("stream", "get", "MARKET", "1", "--json")), fact)
            report["factBefore"] = record
            report["canonicalValidated"], report["negativeControlPassed"] = True, True
            report["cases"].append("canonical-fact")
            report["stage"] = "delete designated empty fixture stream"
            designated = fixture.inventory()[DELETE_STREAM]["state"]
            if designated["messages"] != 0 or designated["consumer_count"] != 0:
                raise ValueError("designated disposable stream is not empty")
            fixture.nats("stream", "rm", DELETE_STREAM, "--force")
            if DELETE_STREAM in fixture.inventory():
                raise ValueError("designated stream was not removed")
            report["cases"].append("empty-stream-removed")
            report["stage"] = "same-revision Argo re-sync"
            report["syncs"].append(fixture.sync(source, "restore-empty-stream"))
            if report["syncs"][0]["jobUID"] == report["syncs"][1]["jobUID"]:
                raise ValueError("Argo reused the completed hook")
            report["cases"].append("same-revision-postsync")
            restored = fixture.inventory()
            check_inventory(expected, restored)
            if restored[DELETE_STREAM]["config"] != before[DELETE_STREAM]["config"]:
                raise ValueError("restored test stream configuration differs")
            report["cases"].append("stream-restored")
            report["stage"] = "replace NATS Pod and recover existing FACT"
            report["storage"] = [fixture.storage()]
            fixture.replace_broker(report["storage"][0]["podUID"])
            report["storage"].append(fixture.storage())
            report["cases"].extend(["pod-replaced", "pvc-preserved"])
            report["factAfter"] = checked_record(json.loads(fixture.nats("stream", "get", "MARKET", "1", "--json")), fact)
            if report["factAfter"] != report["factBefore"]:
                raise ValueError("FACT bytes, subject, headers or sequence changed after replacement")
            check_inventory(expected, fixture.inventory())
            if fixture.canonical_runtime() != report["runtimeConfigHashes"]:
                raise ValueError("runtime configuration changed during recovery")
            report["cases"].append("fact-preserved")
            pods = json.loads(fixture.k("get", "pods", "-A", "-o", "json"))["items"]
            report["runtimeImages"] = [{"namespace": p["metadata"]["namespace"], "pod": p["metadata"]["name"],
                "images": [{"name": c["name"], "imageID": c.get("imageID", "")} for c in p.get("status", {}).get("containerStatuses", [])]} for p in pods]
            report["passed"] = True
        except Exception as error:
            report["failureStage"], report["errorType"] = report["stage"], type(error).__name__
            report["substage"] = fixture.substage
            report["commandFailure"] = fixture.last_failure
            report["objectStatuses"] = fixture.status_diagnostics()
            raise
        finally:
            try:
                fixture.command(["kind", "delete", "cluster", "--name", fixture.name, "--kubeconfig", str(fixture.kubeconfig)], timeout=180)
                report["cleanupPassed"] = fixture.name not in fixture.command(["kind", "get", "clusters"]).splitlines()
                if report["cleanupPassed"]:
                    report["cases"].append("cleanup")
            except Exception as error:
                report["cleanupErrorType"] = type(error).__name__
            report["passed"] &= report["cleanupPassed"]
            if report["passed"]:
                try:
                    check_report(report)
                except Exception as error:
                    report["passed"], report["receiptErrorType"] = False, type(error).__name__
            # A fresh runner-local immutable receipt; no private keys, kubeconfig,
            # raw Pod logs, token, or ambient environment is retained.
            with artifact.open("x", encoding="utf-8") as output:
                json.dump(report, output, indent=2)
                output.write("\n")
            artifact.chmod(0o444)
    if not report["passed"]:
        raise RuntimeError("complete disposable GitOps/PVC acceptance did not pass")


if __name__ == "__main__":
    main()

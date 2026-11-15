"""Owned Linux SPIRE/NATS fixture. It never selects an existing cluster/context."""
import copy
import argparse
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid

import yaml

ROOT = Path(__file__).resolve().parents[2]
_spec = importlib.util.spec_from_file_location("agent_feed_identity", ROOT / "tools/feed-identity/proof.py")
FEED = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(FEED)
RUNTIME = "ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f"
MARKER = "AGENT_IDENTITY_RESULT:"
CAP = 4 * 1024 * 1024
PROBE_NAMESPACES = ("kanz-services", "kanz-messaging")
PUBLICATIONS = {
    "order_submit_denied": "order.order.submit", "order_cancel_denied": "order.order.cancel",
    "order_amend_denied": "order.order.amend", "order_approve_denied": "order.order.approve",
    "tenant_order_submit_denied": "tenant.acme.order.order.submit", "tenant_order_cancel_denied": "tenant.acme.order.order.cancel",
    "tenant_order_amend_denied": "tenant.acme.order.order.amend", "tenant_order_approve_denied": "tenant.acme.order.order.approve",
    "mandate_policy_denied": "compliance.mandate.changed.acme.fixture", "risk_policy_denied": "risk.portfolio.revalued",
    "kill_switch_denied": "platform.mode.changed", "jetstream_stream_create_denied": "$JS.API.STREAM.CREATE.AGENT_FORBIDDEN",
    "jetstream_stream_delete_denied": "$JS.API.STREAM.DELETE.AGENT_IDENTITY",
    "jetstream_consumer_create_denied": "$JS.API.CONSUMER.CREATE.AGENT_IDENTITY",
}
SUBSCRIPTIONS = {"order_subscription_denied": "order.>", "risk_subscription_denied": "risk.>",
                 "mandate_subscription_denied": "compliance.mandate.>", "foreign_tenant_subscription_denied": "tfact.acme.order.>"}
REQUIRED_CASES = set(PUBLICATIONS) | set(SUBSCRIPTIONS) | {
    "copilot_svid_issued", "authz_publish_allowed", "answer_publish_allowed", "capital_publish_denied",
    "control_publish_denied", "jetstream_admin_denied", "protected_subscription_denied",
    "unregistered_identity_denied", "wrong_server_identity_denied", "wrong_server_positive_control",
    "grant_widening_detected", "owned_cleanup",
    "probe_egress_denied", "probe_egress_positive_control",
}


def digest(value):
    return hashlib.sha256(value).hexdigest()


def canonical_tenancy():
    docs = FEED.manifests("kanz/infra/nats/tenancy.yaml")
    if len(docs) != 1 or docs[0].get("kind") != "ConfigMap" or docs[0]["metadata"]["name"] != "nats-tenants":
        raise ValueError("canonical tenancy ConfigMap changed")
    body = docs[0]["data"]["tenants.conf"]
    if body.count('user: "spiffe://kanz.internal/ns/kanz-services/sa/copilot"') != 1:
        raise ValueError("canonical copilot identity missing or duplicated")
    if "agent-identity-unregistered" in body:
        raise ValueError("negative identity is already registered")
    return docs[0]


def widen_copy(tenancy):
    """Only a disposable copy is widened. No source grants are edited."""
    result = copy.deepcopy(tenancy)
    body = result["data"]["tenants.conf"]
    pattern = r'(user: "spiffe://kanz.internal/ns/kanz-services/sa/copilot"\s+permissions: \{\s+publish: \{\s+allow: \[)(.*?)(\])'
    body, count = re.subn(pattern, lambda m: m[1] + m[2] + '\n                  ">",\n                ' + m[3], body, flags=re.S)
    if count != 1:
        raise ValueError("copilot publish mutation did not target exactly one grant")
    result["metadata"]["name"] = "agent-identity-mutant-tenants"
    result["data"]["tenants.conf"] = body
    return result


def broker_objects():
    # Reuse the approved TLS, helper, SPIRE, storage and routing adaptations.
    # Replace the feed fixture's synthetic users with the complete source ACL.
    docs = FEED.broker_objects()
    docs = [d for d in docs if not (d["kind"] == "ConfigMap" and d["metadata"]["name"] == "nats-tenants")]
    docs.append(canonical_tenancy())
    return docs


def mutant_broker(docs):
    template = next(d for d in docs if d["kind"] == "StatefulSet" and d["metadata"]["name"] == "nats")
    result = copy.deepcopy(template)
    result["metadata"]["name"] = "nats-mutant"
    result["spec"]["serviceName"] = "nats-mutant"
    result["spec"]["selector"]["matchLabels"] = {"app": "nats-mutant"}
    pod = result["spec"]["template"]
    pod["metadata"]["labels"]["app"] = "nats-mutant"
    # The same genuine broker identity makes this an ACL mutation control,
    # rather than a different-peer TLS refusal masquerading as a policy test.
    pod["spec"]["serviceAccountName"] = "nats"
    changed = 0
    for volume in pod["spec"]["volumes"]:
        for source in volume.get("projected", {}).get("sources", []):
            if source.get("configMap", {}).get("name") == "nats-tenants":
                source["configMap"]["name"] = "agent-identity-mutant-tenants"
                changed += 1
    if changed != 1:
        raise ValueError("canonical tenant mount changed")
    return result


def probe_job(name, namespace, sa, mode, address, image, recorder=False):
    env = [{"name": "AGENT_IDENTITY_NATS", "value": "tls://" + address}]
    if recorder:
        command = ["/copilot.test", "-test.run=^TestAgentIdentityRecorder$", "-test.v", "-test.timeout=75s"]
        env += [{"name": "AGENT_IDENTITY_RECORDER", "value": "disposable-spire"}, {"name": "AGENT_IDENTITY_MODE", "value": mode}]
    else:
        command = ["/identity_probe", "--mode", mode, "--nats", "tls://" + address,
                   "--namespace", namespace, "--service-account", sa]
    return {"apiVersion": "batch/v1", "kind": "Job", "metadata": {"name": name, "namespace": namespace},
            "spec": {"backoffLimit": 0, "activeDeadlineSeconds": 90, "template": {
                "metadata": {"labels": {"agent-identity-probe": "true"}},
                "spec": {"serviceAccountName": sa, "automountServiceAccountToken": False, "restartPolicy": "Never",
                    "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
                    "containers": [{"name": "probe", "image": image, "imagePullPolicy": "Never", "command": command, "env": env,
                        "resources": {"requests": {"cpu": "50m", "memory": "64Mi"}, "limits": {"cpu": "1", "memory": "256Mi"}},
                        "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                        "volumeMounts": [{"name": "spiffe", "mountPath": "/run/spiffe", "readOnly": True}]}],
                    "volumes": [{"name": "spiffe", "csi": {"driver": "csi.spiffe.io", "readOnly": True}}]}}}}


def namespace_peer(namespace, labels):
    return {"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": namespace}},
            "podSelector": {"matchLabels": labels}}


def probe_policies():
    # Kubernetes combines selectors in one peer with AND. No DNS/public/host
    # grant: these processes use literal owned IPs and the Unix Workload API.
    return [{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
             "metadata": {"name": "agent-identity-probes", "namespace": namespace},
             "spec": {"podSelector": {"matchLabels": {"agent-identity-probe": "true"}},
                      "policyTypes": ["Ingress", "Egress"], "ingress": [],
                      "egress": [{"to": [namespace_peer("kanz-messaging", {"identity-proof-broker": "true"})],
                                  "ports": [{"protocol": "TCP", "port": 4222}]}]}}
            for namespace in PROBE_NAMESPACES]


def validate_probe_policy(document):
    if (document.get("kind") != "NetworkPolicy" or document.get("metadata", {}).get("namespace") not in PROBE_NAMESPACES
            or document.get("metadata", {}).get("name") != "agent-identity-probes"):
        raise ValueError("unowned probe network policy")
    expected = next(d["spec"] for d in probe_policies() if d["metadata"]["namespace"] == document["metadata"]["namespace"])
    observed = copy.deepcopy(document.get("spec", {}))
    observed.setdefault("ingress", [])  # API serialization can omit an empty list.
    if observed != expected:
        raise ValueError("probe ingress/egress boundary changed")


def validate_probe_policies(documents):
    if len(documents) != 2 or {d.get("metadata", {}).get("namespace") for d in documents} != {"kanz-services", "kanz-messaging"}:
        raise ValueError("both probe namespaces require isolation")
    for document in documents:
        validate_probe_policy(document)


def network_controls(image):
    container = {"name": "inert", "image": image, "imagePullPolicy": "Never",
                 "command": ["/identity_probe", "--mode", "network-listener"],
                 "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                 "resources": {"requests": {"cpu": "10m", "memory": "16Mi"}, "limits": {"cpu": "100m", "memory": "64Mi"}},
                 "readinessProbe": {"tcpSocket": {"port": 4222}, "periodSeconds": 1}}
    docs = [{"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "agent-identity-inert", "namespace": "kanz-services",
             "labels": {"agent-identity-inert": "true"}},
             "spec": {"restartPolicy": "Never", "activeDeadlineSeconds": 200, "automountServiceAccountToken": False,
                      "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
                      "containers": [container]}}]
    docs.append({"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
                 "metadata": {"name": "agent-identity-inert", "namespace": "kanz-services"},
                 "spec": {"podSelector": {"matchLabels": {"agent-identity-inert": "true"}},
                          "policyTypes": ["Ingress", "Egress"], "egress": [],
                          "ingress": [{"from": [namespace_peer(ns, {"agent-identity-network-client": "true"}) for ns in PROBE_NAMESPACES],
                                       "ports": [{"protocol": "TCP", "port": 4222}]}]}})
    for namespace in PROBE_NAMESPACES:
        docs.append({"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
                     "metadata": {"name": "agent-identity-network-control", "namespace": namespace},
                     "spec": {"podSelector": {"matchLabels": {"agent-identity-network-control": "true"}},
                              "policyTypes": ["Ingress", "Egress"], "ingress": [],
                              "egress": [{"to": [namespace_peer("kanz-services", {"agent-identity-inert": "true"})],
                                          "ports": [{"protocol": "TCP", "port": 4222}]}]}})
    return docs


def receipt(logs):
    records = [json.loads(line.split(MARKER, 1)[1]) for line in logs.splitlines() if line.startswith(MARKER)]
    if len(records) != 1 or not isinstance(records[0].get("cases"), dict):
        raise ValueError("exactly one bounded probe result is required")
    return records[0]


def command_environment(scratch, module_cache, goroot):
    # Explicit local Docker endpoint; no inherited kubeconfig, tokens, proxy,
    # Docker context/config, Go flags, remote engine or cloud credential paths.
    return {"PATH": os.environ["PATH"], "HOME": str(scratch / "home"), "TMPDIR": str(scratch / "tmp"),
            "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "DOCKER_HOST": "unix:///var/run/docker.sock",
            "DOCKER_CONFIG": str(scratch / "docker"), "GOCACHE": str(scratch / "go-cache"),
            "GOMODCACHE": module_cache, "GOROOT": goroot, "GOENV": "off", "GOTOOLCHAIN": "local",
            "GOFLAGS": "-mod=readonly", "GOPROXY": "off", "GOSUMDB": "off",
            "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}


class Runner:
    def __init__(self, scratch, environment):
        self.scratch, self.environment = scratch, environment

    def command(self, args, data=None, timeout=300, cwd=ROOT):
        import resource  # Linux runtime only; offline model tests work on Windows.

        def limits():
            resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
            # Go link outputs exceed the much smaller retained-output bound.
            resource.setrlimit(resource.RLIMIT_FSIZE, (256 << 20, 256 << 20))
            resource.setrlimit(resource.RLIMIT_NOFILE, (1024, 1024))
            resource.setrlimit(resource.RLIMIT_NPROC, (1024, 1024))
            resource.setrlimit(resource.RLIMIT_AS, (8 << 30, 8 << 30))
            resource.setrlimit(resource.RLIMIT_CPU, (math.ceil(timeout) + 10, math.ceil(timeout) + 10))

        with tempfile.TemporaryFile(dir=self.scratch) as stdout, tempfile.TemporaryFile(dir=self.scratch) as stderr:
            child = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=stdout, stderr=stderr,
                                     cwd=cwd, env=self.environment, start_new_session=True, preexec_fn=limits)
            try:
                child.communicate(None if data is None else data.encode(), timeout=timeout)
            except subprocess.TimeoutExpired:
                os.killpg(child.pid, signal.SIGKILL)
                child.wait(timeout=10)
                raise RuntimeError("bounded fixture command timed out") from None
            except BaseException:
                if child.poll() is None:
                    os.killpg(child.pid, signal.SIGKILL)
                child.wait(timeout=10)
                raise
            if child.returncode:
                raise RuntimeError("fixture command refused: " + str(args[0]) + " exit=" + str(child.returncode))
            size = stdout.tell()
            if size > CAP or stderr.tell() > CAP:
                raise ValueError("fixture output exceeded bound")
            stdout.seek(0)
            return stdout.read(CAP + 1).decode("utf-8").strip()


def protected_directory(value):
    if not value or not os.path.isabs(value):
        raise ValueError("absolute evidence directory required")
    path = Path(os.path.abspath(value))
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current = current / part
        try:
            info = current.lstat()
        except FileNotFoundError:
            current.mkdir(mode=0o700)
            info = current.lstat()
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
            raise ValueError("evidence parent is not an ordinary directory")
    return path


def write_evidence(directory, evidence):
    write_json(directory, "identity.json", evidence)


def write_json(directory, filename, evidence):
    if filename not in {"identity.json", "ownership.json", "cleanup.json"}:
        raise ValueError("unknown fixture output")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
    fd = os.open(directory / filename, flags, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as output:
        json.dump(evidence, output, indent=2, sort_keys=True)
        output.write("\n")


def validate_ownership(value, expected):
    fields = {"schema", "source", "tree", "run", "attempt", "cluster"}
    if not isinstance(value, dict) or set(value) != fields or type(value.get("schema")) is not int or value["schema"] != 1:
        raise ValueError("malformed fixture ownership")
    if any(value.get(key) != expected[key] for key in ("source", "tree", "run", "attempt")):
        raise ValueError("stale or foreign fixture ownership")
    if not re.fullmatch(r"[0-9a-f]{40}", value["source"]) or not re.fullmatch(r"[0-9a-f]{40}", value["tree"]):
        raise ValueError("invalid source ownership")
    if not re.fullmatch(r"[1-9][0-9]*", value["run"]) or not re.fullmatch(r"[1-9][0-9]*", value["attempt"]):
        raise ValueError("invalid run ownership")
    if not isinstance(value["cluster"], str) or not re.fullmatch(r"feed-proof-[0-9a-f]{12}", value["cluster"]):
        raise ValueError("invalid owned cluster grammar")
    return value


def read_ownership(directory):
    try:
        fd = os.open(directory / "ownership.json", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    with os.fdopen(fd, "rb") as source:
        before = os.fstat(source.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or not 0 < before.st_size <= 4096:
            raise ValueError("invalid ownership file")
        data = source.read(4097)
        after = os.fstat(source.fileno())
        if len(data) != before.st_size or (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
            raise ValueError("ownership changed while reading")
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate ownership key")
            result[key] = value
        return result
    return json.loads(data, object_pairs_hook=unique)


def owned_image(cluster):
    if not re.fullmatch(r"feed-proof-[0-9a-f]{12}", cluster):
        raise ValueError("invalid owned image source")
    return "kanz-agent-identity-" + cluster.removeprefix("feed-proof-") + ":fixture"


def cleanup_owner(runner, owner, kubeconfig):
    """Sole resource deletion path, shared by normal and interruption cleanup."""
    name = owner["cluster"]
    image = owned_image(name)
    inventory = runner.command(["kind", "get", "clusters"], timeout=30).splitlines()
    if name in inventory:
        runner.command(["kind", "delete", "cluster", "--name", name, "--kubeconfig", str(kubeconfig)], timeout=180)
    result = {"cluster_absent": name not in runner.command(["kind", "get", "clusters"], timeout=30).splitlines(),
              "owned_containers_absent": not runner.command(["docker", "ps", "-aq", "--filter", "label=io.x-k8s.kind.cluster=" + name], timeout=30)}
    if runner.command(["docker", "image", "ls", "-q", image], timeout=30):
        label = runner.command(["docker", "image", "inspect", "--format", '{{index .Config.Labels "dev.kanz.agent-identity"}}', image], timeout=30)
        if label != name:
            raise ValueError("refusing to remove an image without this run's ownership label")
        runner.command(["docker", "image", "rm", image], timeout=60)
    result["fixture_image_absent"] = not runner.command(["docker", "image", "ls", "-q", image], timeout=30)
    return result


def expected_owner(runner):
    values = {"source": runner.command(["git", "rev-parse", "HEAD"]),
              "tree": runner.command(["git", "rev-parse", "HEAD^{tree}"]),
              "run": os.environ.get("GITHUB_RUN_ID", ""), "attempt": os.environ.get("GITHUB_RUN_ATTEMPT", "")}
    if any(not re.fullmatch(r"[1-9][0-9]*", values[k]) for k in ("run", "attempt")):
        raise ValueError("CI run/attempt required for cleanup")
    return values


def cleanup_recorded(output):
    receipt_value = {"schema": 1, "recorded": False}
    status = 1
    try:
        with tempfile.TemporaryDirectory(prefix="kanz-agent-cleanup-") as directory:
            scratch = Path(directory)
            for name in ("home", "tmp", "docker"):
                (scratch / name).mkdir(mode=0o700)
            runner = Runner(scratch, command_environment(scratch, "", ""))
            expected = expected_owner(runner)
            receipt_value.update(expected)
            value = read_ownership(output)
            if value is None:
                # Missing ownership names no resource. It is not proof of
                # cluster absence or a successful qualification.
                status = 0
            else:
                owner = validate_ownership(value, expected)
                receipt_value.update({"recorded": True, "cluster": owner["cluster"]})
                receipt_value.update(cleanup_owner(runner, owner, scratch / "kubeconfig"))
                status = 0 if all(receipt_value[k] is True for k in ("cluster_absent", "owned_containers_absent", "fixture_image_absent")) else 1
    except Exception as error:
        receipt_value["errorType"] = type(error).__name__
    write_json(output, "cleanup.json", receipt_value)
    return status


def run_fixture(evidence, output):
    name = evidence["cluster"]
    for key, env_name in (("run", "GITHUB_RUN_ID"), ("attempt", "GITHUB_RUN_ATTEMPT")):
        value = os.environ.get(env_name, "")
        if not re.fullmatch(r"[1-9][0-9]*", value):
            raise ValueError("CI run and attempt identity required")
        evidence[key] = value
    runtime_dir = Path(os.environ.get("RUNNER_TEMP", ""))
    chart = runtime_dir / "cilium.tgz"
    if not chart.is_file() or chart.is_symlink() or digest(chart.read_bytes()) != "505abff8385811df1a949776fc42e9f10697989a332acc86d6e8933efcb855ed":
        raise ValueError("approved Cilium chart required")
    with tempfile.TemporaryDirectory(prefix="kanz-agent-identity-") as directory:
        scratch = Path(directory)
        for sub in ("home", "tmp", "docker"):
            (scratch / sub).mkdir(mode=0o700)
        # Resolve public dependency/toolchain locations without inheriting Go
        # configuration or cloud credentials into any build or fixture process.
        discover_env = {"PATH": os.environ["PATH"], "HOME": os.environ.get("HOME", ""), "GOENV": "off", "GOTOOLCHAIN": "local"}
        locations = json.loads(Runner(scratch, discover_env).command(["go", "env", "-json", "GOMODCACHE", "GOROOT"], timeout=15))
        runner = Runner(scratch, command_environment(scratch, locations["GOMODCACHE"], locations["GOROOT"]))
        k = lambda *args, data=None: runner.command(["kubectl", "--kubeconfig", str(scratch / "kubeconfig"), "--context", "kind-" + name, *args], data=data)
        apply = lambda docs: k("apply", "-f", "-", data=yaml.safe_dump_all(docs))
        owner = None
        image = owned_image(name)

        def probe(label, namespace, sa, mode, address, recorder=False, expect_failure=False, network_control=False, merge=True):
            evidence["stage"] = "probe_" + label
            document = probe_job(label, namespace, sa, mode, address, image, recorder)
            if mode in {"network-denied", "network-positive"}:
                pod = document["spec"]["template"]
                pod["metadata"]["labels"]["agent-identity-network-client"] = "true"
                pod["spec"].pop("volumes")
                pod["spec"]["containers"][0].pop("volumeMounts")
            if network_control:
                document["spec"]["template"]["metadata"]["labels"] = {
                    "agent-identity-network-client": "true", "agent-identity-network-control": "true"}
            apply([document])
            deadline = time.monotonic() + 105
            while True:
                status = json.loads(k("-n", namespace, "get", "job/" + label, "-o", "json"))["status"]
                complete, failed = status.get("succeeded", 0) > 0, status.get("failed", 0) > 0
                if complete or failed:
                    break
                if time.monotonic() >= deadline:
                    raise RuntimeError("probe deadline exceeded")
                time.sleep(1)
            value = receipt(k("-n", namespace, "logs", "job/" + label))
            evidence["lastProbe"] = {"label": label, "completed": complete, "failed": failed,
                                     "observedCases": sorted(value["cases"])}
            if expect_failure:
                if not failed or value.get("passed") is not False or value.get("failedCase") != "order_submit_denied" or value.get("reason") != "publication_succeeded_or_explicit_denial_missing":
                    raise ValueError("grant mutation failed for an unrelated reason")
            else:
                if not complete or value.get("passed") is not True or any(v is not True for v in value["cases"].values()):
                    raise ValueError("probe did not completely pass")
                if merge:
                    evidence["cases"].update(value["cases"])
            return value

        try:
            if runner.command(["docker", "info", "--format", "{{.OSType}}"], timeout=30) != "linux":
                raise ValueError("existing native Linux Docker engine required")
            if name in runner.command(["kind", "get", "clusters"], timeout=30).splitlines():
                raise ValueError("owned cluster name already exists")
            evidence["source"] = runner.command(["git", "rev-parse", "HEAD"])
            evidence["tree"] = runner.command(["git", "rev-parse", "HEAD^{tree}"])
            if runner.command(["git", "status", "--porcelain"]):
                raise ValueError("proof requires committed clean source")
            evidence["sourceACLsha256"] = digest(canonical_tenancy()["data"]["tenants.conf"].encode())
            intent = {"schema": 1, **{key: evidence[key] for key in ("source", "tree", "run", "attempt")}, "cluster": name}
            validate_ownership(intent, evidence)
            write_json(output, "ownership.json", intent)
            owner = intent
            evidence["stage"] = "build_test_binaries"
            image_context = scratch / "image_context"
            image_context.mkdir()
            runner.command(["go", "build", "-trimpath", "-o", str(image_context / "identity_probe"), "./test/agent_boundary/cmd/identity_probe"], timeout=600, cwd=ROOT / "kanz")
            runner.command(["go", "test", "-p", "1", "-c", "-o", str(image_context / "copilot.test"), "./services/copilot/cmd/copilot"], timeout=600, cwd=ROOT / "kanz")
            evidence["binaries"] = {"identity_probe": digest((image_context / "identity_probe").read_bytes()), "copilot_recorder_test": digest((image_context / "copilot.test").read_bytes())}
            (image_context / "Dockerfile").write_text("FROM " + RUNTIME + "\nCOPY identity_probe /identity_probe\nCOPY copilot.test /copilot.test\nUSER 65532:65532\n", encoding="utf-8")
            runner.command(["docker", "image", "inspect", RUNTIME], timeout=30)  # pre-pulled outside the credential-free fixture
            if runner.command(["docker", "image", "ls", "-q", image], timeout=30):
                raise ValueError("owned fixture image name already exists")
            runner.command(["docker", "build", "--pull=false", "--network=none", "--label", "dev.kanz.agent-identity=" + name, "-t", image, str(image_context)], timeout=120)
            evidence["images"] = {"fixture": runner.command(["docker", "image", "inspect", "--format", "{{.Id}}", image]),
                                  "runtime": "sha256:" + RUNTIME.rsplit("sha256:", 1)[1],
                                  "nats": "sha256:" + FEED.PINS["nats:2.10-alpine"].rsplit("sha256:", 1)[1],
                                  "kind_node": "sha256:" + FEED.NODE.rsplit("sha256:", 1)[1]}
            evidence["stage"] = "create_private_cluster"
            FEED.command = runner.command
            FEED.create_enforcing_cluster(name, scratch)
            runner.command(["kind", "load", "docker-image", image, "--name", name], timeout=180)
            crds = []
            for path in sorted((ROOT / "kanz/infra/security/spire/crds").glob("*.yaml")):
                crds += FEED.manifests(str(path.relative_to(ROOT)))
            if not crds:
                raise ValueError("canonical SPIRE CRDs missing")
            apply(crds)
            for crd in crds:
                k("wait", "--for=condition=Established", "crd/" + crd["metadata"]["name"], "--timeout=90s")
            evidence["stage"] = "install_spire"
            apply(FEED.spire_objects())
            k("-n", "spire-system", "rollout", "status", "statefulset/spire-server", "--timeout=240s")
            k("-n", "spire-system", "rollout", "status", "daemonset/spire-agent", "--timeout=240s")
            apply(FEED.manifests("kanz/infra/security/spire/registration.yaml"))
            evidence["stage"] = "install_canonical_brokers"
            docs = broker_objects()
            apply(docs)
            mounted = json.loads(k("-n", "kanz-messaging", "get", "configmap/nats-tenants", "-o", "json"))["data"]["tenants.conf"]
            if digest(mounted.encode()) != evidence["sourceACLsha256"]:
                raise ValueError("mounted canonical tenancy bytes differ")
            for broker in ("nats", "nats-wrong"):
                k("-n", "kanz-messaging", "rollout", "status", "statefulset/" + broker, "--timeout=240s")
            for namespace, sa in (("kanz-services", "copilot"), ("kanz-services", "agent-identity-unregistered"), ("kanz-messaging", "nats-bootstrap")):
                apply([{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": sa, "namespace": namespace}}])
            evidence["stage"] = "isolate_probe_networks"
            isolation = probe_policies()
            validate_probe_policies(isolation)
            controls = network_controls(image)
            apply(isolation + [d for d in controls if d["kind"] == "NetworkPolicy"])
            observed = [json.loads(k("-n", ns, "get", "networkpolicy/agent-identity-probes", "-o", "json")) for ns in PROBE_NAMESPACES]
            validate_probe_policies(observed)
            apply([d for d in controls if d["kind"] == "Pod"])
            k("-n", "kanz-services", "wait", "--for=condition=Ready", "pod/agent-identity-inert", "--timeout=60s")
            inert = k("-n", "kanz-services", "get", "pod/agent-identity-inert", "-o", "jsonpath={.status.podIP}") + ":4222"
            network_results = {}
            for namespace, sa in (("kanz-services", "copilot"), ("kanz-messaging", "nats-bootstrap")):
                positive = probe("egress-control-" + namespace, namespace, sa, "network-positive", inert, network_control=True, merge=False)
                denied = probe("egress-denied-" + namespace, namespace, sa, "network-denied", inert, merge=False)
                if positive["cases"] != {"probe_egress_positive_control": True} or denied["cases"] != {"probe_egress_denied": True}:
                    raise ValueError("probe network control is incomplete")
                network_results[namespace] = True
            if set(network_results) != {"kanz-services", "kanz-messaging"}:
                raise ValueError("probe network enforcement omitted a namespace")
            evidence["cases"].update({"probe_egress_positive_control": True, "probe_egress_denied": True})
            addr = k("-n", "kanz-messaging", "get", "service/nats", "-o", "jsonpath={.spec.clusterIP}") + ":4222"
            wrong = k("-n", "kanz-messaging", "get", "service/nats-wrong", "-o", "jsonpath={.spec.clusterIP}") + ":4222"
            probe("bootstrap", "kanz-messaging", "nats-bootstrap", "bootstrap", addr)
            probe("recorders", "kanz-services", "copilot", "allowed", addr, recorder=True)
            probe("denied", "kanz-services", "copilot", "denied", addr)
            logs = k("-n", "kanz-messaging", "logs", "nats-0", "-c", "nats")
            for case, subject in {**PUBLICATIONS, **SUBSCRIPTIONS}.items():
                if not any(subject in line and "/sa/copilot" in line and re.search("violation|permission", line, re.I) for line in logs.splitlines()):
                    raise ValueError("explicit broker diagnostic missing for " + case)
            before = logs.lower().count("authentication error")
            probe("unregistered", "kanz-services", "agent-identity-unregistered", "unregistered", addr)
            if k("-n", "kanz-messaging", "logs", "nats-0", "-c", "nats").lower().count("authentication error") <= before:
                raise ValueError("new unregistered-user broker diagnostic missing")
            probe("wrong-positive", "kanz-services", "copilot", "wrong-control", wrong)
            probe("wrong-negative", "kanz-services", "copilot", "wrong-server", wrong, recorder=True)
            probe("retained", "kanz-messaging", "nats-bootstrap", "verify", addr)
            evidence["stage"] = "grant_mutation"
            mutation = widen_copy(canonical_tenancy())
            apply([mutation, mutant_broker(docs)])
            k("-n", "kanz-messaging", "rollout", "status", "statefulset/nats-mutant", "--timeout=240s")
            mutant_ip = k("-n", "kanz-messaging", "get", "pod/nats-mutant-0", "-o", "jsonpath={.status.podIP}") + ":4222"
            probe("mutation-bootstrap", "kanz-messaging", "nats-bootstrap", "bootstrap", mutant_ip)
            probe("mutation-denied", "kanz-services", "copilot", "denied", mutant_ip, expect_failure=True)
            probe("mutation-retained", "kanz-messaging", "nats-bootstrap", "mutation-verify", mutant_ip)
            evidence["mutationACLsha256"] = digest(mutation["data"]["tenants.conf"].encode())
            if digest(canonical_tenancy()["data"]["tenants.conf"].encode()) != evidence["sourceACLsha256"]:
                raise ValueError("source ACL changed during mutation")
            evidence["passed"] = True
        except Exception as error:
            evidence["passed"] = False
            evidence["failureStage"] = evidence["stage"]
            evidence["errorType"] = type(error).__name__
        finally:
            evidence["cleanup"] = {"cluster_absent": False, "owned_containers_absent": False,
                                   "fixture_image_absent": False, "private_kubeconfig_absent": False}
            try:
                if owner is not None:
                    # Re-read the durable intent, rather than trusting a
                    # changed local variable during failure recovery.
                    recorded = validate_ownership(read_ownership(output), evidence)
                    evidence["cleanup"].update(cleanup_owner(runner, recorded, scratch / "kubeconfig"))
            except Exception as error:
                evidence["cleanupErrorType"] = type(error).__name__
    evidence["cleanup"]["private_kubeconfig_absent"] = not (scratch / "kubeconfig").exists() and not scratch.exists()
    evidence["cases"]["owned_cleanup"] = len(evidence["cleanup"]) == 4 and all(evidence["cleanup"].values())
    evidence["passed"] = evidence["passed"] and set(evidence["cases"]) == REQUIRED_CASES and all(evidence["cases"].values())


def main():
    if sys.platform != "linux" or not hasattr(os, "O_NOFOLLOW"):
        raise RuntimeError("actual identity proof requires the Linux protected process/filesystem APIs")
    def interrupted(signum, _frame):
        signal.signal(signum, signal.SIG_IGN)
        raise RuntimeError("fixture interrupted; owned cleanup required")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    output = protected_directory(os.environ.get("AGENT_IDENTITY_EVIDENCE_DIR", ""))
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cleanup-owned", action="store_true")
    options = parser.parse_args()
    if options.cleanup_owned:
        return cleanup_recorded(output)
    name = "feed-proof-" + uuid.uuid4().hex[:12]  # approved helper's owned-name grammar
    evidence = {"schema": 1, "passed": False, "source": "", "tree": "", "run": "", "attempt": "",
                "cases": {}, "binaries": {}, "images": {}, "cleanup": {}, "cluster": name,
                "developmentOnly": True, "stage": "preflight"}
    try:
        run_fixture(evidence, output)
    except Exception as error:
        evidence["passed"] = False
        evidence["failureStage"] = evidence["stage"]
        evidence["errorType"] = type(error).__name__
    if evidence["passed"]:
        fields = {"schema", "passed", "source", "tree", "run", "attempt", "cases", "sourceACLsha256", "binaries", "images", "cleanup"}
        evidence = {key: evidence[key] for key in fields}
    write_evidence(output, evidence)
    return 0 if evidence["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())

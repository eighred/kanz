"""Qualify browser SSH onboarding and cold GHCR authentication on a fresh k3s worker."""

import base64
from contextlib import contextmanager
import copy
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time

from tools.feed_identity.proof import manifests, spire_objects
from . import estate, identity


ROOT = Path(__file__).resolve().parents[2]
NS = "kanz-services"
OPERATOR = "kanz-operator"
NEGATIVE = "kanz77-missing-secret"
SOCKET = "unix:///run/spiffe/spire-agent.sock"


def named(docs, kind, name):
    item, = [item for item in docs if item["kind"] == kind and item["metadata"]["name"] == name]
    return item


def env(mapping):
    return [{"name": key, "value": value} for key, value in mapping.items()]


def secret_ref(name, key):
    return {"secretKeyRef": {"name": name, "key": key}}


def secret(name, namespace, values):
    return {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": name, "namespace": namespace},
            "stringData": values}


def install_mesh(k, apply):
    for path in sorted((ROOT / "kanz/infra/security/spire/crds").glob("*.yaml")):
        for obj in manifests(str(path.relative_to(ROOT))):
            apply(obj)
            k("wait", "--for=condition=Established", "crd/" + obj["metadata"]["name"], "--timeout=90s")
    for obj in spire_objects():
        apply(obj)
    for target in ("statefulset/spire-server", "daemonset/spire-agent"):
        k("-n", "spire-system", "rollout", "status", target, "--timeout=300s")
    for obj in manifests("kanz/infra/security/spire/registration.yaml"):
        apply(obj)
    apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {
        "name": OPERATOR, "labels": {"kanz.internal/spiffe": "enabled"}}})


def pod(name, account, image, node, container):
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {
        "name": name, "namespace": NS, "labels": {"app": account}},
        "spec": {"serviceAccountName": account, "nodeSelector": {"kubernetes.io/hostname": node},
                 "automountServiceAccountToken": False, "restartPolicy": "Never",
                 "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                                     "seccompProfile": {"type": "RuntimeDefault"}},
                 "volumes": [{"name": "spiffe", "csi": {"driver": "csi.spiffe.io", "readOnly": True}}],
                 "containers": [{"name": account, "image": image,
                                 "securityContext": {"readOnlyRootFilesystem": True,
                                                     "allowPrivilegeEscalation": False,
                                                     "capabilities": {"drop": ["ALL"]}},
                                 "volumeMounts": [{"name": "spiffe", "mountPath": "/run/spiffe", "readOnly": True}],
                                 **container}]}}


def install_control(k, apply, image, control, control_ip, accounts, auth):
    # The dedicated agent join token is read only in this disposable server.
    token = estate.guest(control, "cat", "/var/lib/rancher/k3s/server/agent-token")
    apply(secret("operator-k3s-join", OPERATOR, {"server_url": f"https://{control_ip}:6443", "token": token}))
    for obj in manifests("kanz/infra/deploy/operator-deploy.yaml"):
        if obj["kind"] == "Deployment":
            container = obj["spec"]["template"]["spec"]["containers"][0]
            container.update(image=image, imagePullPolicy="Never", command=["/operator"])
            for entry in container["env"]:
                if entry["name"] == "OPERATOR_PROVISIONER_IMAGE":
                    entry["value"] = image
            container["env"].extend(env({"OPERATOR_PROVISIONER_IMAGE_PULL_POLICY": "Never"}))
        if obj["kind"] == "NetworkPolicy" and obj["metadata"]["name"] == "operator-egress":
            rule, = [r for r in obj["spec"]["egress"] if {"port": 6443, "protocol": "TCP"} in r["ports"]]
            rule["to"] = [{"ipBlock": {"cidr": control_ip + "/32"}}]
        apply(obj)
    k("-n", OPERATOR, "rollout", "status", "deployment/operator", "--timeout=180s")
    for sa in accounts:
        apply(sa)
    apply(secret("control-signing", NS, {"key": secrets.token_hex(32)}))
    gateway_env = {
        "API_GATEWAY_BFF_TLS_LISTEN": ":8443", "API_GATEWAY_SPIFFE_SOCKET": SOCKET,
        "API_GATEWAY_OPERATOR_ADDR": "operator.kanz-operator.svc:9090",
        "API_GATEWAY_OPERATOR_ROLE": "kanz-operator", "API_GATEWAY_REQUIRED_ROLE": "kanz-user",
        "API_GATEWAY_TRADE_ROLE": "kanz-trader", "API_GATEWAY_MFA_REQUIRED": "true",
        "API_GATEWAY_RISK_ENGINE_ADDR": "unused.invalid:9090",
        "API_GATEWAY_OIDC_ISSUER": auth["issuer"], "API_GATEWAY_OIDC_AUDIENCE": auth["audience"],
        "API_GATEWAY_OIDC_JWKS_URI": auth["jwks_uri"], "API_GATEWAY_REVOCATIONS_URI": auth["revocations_uri"],
    }
    srv = pod("onboarding-gateway", "api-gateway", image, control, {
        "command": ["/gateway"], "imagePullPolicy": "Never", "env": env(gateway_env) +
        [{"name": "API_GATEWAY_SIGNING_SECRET", "valueFrom": secret_ref("control-signing", "key")}],
        "readinessProbe": {"httpGet": {"path": "/readyz", "port": 8080}}})
    apply(srv)
    apply({"apiVersion": "v1", "kind": "Service", "metadata": {"name": "onboarding-gateway", "namespace": NS},
           "spec": {"selector": {"app": "api-gateway"}, "ports": [{"port": 8443, "targetPort": 8443}]}})
    bff_env = {"WEB_BFF_GATEWAY_URL": "https://onboarding-gateway.kanz-services.svc:8443",
               "WEB_BFF_GATEWAY_SPIFFE_SOCKET": SOCKET, "WEB_BFF_IDENTITY_URL": auth["identity_url"],
               "WEB_BFF_STATIC_DIR": "/static"}
    bff = pod("onboarding-bff", "web-bff", image, control, {
        "command": ["/web-bff"], "imagePullPolicy": "Never", "env": env(bff_env) +
        [{"name": "WEB_BFF_SIGNING_SECRET", "valueFrom": secret_ref("control-signing", "key")}],
        "readinessProbe": {"httpGet": {"path": "/readyz", "port": 8084}}})
    apply(bff)
    for item in (srv, bff):
        k("-n", NS, "wait", "--for=condition=Ready", "pod/" + item["metadata"]["name"], "--timeout=180s")
    return bff_env


@contextmanager
def edge(control, control_ip):
    processes = []
    try:
        for cmd in (["lxc", "exec", control, "--", "k3s", "kubectl", "-n", NS, "port-forward",
                     "--address", control_ip, "pod/onboarding-bff", "18084:8084"],
                    ["socat", "TCP4-LISTEN:18084,bind=127.0.0.1,reuseaddr,fork", f"TCP4:{control_ip}:18084"]):
            processes.append(subprocess.Popen(cmd, env=estate.child_env(), stdout=subprocess.DEVNULL,
                                              stderr=subprocess.DEVNULL))
        for _ in range(30):
            if any(p.poll() is not None for p in processes):
                raise RuntimeError("private BFF forwarding exited")
            try:
                estate.run(["curl", "--fail", "--silent", "http://127.0.0.1:18084/readyz"], timeout=3)
                break
            except RuntimeError:
                time.sleep(1)
        else:
            raise RuntimeError("loopback BFF forwarding unavailable")
        yield
    finally:
        for p in reversed(processes):
            p.terminate()
            p.wait(timeout=10)


def assert_cold(worker, image):
    inventories = json.loads(estate.guest(worker, "k3s", "crictl", "images", "-o", "json"))["images"]
    repo, digest = image.split("@", 1)
    for entry in inventories:
        refs = entry.get("repoTags", []) + entry.get("repoDigests", [])
        if entry["id"] == digest or any(ref.startswith((repo + "@", repo + ":")) for ref in refs):
            raise RuntimeError("pinned platform image was already present on the worker")


def diagnose_pull(k, namespace, name):
    obj = json.loads(k("-n", namespace, "get", "pod", name, "-o", "json"))
    uid = obj["metadata"]["uid"]
    events = json.loads(k("-n", namespace, "get", "events", "--field-selector",
                          "involvedObject.uid=" + uid, "-o", "json"))["items"]
    return obj, events


def registry_proof(k, apply, worker, platform, runtime_env):
    static_path, = [setting["value"] for setting in platform["env"] if setting["name"] == "WEB_BFF_STATIC_DIR"]
    projected = pod("ghcr-cold-pull", "web-bff", platform["image"], worker, {
        "imagePullPolicy": "Always", "env": env({**runtime_env, "WEB_BFF_STATIC_DIR": static_path}),
        "readinessProbe": platform["readinessProbe"]})
    neg = copy.deepcopy(projected)
    neg["metadata"]["namespace"] = NEGATIVE
    # The control requires image authentication, before the positive pod can warm the cache.
    assert_cold(worker, platform["image"])
    apply(neg)
    deadline = time.monotonic() + 140
    while time.monotonic() < deadline:
        obj, events = diagnose_pull(k, NEGATIVE, neg["metadata"]["name"])
        if any(e["reason"] == "Failed" and re.search(r"unauthorized|denied|401|403",
                                                      e.get("message", ""), re.I) for e in events):
            if obj["spec"]["nodeName"] != worker or obj["spec"].get("imagePullSecrets") != [{"name": "ghcr-pull"}]:
                raise RuntimeError("negative pull has wrong placement or ServiceAccount admission")
            break
        time.sleep(2)
    else:
        raise RuntimeError("no explicit unauthenticated GHCR pull failure on fresh worker")
    print(f"Pod {obj['metadata']['uid']} on {worker}: missing namespace pull secret, GHCR authorization denied.",
          flush=True)
    k("-n", NEGATIVE, "delete", "pod/" + neg["metadata"]["name"], "--wait=true")
    assert_cold(worker, platform["image"])
    apply(projected)
    name = projected["metadata"]["name"]
    k("-n", NS, "wait", "--for=condition=Ready", "pod/" + name, "--timeout=180s")
    obj, events = diagnose_pull(k, NS, name)
    status, = obj["status"]["containerStatuses"]
    if (obj["spec"]["nodeName"] != worker or obj["spec"]["serviceAccountName"] != "web-bff" or
            obj["spec"].get("imagePullSecrets") != [{"name": "ghcr-pull"}] or not status["ready"] or
            not any(e["reason"] == "Pulled" and platform["image"] in e.get("message", "") for e in events)):
        raise RuntimeError("positive pod lacks its placement, SA, authenticated pull event or Ready state")
    installed = json.loads(estate.guest(worker, "k3s", "crictl", "inspecti", platform["image"]))["status"]
    if platform["image"] not in installed["repoDigests"] or status["imageID"] not in {
            platform["image"], platform["image"].split("@")[-1], installed["id"]}:
        raise RuntimeError("container image ID is not bound to the deployed digest")
    print(json.dumps({"worker": worker, "podUID": obj["metadata"]["uid"], "serviceAccount": "web-bff",
                      "imagePullSecrets": ["ghcr-pull"], "image": platform["image"],
                      "imageID": status["imageID"], "events": sorted({e["reason"] for e in events}), "ready": True}))


def main():
    registry_token = os.environ.pop("GHCR_JOB_TOKEN")
    source = os.environ["GITHUB_SHA"]
    if not re.fullmatch("[0-9a-f]{40}", source) or Path("source-sha").read_text().strip() != source:
        raise RuntimeError("build artifact differs from the exact checked source")
    image = "docker.io/kanz-fixture/onboarding:" + source
    with estate.estate() as (control, worker):
        control_ip = estate.machine_address(control)
        worker_ip = estate.machine_address(worker)
        host_key, key = estate.prepare_worker(worker)
        print("Starting the encrypted, disposable k3s control plane.", flush=True)
        estate.start_server(control)
        estate.run(["lxc", "file", "push", "onboarding.tar", control + "/var/tmp/onboarding.tar"], timeout=240)
        estate.guest(control, "k3s", "ctr", "images", "import", "/var/tmp/onboarding.tar", timeout=240)
        estate.guest(control, "rm", "/var/tmp/onboarding.tar")
        def k(*args, data=None):
            try:
                return estate.guest(control, "k3s", "kubectl", *args, data=data, timeout=390)
            except RuntimeError:
                # Only Kubernetes command/namespace/resource, never exec arguments or stdin.
                public = args[:args.index("--")] if "--" in args else args
                raise RuntimeError("kubectl failed: " + " ".join(public[:5])) from None
        def apply(obj):
            # A Secret must not acquire a cleartext last-applied annotation.
            return k("create" if obj["kind"] == "Secret" else "apply", "-f", "-", data=json.dumps(obj))
        print("Installing the per-estate SPIRE trust domain.", flush=True)
        install_mesh(k, apply)
        web = manifests("kanz/infra/deploy/web-bff-deploy.yaml")
        gateway = manifests("kanz/infra/deploy/api-gateway-deploy.yaml")
        account = named(web, "ServiceAccount", "web-bff")
        if account["imagePullSecrets"] != [{"name": "ghcr-pull"}]:
            raise RuntimeError("canonical platform ServiceAccount has unexpected registry credentials")
        auth_pair = base64.b64encode(f"{os.environ['GITHUB_ACTOR']}:{registry_token}".encode()).decode()
        docker_config = json.dumps({"auths": {"ghcr.io": {"auth": auth_pair}}})
        auth_secret = secret("ghcr-pull", NS, {".dockerconfigjson": docker_config})
        auth_secret["type"] = "kubernetes.io/dockerconfigjson"
        apply(auth_secret)
        del registry_token, auth_pair, docker_config, auth_secret
        apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": NEGATIVE}})
        negative_sa = copy.deepcopy(account)
        negative_sa["metadata"]["namespace"] = NEGATIVE
        apply(negative_sa)
        print("Installing restricted PostgreSQL and the disposable identity issuer.", flush=True)
        auth = identity.install(k, apply, image, control)
        runtime_env = install_control(k, apply, image, control, control_ip,
                                      [account, named(gateway, "ServiceAccount", "api-gateway")], auth)
        nodes = json.loads(k("get", "nodes", "-o", "json"))["items"]
        if [n["metadata"]["name"] for n in nodes] != [control]:
            raise RuntimeError("expected exactly one control-plane node before browser submission")
        print("Authenticating the browser and submitting the fresh SSH worker.", flush=True)
        with edge(control, control_ip):
            browser_env = {k: os.environ[k] for k in ("PATH", "HOME", "TEST_MFA_PLAYWRIGHT")}
            receipt = estate.run(["node", str(ROOT / "tools/operator_onboarding/browser.cjs")],
                                 environment=browser_env,
                                 data=json.dumps({"base": auth["origin"], "subject": auth["subject"],
                                                  "invite": auth["invite"], "hostname": worker, "ip": worker_ip,
                                                  "sshUser": "ubuntu", "sshHostKey": host_key,
                                                  "sshPrivateKey": key}), timeout=180)
        del key, auth
        if not re.fullmatch("[a-z0-9-]{1,63}", receipt):
            raise RuntimeError("browser returned no usable provisioning receipt")
        k("-n", OPERATOR, "wait", "--for=condition=complete", "job/" + receipt, "--timeout=360s")
        k("wait", "--for=condition=Ready", "node/" + worker, "--timeout=120s")
        estate.guest(worker, "rm", "/home/ubuntu/.ssh/authorized_keys")
        k("-n", "spire-system", "rollout", "status", "daemonset/spire-agent", "--timeout=180s")
        n = json.loads(k("get", "node", worker, "-o", "json"))
        if (n["metadata"]["uid"] == nodes[0]["metadata"]["uid"] or
                "node-role.kubernetes.io/control-plane" in n["metadata"]["labels"] or
                worker_ip not in [a["address"] for a in n["status"]["addresses"] if a["type"] == "InternalIP"] or
                n["status"]["nodeInfo"]["kubeletVersion"] != estate.K3S_VERSION):
            raise RuntimeError("worker identity/version does not match the pristine SSH guest")
        job_pods = json.loads(k("-n", OPERATOR, "get", "pods", "-l", "job-name=" + receipt, "-o", "json"))["items"]
        if not job_pods or any(p["spec"]["nodeName"] != control for p in job_pods):
            raise RuntimeError("credentialed provisioner ran outside the control plane")
        print(f"Source {source}: browser run {receipt}; worker {worker} UID {n['metadata']['uid']} Ready; "
              f"credentialed install on {control}; prior node count 1.", flush=True)
        platform, = [c for c in named(web, "Deployment", "web-bff")["spec"]["template"]["spec"]["containers"]
                     if c["name"] == "web-bff"]
        if not re.fullmatch(r"ghcr\.io/eighred/web-bff@sha256:[a-f0-9]{64}", platform["image"]):
            raise RuntimeError("platform deployment has no immutable GHCR image")
        print("Checking the uncached negative and authorized pull on the new worker.", flush=True)
        registry_proof(k, apply, worker, platform, runtime_env)


if __name__ == "__main__":
    main()

"""Exercise the operator's authenticated TestConnection RPC during a disposable k3s drain."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

import yaml


ROOT = Path(__file__).resolve().parents[1]
K3S = "rancher/k3s:v1.36.4-k3s1@sha256:8b3560e0a99b2f22832b2c963550952ed69830320d4ee712693c52026ada7b89"
OPERATOR_NS = "kanz-operator"


def run(args, *, stdin=None, cwd=ROOT, env=None, timeout=300):
    result = subprocess.run(args, cwd=cwd, env=env, input=stdin, text=True,
                            encoding="utf-8", errors="replace", stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{args[:5]} failed ({result.returncode}): {result.stderr}\n{result.stdout}")
    return result.stdout


def compile_images(directory, cluster):
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
    builds = [
        ("operator", [("operator", ["build"], "./services/operator/cmd/operator"),
                      ("placement.test", ["test", "-p", "1", "-c", "-tags", "operator_placement"],
                       "./services/operator/cmd/operator")]),
        ("provisioner", [("provisioner", ["build"], "./cmd/kanz_provisioner")]),
    ]
    for entry, binaries in builds:
        path = directory / entry
        path.mkdir()
        dockerfile = ["FROM scratch"]
        for name, flags, package in binaries:
            run(["go", *flags, "-o", str(path / name), package], cwd=ROOT / "kanz", env=env, timeout=900)
            dockerfile.append(f"COPY --chmod=0555 {name} /{name}")
        dockerfile.extend(["USER 65532:65532", f'ENTRYPOINT ["/{entry}"]'])
        (path / "Dockerfile").write_text("\n".join(dockerfile) + "\n", encoding="ascii")
        tag = f"kanz-placement-{cluster}/{entry}:fixture"
        run(["docker", "build", "-t", tag, str(path)])
        yield tag


def exercise(kubectl, operator_image, provisioner_image, server_names, worker):
    def query(*args):
        return json.loads(kubectl("get", *args, "-o", "json"))

    def apply(resource):
        return kubectl("apply", "-f", "-", stdin=json.dumps(resource))

    for name in (OPERATOR_NS, "kanz-services"):
        kubectl("create", "namespace", name)

    def target(name, node):
        spec = {
            "automountServiceAccountToken": False,
            "nodeSelector": {"kubernetes.io/hostname": node},
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                                "seccompProfile": {"type": "RuntimeDefault"},
                                "sysctls": [{"name": "net.ipv4.ip_unprivileged_port_start", "value": "0"}]},
            "containers": [{"name": "target", "image": operator_image, "imagePullPolicy": "Never",
                            "command": ["/placement.test", "-test.run=^TestPlacementTarget$", "-test.timeout=20m"],
                            "readinessProbe": {"tcpSocket": {"port": 22}, "periodSeconds": 1},
                            "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                                "capabilities": {"drop": ["ALL"]}}}],
        }
        labels = {"app": name}
        apply({"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": name},
               "spec": {"selector": {"matchLabels": labels},
                        "template": {"metadata": {"labels": labels}, "spec": spec}}})
        kubectl("rollout", "status", "deployment/" + name, "--timeout=120s")
        return query("pods", "-l", "app=" + name)["items"][0]

    endpoint = target("probe-target", worker)["status"]["podIP"]
    mesh = query("endpointslices", "-n", "default", "-l", "kubernetes.io/service-name=kubernetes")
    api_ips = sorted({address for item in mesh["items"] for ep in item["endpoints"] for address in ep["addresses"]})
    if len(api_ips) != len(server_names):
        raise RuntimeError("expected one private API address per control-plane node")

    manifest = ROOT / "kanz/infra/deploy/operator-deploy.yaml"
    for obj in yaml.safe_load_all(manifest.read_text(encoding="utf-8")):
        if obj["kind"] == "Deployment":
            spec = obj["spec"]["template"]["spec"]
            container = spec["containers"][0]
            container.update(image=operator_image, imagePullPolicy="Never", command=["/placement.test"],
                             args=["-test.run=^TestWorkerProbeDuringServerDrain$", "-test.v", "-test.timeout=6m"])
            # testidentity supplies a disposable Workload API socket in this scratch volume.
            spec["volumes"] = [{"name": "fixture-tmp", "emptyDir": {}}]
            container["volumeMounts"] = [{"name": "fixture-tmp", "mountPath": "/tmp"}]
            image = next(e for e in container["env"] if e["name"] == "OPERATOR_PROVISIONER_IMAGE")
            image["value"] = provisioner_image
            container["env"].extend([{"name": name, "value": value} for name, value in (
                ("OPERATOR_PROVISIONER_IMAGE_PULL_POLICY", "Never"),
                ("FIXTURE_TARGET", endpoint), ("FIXTURE_WORKER", worker))])
        if obj["kind"] == "NetworkPolicy" and obj["metadata"]["name"] == "operator-egress":
            rule = next(r for r in obj["spec"]["egress"] if {"port": 6443, "protocol": "TCP"} in r["ports"])
            rule["to"] = [{"ipBlock": {"cidr": address + "/32"}} for address in api_ips]
        apply(obj)
    kubectl("rollout", "status", "deployment/operator", "-n", OPERATOR_NS, "--timeout=120s")
    account = query("serviceaccount", "kanz-node-provisioner", "-n", OPERATOR_NS)
    if account.get("automountServiceAccountToken") is not False:
        raise RuntimeError("the probe service account must disable credential automount")
    operator = query("pods", "-n", OPERATOR_NS, "-l", "app=operator")["items"][0]
    retained = operator["spec"]["nodeName"]
    if retained not in server_names:
        raise RuntimeError("operator must be pinned to a control-plane node")
    drained, = set(server_names) - {retained}
    witness = target("eviction-witness", drained)["metadata"]
    budget = query("pdb", "operator", "-n", OPERATOR_NS)
    if budget["spec"]["minAvailable"] != 1 or budget["status"]["disruptionsAllowed"] != 0:
        raise RuntimeError("expected the production singleton disruption budget")
    report = kubectl("drain", drained, "--ignore-daemonsets", "--timeout=150s")
    if f"pod/{witness['name']} evicted" not in report or f"node/{drained} drained" not in report:
        raise RuntimeError("missing eviction of the Ready witness: " + report)
    if any(p["metadata"]["uid"] == witness["uid"] for p in query("pods")["items"]):
        raise RuntimeError("drained witness still exists")
    print(report.strip(), flush=True)
    # This does not evict the operator. It leaves the worker as the only
    # possible placement and signals that the other server completed its drain.
    kubectl("cordon", retained)
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        logs = kubectl("logs", "-n", OPERATOR_NS, operator["metadata"]["name"])
        if "--- PASS: TestWorkerProbeDuringServerDrain" in logs and "TestConnection reached" in logs:
            print(next(line.strip() for line in logs.splitlines() if "TestConnection reached" in line), flush=True)
            return
        if "--- FAIL:" in logs:
            raise RuntimeError(logs)
        time.sleep(1)
    raise RuntimeError("timed out waiting for the operator's authenticated probe: " + logs)


def main():
    cluster = "kz76-" + uuid.uuid4().hex[:10]
    k3d = os.environ.get("K3D", "k3d")
    server = "k3d-" + cluster + "-server-0"
    images = []
    with tempfile.TemporaryDirectory(prefix="kanz-placement-") as build:
        cluster_env = dict(os.environ, KUBECONFIG=str(Path(build) / "kubeconfig"))
        def cluster_tool(*args):
            return run([k3d, *args], env=cluster_env)

        try:
            for image in compile_images(Path(build), cluster):
                images.append(image)
            cluster_tool("cluster", "create", cluster, "--servers", "2", "--agents", "1",
                 "--servers-memory", "1600m", "--agents-memory", "1100m", "--image", K3S,
                 "--api-port", "127.0.0.1:0", "--no-lb", "--kubeconfig-update-default=false",
                 "--kubeconfig-switch-context=false", "--k3s-arg",
                 "--disable=traefik,servicelb,metrics-server,local-storage@server:*", "--timeout", "180s")
            try:
                cluster_tool("image", "import", "--cluster", cluster, *images)
                # Never read a host kubeconfig or accept an existing cluster.
                def kubectl(*args, stdin=None):
                    return run(["docker", "exec", "-i", server, "kubectl", *args], stdin=stdin)

                worker = f"k3d-{cluster}-agent-0"
                servers = [server, f"k3d-{cluster}-server-1"]
                kubectl("wait", "--for=condition=Ready", "node", *servers, worker, "--timeout=120s")
                exercise(kubectl, *images, servers, worker)
            finally:
                cluster_tool("cluster", "delete", cluster)
        finally:
            if images:
                run(["docker", "image", "rm", *images])


if __name__ == "__main__":
    main()

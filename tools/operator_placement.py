"""Exercise the operator's authenticated TestConnection RPC during a disposable k3s drain."""

import json
import os
from contextlib import contextmanager
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


def cluster_resources(cluster):
    """Only exact k3d names in this invocation's private namespace."""
    prefix = f"k3d-{cluster}"
    names = {"container": {f"{prefix}-{s}" for s in ("server-0", "server-1", "agent-0", "tools")},
             "network": {prefix}, "volume": {prefix + "-images"}}
    found = {}
    for kind, expected in names.items():
        args = ["docker", kind, "ls"]
        if kind == "container":
            args.append("--all")
        field = "Names" if kind == "container" else "Name"
        listed = run([*args, "--filter", f"name={prefix}", "--format", "{{." + field + "}}"])
        found[kind] = expected.intersection(listed.splitlines())
    return found


def remove_cluster(cluster, cluster_tool):
    resources = cluster_resources(cluster)
    if not any(resources.values()):
        return  # k3d has already rolled back a failed create.
    identifiers = {}
    for kind, names in resources.items():
        for name in names:
            # The k3d network carries only app=k3d. Its exact name was checked
            # absent before creation. Node containers and the image volume
            # additionally carry the cluster label.
            labels_path = ".Config.Labels" if kind == "container" else ".Labels"
            labels = json.loads(run(["docker", kind, "inspect", name, "--format", "{{json " + labels_path + "}}"]))
            if (not labels or labels.get("app") != "k3d" or
                    (kind != "network" and labels.get("k3d.cluster") != cluster)):
                raise RuntimeError(f"refusing to delete a foreign {kind}: {name}")
            if kind != "volume":
                identifiers[name] = run(["docker", kind, "inspect", name, "--format", "{{.Id}}"]).strip()
    if resources["container"]:
        try:
            cluster_tool("cluster", "delete", cluster)
        except (RuntimeError, subprocess.TimeoutExpired) as error:
            print(f"k3d cleanup incomplete: {error}", flush=True)
    # A failure before the first node exists can leave just k3d's network
    # and image volume. k3d cannot discover a cluster from those alone.
    remaining = cluster_resources(cluster)
    for kind in ("container", "network", "volume"):
        for name in remaining[kind]:
            if name not in resources[kind]:
                raise RuntimeError(f"unexpected {kind} appeared during cleanup: {name}")
            args = ["docker", kind, "rm"]
            if kind == "container":
                args.extend(["--force", "--volumes"])
            run([*args, identifiers.get(name, name)])
    if any(cluster_resources(cluster).values()):
        raise RuntimeError("owned k3d resources remain after cleanup")


@contextmanager
def fresh_cluster(cluster, cluster_tool):
    if any(cluster_resources(cluster).values()):
        raise RuntimeError(f"refusing to use a pre-existing cluster name: {cluster}")
    try:
        yield
    finally:
        # Python exception chaining preserves the create/exercise error as
        # well as a cleanup failure. The lease starts before k3d creates anything.
        remove_cluster(cluster, cluster_tool)


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
        if run(["docker", "image", "ls", "--quiet", tag]).strip():
            raise RuntimeError(f"refusing to replace pre-existing image: {tag}")
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
    remaining = query("pods", "-A", "--field-selector", f"spec.nodeName={drained}")["items"]
    for pod in remaining:
        metadata = pod["metadata"]
        if not (metadata.get("annotations", {}).get("kubernetes.io/config.mirror") or
                any(owner["kind"] == "DaemonSet" for owner in metadata.get("ownerReferences", []))):
            raise RuntimeError(f"unexpected workload left on drained server: {metadata['name']}")
    inventory = [p["metadata"]["namespace"] + "/" + p["metadata"]["name"] for p in remaining]
    print(f"Post-drain inventory on {drained}: {inventory}", flush=True)
    saved = query("pod", operator["metadata"]["name"], "-n", OPERATOR_NS)
    if (saved["metadata"]["uid"] != operator["metadata"]["uid"] or saved["spec"]["nodeName"] != retained or
            not any(c["type"] == "Ready" and c["status"] == "True" for c in saved["status"]["conditions"])):
        raise RuntimeError("the same operator must remain Ready on the retained control-plane node")
    print(f"Operator {saved['metadata']['name']} remained Ready on {retained} throughout the drain; "
          "PDB minAvailable=1, disruptionsAllowed=0", flush=True)
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
            with fresh_cluster(cluster, cluster_tool):
                cluster_tool("cluster", "create", cluster, "--servers", "2", "--agents", "1",
                             "--servers-memory", "1600m", "--agents-memory", "1100m", "--image", K3S,
                             "--api-port", "127.0.0.1:0", "--no-lb", "--kubeconfig-update-default=false",
                             "--kubeconfig-switch-context=false", "--k3s-arg",
                             "--disable=traefik,servicelb,metrics-server,local-storage@server:*", "--timeout", "180s")
                cluster_tool("image", "import", "--cluster", cluster, *images)
                # Never read a host kubeconfig or accept an existing cluster.
                def kubectl(*args, stdin=None):
                    return run(["docker", "exec", "-i", server, "kubectl", *args], stdin=stdin)

                worker = f"k3d-{cluster}-agent-0"
                servers = [server, f"k3d-{cluster}-server-1"]
                kubectl("wait", "--for=condition=Ready", "node", *servers, worker, "--timeout=120s")
                exercise(kubectl, *images, servers, worker)
        finally:
            if images:
                run(["docker", "image", "rm", *images])
                if any(run(["docker", "image", "ls", "--quiet", tag]).strip() for tag in images):
                    raise RuntimeError("fixture image tags remain after cleanup")


if __name__ == "__main__":
    main()

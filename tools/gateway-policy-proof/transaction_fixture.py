"""Real Kubernetes transaction oracle in a fresh owned kind cluster only."""
import copy
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
from bounded_process import run as bounded_run

CASES = ["response_loss_" + str(at) for at in range(1, 9)] + [
    "dry_run_no_write", "four_policy_success", "concurrent_change_refuses_rollback",
    "deployment_uid_lineage", "controller_adoption_refusal", "never_ready_endpoint_nonparticipation", "publish_unready_refusal", "actual_tcp_policy_controls", "probe_uid_cleanup"]


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

        def k(*args, value=None):
            return run(["kubectl", "--kubeconfig", str(scratch / "kubeconfig"), "--context", "kind-" + cluster, *args],
                       None if value is None else json.dumps(value))

        try:
            report["source"] = run(["git", "rev-parse", "HEAD"]).strip()
            report["tree"] = run(["git", "rev-parse", "HEAD^{tree}"]).strip()
            report["runID"] = os.environ["GITHUB_RUN_ID"]
            report["runAttempt"] = os.environ["GITHUB_RUN_ATTEMPT"]
            rendered, _, report["renderSHA256"] = proof.render(run)
            desired = [p for p in rendered if p["metadata"]["name"] in tx.NAMES]
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
            # Exercise the exact live-probe constructor on the disposable API.
            # No live target or domain workload is contacted by this fixture.
            identity = public_identity()
            run(["docker", "pull", rollout_probe.IMAGE])
            run(["kind", "load", "docker-image", rollout_probe.IMAGE, "--name", cluster])
            k("create", "namespace", "kanz-observability")
            k("create", "-f", "-", value={"apiVersion": "v1", "kind": "ConfigMap",
              "metadata": {"name": "scripts", "namespace": tx.NAMESPACE},
              "data": {"listener.py": (proof.HERE / "listener.py").read_text()}})
            endpoints = {}
            for name, ports in {**proof.TARGETS, "api-gateway": (8080,), "web-bff": (8080,)}.items():
                template = proof.pod("target-" + name, tx.NAMESPACE, {"app": name}, ports)
                template["spec"]["restartPolicy"] = "Always"
                template["spec"].pop("activeDeadlineSeconds", None)
                deployment = {"apiVersion": "apps/v1", "kind": "Deployment",
                    "metadata": {"name": name, "namespace": tx.NAMESPACE},
                    "spec": {"replicas": 1, "selector": {"matchLabels": {"app": name}},
                             "template": {"metadata": {"labels": {"app": name}}, "spec": template["spec"]}}}
                k("create", "-f", "-", value=deployment)
                k("-n", tx.NAMESPACE, "rollout", "status", "deployment/" + name, "--timeout=90s")
                pods = json.loads(k("-n", tx.NAMESPACE, "get", "pods", "-l", "app=" + name, "-o", "json"))["items"]
                if len(pods) != 1:
                    raise AssertionError("fixture deployment is not a singleton")
                actual = pods[0]
                expected = {name: {"labels": {"app": name}, "selector": deployment["spec"]["selector"],
                            "images": {key: {c["name"]: c["image"] for c in template["spec"].get(key, [])}
                                       for key in ("containers", "initContainers")}}}
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
            probes = Probes(k, lambda image_id: json.loads(run(["docker", "exec", cluster + "-control-plane", "crictl", "inspecti", image_id])), identity, uuid.uuid4().hex)
            try:
                probes.create()
                end = time.monotonic() + 30
                while True:
                    slices = probes.endpoints()
                    matching = [e for item in slices for e in item.get("endpoints", [])
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
                probes.close()
            report["cases"].append("probe_uid_cleanup")
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

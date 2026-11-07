"""Fixed Tokyo policy entrypoint."""
import argparse
import copy
import json
import os
from pathlib import Path
import re
import signal
import sys
import tempfile
import time
import uuid

from bounded_process import run as bounded_run
from policy_client import KubernetesClient
import rollout_probe as probe
import transaction as tx

CLUSTER_UID = "86ea9839-5806-461d-b2d1-afd1ff5d87ff"
APPS = {"kanz-services": ("accounting", "audit", "compliance", "api-gateway", "web-bff"),
        "kanz-messaging": ("nats", "redis")}
PORTS = {"accounting": (8101, 8080), "audit": (8102, 8083), "compliance": (8095, 8091)}
TCP = """import socket,sys,time,json
values=[]
for _ in range(3):
 try:
  s=socket.create_connection((sys.argv[1],int(sys.argv[2])),timeout=1); s.close(); values.append('connected')
 except TimeoutError: values.append('timeout')
 except OSError: values.append('error')
 time.sleep(.2)
print(json.dumps(values))
"""


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


class Probes:
    def __init__(self, k, inspect, identity, nonce, save=lambda value: None):
        self.k, self.inspect, self.identity, self.nonce = k, inspect, identity, nonce
        self.owned = []
        self.addresses = []
        self.peers = {}
        self.namespace = "gateway-policy-" + nonce
        self.ns_uid = None
        self.namespace_uid_custody = None
        self.save = save

    def record(self, phase):
        self.save({"format": "kanz-gateway-policy-probes-v1", "nonce": self.nonce, "phase": phase,
                   "namespace": self.namespace, "namespaceUID": self.namespace_uid_custody,
                   "pods": [{key: entry.get(key) for key in ("name", "namespace", "uid", "address", "deleted")} for entry in self.owned]})

    def services(self):
        return json.loads(self.k("get", "services", "-A", "-o", "json"))["items"]

    def endpoints(self):
        return json.loads(self.k("get", "endpointslices", "-A", "-o", "json"))["items"]

    def controllers(self):
        return json.loads(self.k("get", "replicasets,replicationcontrollers,statefulsets,daemonsets,jobs", "-A", "-o", "json"))["items"]

    def create(self):
        before = json.loads(self.k("get", "namespace", self.namespace, "--ignore-not-found", "-o", "json") or "null")
        if before is not None:
            raise tx.Refusal("probe namespace collision")
        ns = {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": self.namespace, "labels": {probe.OWNER: self.nonce}}}
        self.record("namespace_create_intent")
        try:
            self.k("create", "-f", "-", value=ns)
        finally:
            observed = json.loads(self.k("get", "namespace", self.namespace, "--ignore-not-found", "-o", "json") or "null")
            if observed and observed["metadata"].get("labels", {}).get(probe.OWNER) == self.nonce:
                self.ns_uid = observed["metadata"]["uid"]
                self.namespace_uid_custody = self.ns_uid
            self.record("namespace_create_observed")
        if self.ns_uid is None:
            raise tx.Refusal("probe namespace creation unknown")
        for peer, namespace, app in (("gateway", tx.NAMESPACE, "api-gateway"), ("bff", tx.NAMESPACE, "web-bff"),
                                     ("untrusted", tx.NAMESPACE, "policy-untrusted"),
                                     ("observer", "kanz-observability", "policy-observer"),
                                     ("outsider", self.namespace, "api-gateway")):
            desired = probe.caller_pod(namespace, app, self.nonce)
            probe.service_boundary(self.services(), namespace, desired["metadata"]["labels"])
            probe.controller_boundary(self.controllers(), namespace, desired["metadata"]["labels"])
            name = desired["metadata"]["name"]
            if self.k("-n", namespace, "get", "pod", name, "--ignore-not-found", "-o", "name").strip():
                raise tx.Refusal("probe pod collision")
            entry = {"namespace": namespace, "name": name, "uid": None, "desired": desired}
            self.owned.append(entry)
            self.record("pod_create_intent")
            try:
                self.k("create", "-f", "-", value=desired)
            finally:
                actual = json.loads(self.k("-n", namespace, "get", "pod", name, "--ignore-not-found", "-o", "json") or "null")
                if actual and actual["metadata"].get("labels", {}).get(probe.OWNER) == self.nonce:
                    entry["uid"] = actual["metadata"]["uid"]
                self.record("pod_create_observed")
            end = time.monotonic() + 90
            while True:
                actual = json.loads(self.k("-n", namespace, "get", "pod", name, "-o", "json"))
                if actual.get("status", {}).get("phase") == "Running":
                    break
                if time.monotonic() >= end:
                    raise tx.Refusal("probe did not become Running")
                time.sleep(1)
            image_id = actual["status"]["containerStatuses"][0]["imageID"]
            entry["verifiedImageID"] = probe.verify_cri(image_id, self.inspect(image_id), self.identity)
            address = probe.running_unready(actual, desired, entry["verifiedImageID"])
            entry["address"] = address
            self.addresses.append(address)
            self.record("pod_running_unready_observed")
            self.peers[peer] = entry
        self.check()

    def check(self):
        services = self.services()
        controllers = self.controllers()
        for entry in self.owned:
            obj = json.loads(self.k("-n", entry["namespace"], "get", "pod", entry["name"], "-o", "json"))
            if obj["metadata"]["uid"] != entry["uid"]:
                raise tx.Refusal("probe pod was replaced")
            probe.service_boundary(services, entry["namespace"], entry["desired"]["metadata"]["labels"])
            probe.controller_boundary(controllers, entry["namespace"], entry["desired"]["metadata"]["labels"])
            if probe.running_unready(obj, entry["desired"], entry["verifiedImageID"]) != entry["address"]:
                raise tx.Refusal("probe address changed")
        probe.endpoint_boundary(self.endpoints(), self.addresses)

    def tcp(self, peer, address, port, expected):
        self.check()
        entry = self.peers[peer]
        result = json.loads(self.k("-n", entry["namespace"], "exec", entry["name"], "--", "python3", "-I", "-B", "-c", TCP, address, str(port)))
        if result != [expected] * 3:
            raise tx.Refusal("network-only probe did not establish expected connectivity")

    def verify(self, workloads, isolated):
        for app, (api, ops) in PORTS.items():
            address = workloads[(tx.NAMESPACE, app)]["ip"]
            self.tcp("gateway", address, api, "connected")
            self.tcp("observer", address, ops, "connected")
            if isolated:
                for peer in ("untrusted", "bff", "observer", "outsider"):
                    self.tcp("gateway", address, api, "connected")
                    self.tcp(peer, address, api, "timeout")
                    self.tcp("gateway", address, api, "connected")
        self.tcp("bff", workloads[(tx.NAMESPACE, "api-gateway")]["ip"], 8080, "connected")

    def close(self):
        self.record("probe_cleanup_intent")
        for entry in reversed(self.owned):
            actual = json.loads(self.k("-n", entry["namespace"], "get", "pod", entry["name"], "--ignore-not-found", "-o", "json") or "null")
            if actual is None:
                entry["deleted"] = True
                self.record("pod_absence_observed")
                continue
            if actual["metadata"].get("labels", {}).get(probe.OWNER) != self.nonce or actual["metadata"]["uid"] != entry["uid"]:
                raise tx.Refusal("probe cleanup ownership uncertain")
            self.k("delete", "--raw", "/api/v1/namespaces/" + entry["namespace"] + "/pods/" + entry["name"], "-f", "-",
                   value={"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {"uid": entry["uid"], "resourceVersion": actual["metadata"]["resourceVersion"]}, "gracePeriodSeconds": 0})
            self.k("-n", entry["namespace"], "wait", "--for=delete", "pod/" + entry["name"], "--timeout=60s")
            entry["deleted"] = True
            self.record("pod_absence_observed")
        if self.ns_uid:
            actual = json.loads(self.k("get", "namespace", self.namespace, "-o", "json"))
            if actual["metadata"]["uid"] != self.ns_uid or actual["metadata"].get("labels", {}).get(probe.OWNER) != self.nonce:
                raise tx.Refusal("probe namespace replaced")
            self.k("delete", "--raw", "/api/v1/namespaces/" + self.namespace, "-f", "-", value={"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {"uid": self.ns_uid, "resourceVersion": actual["metadata"]["resourceVersion"]}})
            self.k("wait", "--for=delete", "namespace/" + self.namespace, "--timeout=60s")
            self.ns_uid = None
        end = time.monotonic() + 30
        while True:
            try:
                probe.endpoint_boundary(self.endpoints(), self.addresses, require_absent=True)
                break
            except ValueError:
                if time.monotonic() >= end:
                    raise
                time.sleep(1)
        self.record("probe_cleanup_verified")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--tokyo", action="store_true", required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    if sys.platform != "linux" or os.geteuid() != 0 or not re.fullmatch(r"[0-9a-f]{40}", args.source):
        raise tx.Refusal("fixed Tokyo root execution required")
    def interrupted(signum, frame):
        raise tx.Refusal("rollout interrupted; retain private receipts")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raw = sys.stdin.buffer.read((4 << 20) + 1)
    if len(raw) > 4 << 20:
        raise tx.Refusal("rollout input exceeds bound")
    payload = json.loads(raw)
    if set(payload) != {"source", "policies", "identity", "workloads"} or payload["source"] != args.source:
        raise tx.Refusal("rollout source binding differs")
    desired = [p for p in payload["policies"] if p["metadata"]["name"] in tx.NAMES]
    nonce = uuid.uuid4().hex
    # Dry-run owns scratch only; apply owns durable receipts.
    scratch = None
    receipts = None
    probes = None
    receipt_path = None
    try:
        if args.apply:
            directory = tempfile.mkdtemp(prefix="kanz-gateway-policy-", dir="/var/tmp")
            receipt_path = directory
            receipts = tx.Receipts(directory)
            record = receipts
        else:
            scratch = tempfile.TemporaryDirectory(prefix="kanz-gateway-dry-run-", dir="/var/tmp")
            directory = scratch.name
            record = lambda value: None
        environment = {"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                       "HOME": directory, "TMPDIR": directory, "LANG": "C.UTF-8"}
        def command(argv, value=None):
            return bounded_run(argv, data=None if value is None else json.dumps(value),
                               env=environment, cwd=directory, timeout=100)
        def k(*argv, value=None):
            return command(["/usr/local/bin/k3s", "kubectl", *argv], value)
        client = KubernetesClient(k, CLUSTER_UID)
        client.check_scope()
        before = snapshot(k, payload["workloads"])
        for app in ("api-gateway", "web-bff"):
            probe.equivalent_source_selectors(payload["policies"], before[(tx.NAMESPACE, app)]["labels"],
                                              {"app": app, probe.OWNER: nonce})
        other_policies = outside_policies(k)
        original_services = service_snapshot(k)
        def stable():
            if snapshot(k, payload["workloads"]) != before or not tx.same_inventory(other_policies, outside_policies(k), exact_version=True) or service_snapshot(k) != original_services:
                raise tx.Refusal("workload identity/readiness/restart changed")
        client.check_workloads = stable
        transaction = tx.Transaction(client, desired, nonce, record)
        transaction.prepare(lambda actual, target: tx.qualify_rendered_baseline(actual, target, payload["policies"]))
        if args.apply:
            probes = Probes(k, lambda image_id: json.loads(command(["/usr/local/bin/k3s", "crictl", "inspecti", image_id])), payload["identity"], nonce, record)
            probes.create()
            def verify_and_clean():
                probes.verify(before, True)
                probes.close()
                stable()
            transaction.apply(verify_and_clean)
        print(json.dumps({"format": "kanz-tokyo-gateway-policy-v1", "source": args.source,
                          "mode": "apply" if args.apply else "dry_run", "phase": transaction.receipt["phase"],
                          "receipt": receipt_path, "cleanup": "verified" if args.apply else "not_created"}))
    except Exception:
        try:
            if probes is not None:
                probes.close()
        finally:
            print(json.dumps({"source": args.source, "verdict": "REFUSED", "receipt": receipt_path}))
        raise
    finally:
        if receipts is not None:
            receipts.close()
        if scratch is not None:
            scratch.cleanup()


if __name__ == "__main__":
    main()

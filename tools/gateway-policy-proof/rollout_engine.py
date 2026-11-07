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

from rollout_scope import CLUSTER_UID, APPS, snapshot, outside_policies, service_snapshot

PORTS = {"accounting": (8101, 8080), "audit": (8102, 8083), "compliance": (8095, 8091)}
from network_observation import TCP_BATCH as TCP, DENIALS, batch


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
        self.started = time.monotonic()

    def record(self, phase):
        self.save({"format": "kanz-gateway-policy-probes-v1", "nonce": self.nonce, "phase": phase,
                   "namespace": self.namespace, "namespaceUID": self.namespace_uid_custody,
                   "pods": [{key: entry.get(key) for key in ("name", "namespace", "uid", "address", "addresses", "addressState", "addressUncertain", "deleted")} for entry in self.owned]})

    def observe(self, entry, actual, cleanup=False):
        uid, _ = tx.version(actual)
        if uid != entry["uid"] or actual["metadata"].get("labels", {}).get(probe.OWNER) != self.nonce:
            raise tx.Refusal("probe observation ownership uncertain")
        try:
            values = probe.pod_addresses(actual)
        except ValueError:
            entry["addressState"] = "malformed"
            entry["addressUncertain"] = True
            self.record("pod_address_observed")
            if cleanup:
                return
            raise
        old = (entry.get("addressState"), entry.get("addresses", []))
        entry["addressState"] = "unknown" if values is None else ("observed" if values else "empty")
        addresses = sorted(set(old[1]) | set(values or []))
        if len(addresses) > 8:
            entry["addressState"] = "overflow"
            entry["addressUncertain"] = True
            self.record("pod_address_observed")
            if cleanup:
                return
            raise tx.Refusal("probe address history exceeds bound")
        entry["addresses"] = addresses
        self.addresses = sorted({a for p in self.owned for a in p.get("addresses", [])})
        if old != (entry["addressState"], entry["addresses"]):
            self.record("pod_address_observed")

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
                    self.observe(entry, actual)
                self.record("pod_create_observed")
            end = time.monotonic() + 90
            while True:
                actual = json.loads(self.k("-n", namespace, "get", "pod", name, "-o", "json"))
                self.observe(entry, actual)
                if actual.get("status", {}).get("phase") == "Running":
                    break
                if time.monotonic() >= end:
                    raise tx.Refusal("probe did not become Running")
                time.sleep(1)
            image_id = actual["status"]["containerStatuses"][0]["imageID"]
            entry["verifiedImageID"] = probe.verify_cri(image_id, self.inspect(image_id), self.identity)
            address = probe.running_unready(actual, desired, entry["verifiedImageID"])
            entry["address"] = address
            self.record("pod_running_unready_observed")
            self.peers[peer] = entry
        self.check()

    def check(self):
        services = self.services()
        controllers = self.controllers()
        inventory = json.loads(self.k("get", "pods", "-A", "-l", probe.OWNER + "=" + self.nonce, "-o", "json"))
        items = inventory.get("items") if isinstance(inventory, dict) else None
        expected = {(p["namespace"], p["name"]) for p in self.owned}
        if not isinstance(items, list) or len(items) != len(self.owned):
            raise tx.Refusal("probe inventory count differs")
        pods = {}
        for obj in items:
            meta = obj.get("metadata", {}) if isinstance(obj, dict) else {}
            if (not isinstance(meta, dict) or not isinstance(meta.get("namespace"), str)
                    or not isinstance(meta.get("name"), str) or not isinstance(meta.get("labels"), dict)):
                raise tx.Refusal("malformed probe inventory")
            key = (meta["namespace"], meta["name"])
            if key in pods:
                raise tx.Refusal("duplicate probe inventory key")
            pods[key] = obj
        if set(pods) != expected:
            raise tx.Refusal("probe inventory keys differ")
        for entry in self.owned:
            obj = pods[(entry["namespace"], entry["name"])]
            self.observe(entry, obj)
            probe.service_boundary(services, entry["namespace"], entry["desired"]["metadata"]["labels"])
            probe.controller_boundary(controllers, entry["namespace"], entry["desired"]["metadata"]["labels"])
            if probe.running_unready(obj, entry["desired"], entry["verifiedImageID"]) != entry["address"]:
                raise tx.Refusal("probe address changed")
        probe.endpoint_boundary(self.endpoints(), self.addresses)

    def tcp(self, peer, address, port, expected, target, observations, deadline):
        item = {"target": target, "peer": peer, "port": port, "expected": expected,
                "batches": [], "result": "unknown"}
        if len(observations) >= 48:
            raise tx.Refusal("network observation bound")
        observations.append(item)
        consecutive = 0
        end = min(deadline, time.monotonic() + 20)
        for _ in range(6):
            if time.monotonic() >= end:
                break
            self.check()
            entry = self.peers[peer]
            values = batch(json.loads(self.k("-n", entry["namespace"], "exec", entry["name"], "--",
                                            "python3", "-I", "-B", "-c", TCP, address, str(port),
                                            timeout=min(10, max(.001, end - time.monotonic())))))
            item["batches"].append(values)
            if time.monotonic() >= end or "unknown" in values:
                raise tx.Refusal("network observation unknown or late")
            for value in values:
                accepted = value == "connected" if expected == "connected" else value in DENIALS
                consecutive = consecutive + 1 if accepted else 0
            if consecutive >= 3:
                item["result"] = "verified"
                return
        item["result"] = "not_converged"
        raise tx.Refusal("network observation did not converge")

    def verify(self, workloads, isolated):
        if type(isolated) is not bool:
            raise tx.Refusal("invalid isolation mode")
        observations = []
        passed = False
        original = self.k
        # Work fits inside the existing 450-second caller lifetime. Reserve a
        # separate ten seconds for the mandatory post-negative positive control.
        final_end = min(self.started + 420, time.monotonic() + 180)
        work_end = final_end - 10
        active_end = work_end
        def bounded_k(*args, value=None, timeout=100):
            remaining = active_end - time.monotonic()
            if remaining <= 0:
                raise tx.Refusal("network observation deadline")
            return original(*args, value=value, timeout=min(timeout, remaining, 100))
        self.k = bounded_k
        def observe(peer, target, port, expected):
            self.tcp(peer, workloads[(tx.NAMESPACE, target)]["ip"], port, expected,
                     target, observations, active_end)
        try:
            for app, (api, ops) in PORTS.items():
                observe("gateway", app, api, "connected")
                observe("observer", app, ops, "connected")
                if isolated:
                    for peer in ("untrusted", "bff", "observer", "outsider"):
                        observe("gateway", app, api, "connected")
                        try:
                            observe(peer, app, api, "denied")
                        finally:
                            active_end = min(final_end, time.monotonic() + 10)
                            try:
                                observe("gateway", app, api, "connected")
                            finally:
                                active_end = work_end
            observe("bff", "api-gateway", 8080, "connected")
            passed = True
        finally:
            self.k = original
            # One bounded aggregate per verification, including failures, reaches
            # durable storage before Transaction.apply can start rollback.
            self.save({"format": "kanz-gateway-network-v1", "nonce": self.nonce,
                       "phase": "network_verified" if passed else "network_refused", "isolated": isolated,
                       "observations": observations})

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
            self.observe(entry, actual, cleanup=True)
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
                probe.endpoint_boundary(self.endpoints(), self.addresses, require_absent=True, owned_uids=[p["uid"] for p in self.owned])
                break
            except ValueError:
                if time.monotonic() >= end:
                    raise
                time.sleep(1)
        if any(p.get("addressUncertain") or p.get("addressState") not in ("observed", "empty") for p in self.owned):
            raise tx.Refusal("probe address custody remains unknown")
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
        def command(argv, value=None, timeout=100):
            return bounded_run(argv, data=None if value is None else json.dumps(value),
                               env=environment, cwd=directory, timeout=min(timeout, 100))
        def k(*argv, value=None, timeout=100):
            return command(["/usr/local/bin/k3s", "kubectl", *argv], value, timeout)
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

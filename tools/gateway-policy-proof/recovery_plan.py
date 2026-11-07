"""Read-only retained transaction plan. This module cannot restore a policy."""
import argparse
import copy
import ipaddress
import json
import os
import re
import sys
import tempfile

from bounded_process import run as bounded_run
from policy_client import KubernetesClient
import recovery_receipts as receipts
import rollout_scope as scope
import policy_state as tx


def cleanup_custody(records, nonce):
    namespace = "gateway-policy-" + nonce
    uid, pods, addresses = None, {}, set()
    for record in records:
        if record.get("namespace") != namespace:
            raise tx.Refusal("probe namespace history differs")
        observed = record.get("namespaceUID")
        if observed is not None:
            if not isinstance(observed, str) or not re.fullmatch(receipts.UUID, observed) or uid not in (None, observed):
                raise tx.Refusal("probe namespace UID history differs")
            uid = observed
        entries = record.get("pods")
        if not isinstance(entries, list) or len(entries) > 5:
            raise tx.Refusal("probe receipt inventory differs")
        for entry in entries:
            if not isinstance(entry, dict):
                raise tx.Refusal("probe receipt shape differs")
            key = (entry.get("namespace"), entry.get("name"))
            if (key[0] not in (tx.NAMESPACE, "kanz-observability", namespace)
                    or not isinstance(key[1], str) or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,62}", key[1])):
                raise tx.Refusal("probe identity history differs")
            old = pods.get(key)
            pod_uid = entry.get("uid")
            if pod_uid is not None and (not isinstance(pod_uid, str) or not re.fullmatch(receipts.UUID, pod_uid)):
                raise tx.Refusal("probe UID malformed")
            if old and old.get("uid") is not None and pod_uid != old["uid"]:
                raise tx.Refusal("probe UID changed")
            if entry.get("addressUncertain") or entry.get("addressState") in ("malformed", "overflow"):
                raise tx.Refusal("historical observed address inventory uncertain")
            values = entry.get("addresses")
            if values is None:
                if entry.get("addressState") not in (None, "unknown", "empty"):
                    raise tx.Refusal("probe address list absent")
                values = []
            if not isinstance(values, list) or len(values) > 8:
                raise tx.Refusal("probe address list malformed")
            primary = entry.get("address")
            for value in [*values, *([] if primary is None else [primary])]:
                if not isinstance(value, str) or len(value) > 45:
                    raise tx.Refusal("probe address malformed")
                try:
                    address = ipaddress.ip_address(value)
                except ValueError:
                    raise tx.Refusal("probe address invalid") from None
                addresses.add(str(address))
            pods[key] = entry
    if uid is None or len(pods) != 5 or len(addresses) > 40:
        raise tx.Refusal("probe custody incomplete")
    for entry in pods.values():
        if entry.get("uid") is None or entry.get("deleted") is not True or entry.get("addressState") not in ("observed", "empty"):
            raise tx.Refusal("final probe custody unknown")
        if entry["addressState"] == "observed" and not entry.get("addresses"):
            raise tx.Refusal("observed probe addresses missing")
    uids = [entry["uid"] for entry in pods.values()]
    if len(set(uids)) != 5:
        raise tx.Refusal("probe UID collision")
    return namespace, uid, pods, sorted(addresses)


def cleanup_absent(k, custody, nonce):
    namespace, namespace_uid, pods, addresses = custody
    items = json.loads(k("get", "pods", "-A", "-o", "json"))["items"]
    if not isinstance(items, list) or len(items) > 512:
        raise tx.Refusal("pod inventory exceeds bound")
    uids = {entry["uid"] for entry in pods.values()}
    for pod in items:
        meta = pod["metadata"]
        if (meta["uid"] in uids or (meta["namespace"], meta["name"]) in pods
                or meta.get("labels", {}).get(scope.OWNER) == nonce):
            raise tx.Refusal("retained probe still present or replaced")
    namespaces = json.loads(k("get", "namespaces", "-o", "json"))["items"]
    if not isinstance(namespaces, list) or len(namespaces) > 256:
        raise tx.Refusal("namespace inventory exceeds bound")
    if any(n["metadata"]["name"] == namespace or n["metadata"]["uid"] == namespace_uid
           or n["metadata"].get("labels", {}).get(scope.OWNER) == nonce for n in namespaces):
        raise tx.Refusal("retained namespace still present or replaced")
    endpoints = json.loads(k("get", "endpointslices", "-A", "-o", "json"))["items"]
    scope.endpoint_boundary(endpoints, addresses, require_absent=True, owned_uids=sorted(uids))


def plan(k, payload, records, receipt_hash):
    if set(payload) != {"source", "policies", "workloads", "provenance"} or not re.fullmatch(r"[0-9a-f]{40}", payload["source"]):
        raise tx.Refusal("plan input differs")
    proof = payload["provenance"]
    expected = {"command": receipts.COMMAND, "instance": "i-0ee1235614219f089", "oldSource": receipts.OLD_SOURCE,
                "directory": receipts.DIRECTORY, "originalPoliciesSHA256": tx.fingerprint(payload["policies"]),
                "originalWorkloadsSHA256": tx.fingerprint(payload["workloads"])}
    if not isinstance(proof, dict) or set(proof) != set(expected) | {"commandSHA256", "sourceSHA256", "bundleSHA256"}:
        raise tx.Refusal("host provenance binding missing")
    if any(proof[key] != value for key, value in expected.items()) or any(not isinstance(proof[key], str) or not re.fullmatch(r"[0-9a-f]{64}", proof[key]) for key in ("commandSHA256", "sourceSHA256", "bundleSHA256")):
        raise tx.Refusal("host provenance binding differs")
    baseline, entry, nonce, probe_records = receipts.retained(records)
    desired = tx.inventory([p for p in payload["policies"] if p["metadata"]["name"] in tx.NAMES])
    rendered = [p for p in payload["policies"] if p["metadata"]["namespace"] == tx.NAMESPACE]
    tx.qualify_rendered_baseline(baseline, desired, rendered)
    if tx.fingerprint(list(desired.values())) != records[0].get("desiredSHA256"):
        raise tx.Refusal("original desired inventory differs")
    client = KubernetesClient(k, scope.CLUSTER_UID)
    client.check_scope()
    current = tx.inventory(client.list_policies())
    actual = receipts.target_matches(current, baseline, entry, nonce)
    workloads = scope.snapshot(k, payload["workloads"])
    services = scope.service_snapshot(k)
    outside = scope.outside_policies(k)
    custody = cleanup_custody(probe_records, nonce)
    cleanup_absent(k, custody, nonce)
    def stable():
        client.check_scope()
        if (scope.snapshot(k, payload["workloads"]) != workloads or scope.service_snapshot(k) != services
                or not tx.same_inventory(scope.outside_policies(k), outside, exact_version=True)
                or not tx.same_inventory(tx.inventory(client.list_policies()), current, exact_version=True)):
            raise tx.Refusal("fresh plan scope changed")
        cleanup_absent(k, custody, nonce)
    stable()
    proposed = copy.deepcopy(baseline[receipts.TARGET])
    proposed["metadata"]["resourceVersion"] = tx.version(actual)[1]
    for field in tx.SERVER_METADATA - {"uid", "resourceVersion"}:
        proposed["metadata"].pop(field, None)
    # Server validation only. No observation is appended and no restore is issued.
    client.dry_run(proposed, True)
    stable()
    return {"format": "kanz-tokyo-retained-plan-v1", "source": payload["source"],
            "oldSource": receipts.OLD_SOURCE, "command": receipts.COMMAND,
            "receiptSHA256": receipt_hash, "provenanceSHA256": tx.fingerprint(proof),
            "policyInventorySHA256": tx.fingerprint(list(current.values())),
            "freshScopeSHA256": tx.fingerprint({"workloads": sorted(workloads.items()),
                "services": services, "outsidePolicies": list(outside.values())}),
            "cleanupCustodySHA256": tx.fingerprint({"namespaceUID": custody[1],
                "pods": sorted(custody[2].items()), "observedAddresses": custody[3]}),
            "fullCurrentTargetSHA256": tx.fingerprint(actual),
            "targetUID": tx.version(actual)[0], "targetResourceVersion": tx.version(actual)[1],
            "proposedBaselineSHA256": tx.fingerprint(tx.semantic(proposed)),
            "legacyHistoricalSnapshots": "UNKNOWN", "freshCanonicalScope": "VERIFIED",
            "observedProbeUIDAndAddressAbsence": "VERIFIED", "serverDryRun": "VERIFIED",
            "persistentPolicyWrites": 0, "restoreAuthorized": False}


def inventory_command(argv, value, environment, directory):
    reads = {
        ("get", "--raw", "/apis/networking.k8s.io/v1/namespaces/kanz-services/networkpolicies/allow-gateway-runtime-egress"),
        ("get", "namespace", "kube-system", "-o", "json"),
        ("-n", "kanz-services", "get", "networkpolicies", "-o", "json"),
        ("-n", "kanz-services", "get", "deployments", "-o", "json"),
        ("-n", "kanz-services", "get", "replicasets", "-o", "json"),
        ("-n", "kanz-services", "get", "pods", "-o", "json"),
        ("-n", "kanz-messaging", "get", "pods", "-o", "json"),
        ("get", "networkpolicies", "-A", "-o", "json"),
        ("get", "services", "-A", "-o", "json"),
        ("get", "pods", "-A", "-o", "json"),
        ("get", "namespaces", "-o", "json"),
        ("get", "endpointslices", "-A", "-o", "json"),
    }
    dry_run = ("replace", "--raw", "/apis/networking.k8s.io/v1/namespaces/kanz-services/networkpolicies/allow-gateway-runtime-egress?dryRun=All", "-f", "-")
    if not ((argv in reads and value is None) or (argv == dry_run and isinstance(value, dict))):
        raise tx.Refusal("planner command outside exact inventory/dry-run scope")
    return bounded_run(["/usr/local/bin/k3s", "kubectl", *argv], data=None if value is None else json.dumps(value),
                       env=environment, cwd=directory, timeout=100)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--tokyo-plan", action="store_true", required=True)
    parser.add_argument("--source", required=True)
    args = parser.parse_args()
    if sys.platform != "linux" or os.geteuid() != 0:
        raise tx.Refusal("fixed Linux root planning required")
    raw = sys.stdin.buffer.read(524289)
    if len(raw) > 524288:
        raise tx.Refusal("plan input exceeds bound")
    payload = receipts.document(raw)
    if payload.get("source") != args.source:
        raise tx.Refusal("plan source differs")
    records, receipt_hash = receipts.read_receipts()
    with tempfile.TemporaryDirectory(prefix="kanz-gateway-plan-", dir="/var/tmp") as directory:
        environment = {"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                       "HOME": directory, "TMPDIR": directory, "LANG": "C.UTF-8"}
        def k(*argv, value=None):
            return inventory_command(argv, value, environment, directory)
        result = plan(k, payload, records, receipt_hash)
        _, after = receipts.read_receipts()
        if after != receipt_hash:
            raise tx.Refusal("retained receipts changed during plan")
        print(json.dumps(result, separators=(",", ":")))


if __name__ == "__main__":
    main()

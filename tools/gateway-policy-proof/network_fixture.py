"""Actual TCP gate against fixture-owned listeners; no live context entrypoint."""
import json
from pathlib import Path
import sys
import types


def load(name):
    path = Path(__file__).parent / (name + ".py")
    module = types.ModuleType(name)
    module.__file__ = str(path)
    sys.modules[name] = module
    exec(compile(path.read_bytes(), str(path), "exec"), module.__dict__)
    return module


def qualify(k, peers, endpoints, image, mode):
    # Same checked production gate, with a fixture-specific ownership check:
    # these synthetic HTTP servers intentionally are ready Service endpoints.
    for name in ("network_observation", "bounded_process", "policy_state", "transaction", "policy_client", "rollout_scope", "rollout_probe"):
        load(name)
    engine = load("rollout_engine")
    evidence = load("network_evidence")
    identities = {}
    def snapshot(query=k):
        result = {}
        for namespace in sorted({namespace for namespace, _ in peers.values()}):
            pods = json.loads(query("-n", namespace, "get", "pods", "-o", "json"))["items"]
            if len(pods) > 16:
                raise ValueError("fixture inventory exceeds bound")
            for pod in pods:
                meta, status = pod["metadata"], pod["status"]
                if (status.get("phase") != "Running" or not any(c.get("type") == "Ready" and c.get("status") == "True" for c in status.get("conditions", []))
                        or len(pod["spec"]["containers"]) != 1 or pod["spec"]["containers"][0]["image"] != image
                        or len(status.get("containerStatuses", [])) != 1 or status["containerStatuses"][0].get("restartCount") != 0):
                    raise ValueError("fixture workload changed")
                result[(namespace, meta["name"])] = (meta["uid"], status["podIP"], status["containerStatuses"][0]["imageID"])
        return result
    identities = snapshot()
    def check():
        if snapshot(probes.k) != identities:
            raise ValueError("fixture identity changed")
    def transport(*args, value=None, timeout=100):
        return k(*args, data=None if value is None else json.dumps(value), timeout=min(timeout, 90))
    records = []
    probes = engine.Probes(transport, None, None, "a" * 32, records.append)
    probes.check = check
    aliases = {"gateway": "api-gateway", "bff": "web-bff", "untrusted": "untrusted", "observer": "observer", "outsider": "spoof-gateway"}
    probes.peers = {alias: {"namespace": peers[name][0], "name": peers[name][1]} for alias, name in aliases.items()}
    workloads = {("kanz-services", app): {"ip": value["pod"]} for app, value in endpoints.items()}
    if mode == "closed":
        workloads[("kanz-services", "accounting")] = {"ip": identities[peers["untrusted"]][1]}
    refused = False
    try:
        probes.verify(workloads, True)
    except engine.tx.Refusal:
        refused = True
    if len(records) != 1 or refused != (mode != "enforced"):
        raise ValueError("actual network gate discriminating control failed")
    record = records[0]
    evidence.validate(record, "a" * 32)
    validate_control(mode, record)
    return record


def validate_control(mode, record):
    load("network_observation")
    evidence = load("network_evidence")
    evidence.validate(record, "a" * 32)
    if record["isolated"] is not True or record["phase"] != ("network_verified" if mode == "enforced" else "network_refused"):
        raise ValueError("actual network control verdict differs")
    items = record["observations"]
    if mode == "closed":
        if (len(items) != 1 or items[0]["result"] != "not_converged" or not items[0]["batches"]
                or not all(v == "refused" for b in items[0]["batches"] for v in b)):
            raise ValueError("closed listener did not produce exact refusal")
    if mode == "no_policy":
        if len(items) != 5:
            raise ValueError("no-policy observation inventory")
        negative = items[-2]
        if (negative["expected"] != "denied" or negative["result"] != "not_converged" or not negative["batches"]
                or not all(v == "connected" for b in negative["batches"] for v in b)
                or items[-1]["result"] != "verified"):
            raise ValueError("no-policy control or after-positive differs")


def validate(controls):
    if not isinstance(controls, dict) or set(controls) != {"closed", "no_policy", "enforced"}:
        raise ValueError("actual network controls incomplete")
    for name, record in controls.items():
        validate_control(name, record)

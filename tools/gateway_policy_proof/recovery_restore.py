"""One-shot restoration of the fixed retained baseline; never retry or roll forward."""
import argparse
import copy
import json
import os
import signal
import sys
import tempfile

from bounded_process import run as bounded_run
import policy_state as state
import recovery_plan as planner
import recovery_receipts as receipts
import rollout_scope as scope
from policy_client import KubernetesClient

DIRECTORY = "/var/tmp/kanz-gateway-reconcile-nu1ezqwo"
PUT = ("replace", "--raw", "/apis/networking.k8s.io/v1/namespaces/kanz-services/networkpolicies/allow-gateway-runtime-egress", "-f", "-")
GET = ("get", "--raw", PUT[2])


def target_projection(value):
    # Pinned K3s JSONYamlPrintFlags wraps OmitManagedFieldsPrinter by default.
    # This projection is only for comparing this target's raw object with the
    # existing list representation; raw metadata remains independently bound.
    result = copy.deepcopy(value)
    result["metadata"].pop("managedFields", None)
    return result


def read_target(k, projected):
    raw = receipts.document(k(*GET).encode("utf-8"))
    if set(state.inventory([raw])) != {receipts.TARGET} or target_projection(raw) != projected:
        raise state.Refusal("raw target differs from exact list projection")
    state.version(raw)
    return raw


class PutOnce:
    def __init__(self, environment, directory, executor=None):
        self.environment, self.directory = environment, directory
        self.execute = bounded_run if executor is None else executor
        self.attempted = False

    def __call__(self, value):
        if self.attempted:
            raise state.Refusal("restore PUT already attempted")
        if not isinstance(value, dict) or set(state.inventory([value])) != {receipts.TARGET}:
            raise state.Refusal("restore PUT outside fixed target")
        state.version(value)
        self.attempted = True
        return self.execute(["/usr/local/bin/k3s", "kubectl", *PUT], data=json.dumps(value),
                            env=self.environment, cwd=self.directory, timeout=100)


def create_journal(directory):
    if not os.path.isabs(directory):
        raise state.Refusal("journal must have an absolute owned path")
    parent = os.open(os.path.dirname(directory), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        os.mkdir(os.path.basename(directory), 0o700, dir_fd=parent)
        os.fsync(parent)
    finally:
        os.close(parent)
    return state.Receipts(directory)


def observation(k, payload, custody, nonce):
    client = KubernetesClient(k, scope.CLUSTER_UID)
    client.check_scope()
    policies = state.inventory(client.list_policies())
    for obj in policies.values():
        state.version(obj)
    workloads = scope.snapshot(k, payload["workloads"])
    services = scope.service_snapshot(k)
    outside = scope.outside_policies(k)
    planner.cleanup_absent(k, custody, nonce)
    return {"policies": list(policies.values()), "workloads": sorted(workloads.items()),
            "services": services, "outsidePolicies": list(outside.values())}


def acknowledged(raw, intended, before):
    obj = receipts.document(raw.encode("utf-8"))
    actual = state.inventory([obj])
    if set(actual) != {receipts.TARGET}:
        raise state.Refusal("restore response identity differs")
    uid, rv = state.version(obj)
    old_uid, old_rv = state.version(before)
    meta, prior = obj["metadata"], before["metadata"]
    if (uid != old_uid or rv == old_rv
            or ("creationTimestamp" in meta) != ("creationTimestamp" in prior)
            or meta.get("creationTimestamp") != prior.get("creationTimestamp")
            or state.semantic(obj) != state.semantic(intended)):
        raise state.Refusal("restore response does not acknowledge exact baseline")
    return obj


def reconcile(k, put_once, payload, read_receipts, save):
    """The caller supplies a fresh exclusive journal, never a resume facility."""
    attempted = False
    phase = "preflight"
    journal = {"format": "kanz-tokyo-retained-restore-v1", "source": payload.get("source"),
               "oldSource": receipts.OLD_SOURCE, "command": receipts.COMMAND,
               "legacyHistoricalSnapshots": "UNKNOWN", "writeAttempted": False}
    try:
        records, receipt_hash = read_receipts()
        plan = planner.plan(k, payload, records, receipt_hash)
        baseline, entry, nonce, probes = receipts.retained(records)
        custody = planner.cleanup_custody(probes, nonce)
        before = observation(k, payload, custody, nonce)
        current = state.inventory(before["policies"])
        target = receipts.target_matches(current, baseline, entry, nonce)
        raw_target = read_target(k, target)
        if (state.fingerprint(before["policies"]) != plan["policyInventorySHA256"]
                or state.fingerprint(target) != plan["fullCurrentTargetSHA256"]
                or state.fingerprint({key: before[key] for key in ("workloads", "services", "outsidePolicies")}) != plan["freshScopeSHA256"]):
            raise state.Refusal("fresh observation differs from verified plan")
        proposed = copy.deepcopy(baseline[receipts.TARGET])
        proposed["metadata"]["resourceVersion"] = state.version(target)[1]
        for field in state.SERVER_METADATA - {"uid", "resourceVersion"}:
            proposed["metadata"].pop(field, None)
        journal.update(nonce=nonce, receiptSHA256=receipt_hash,
                       provenanceSHA256=state.fingerprint(payload["provenance"]),
                       plan=plan, observation=before, originalBaseline=baseline[receipts.TARGET],
                       rawCurrentTarget=raw_target, rawCurrentTargetSHA256=state.fingerprint(raw_target),
                       originalIntent=entry, cleanupCustody={"namespaceUID": custody[1],
                           "pods": sorted(custody[2].items()), "observedAddresses": custody[3]})
        phase = "observation_recorded"
        save({**journal, "phase": phase})

        def stable():
            _, current_hash = read_receipts()
            if current_hash != receipt_hash or observation(k, payload, custody, nonce) != before:
                raise state.Refusal("restore preconditions changed")
            if read_target(k, target) != raw_target:
                raise state.Refusal("full raw target changed before restoration")

        stable()
        phase = "restore_intent"
        journal["intended"] = proposed
        save({**journal, "phase": phase})
        # Recheck after durable intent: fsync is not an authorization lease.
        stable()
        attempted = True
        journal["writeAttempted"] = True
        phase = "put_response"
        response = acknowledged(put_once(proposed), proposed, target)
        journal["response"] = response
        phase = "restore_acknowledged"
        save({**journal, "phase": phase})
        after = observation(k, payload, custody, nonce)
        policies = state.inventory(after["policies"])
        if policies.keys() != current.keys() or policies[receipts.TARGET] != target_projection(response):
            raise state.Refusal("restore readback differs from acknowledgment")
        raw_after = read_target(k, policies[receipts.TARGET])
        if raw_after != response:
            raise state.Refusal("full raw readback differs from acknowledgment")
        if any(policies[key] != current[key] for key in current if key != receipts.TARGET):
            raise state.Refusal("unowned policy changed during restoration")
        if any(after[key] != before[key] for key in ("workloads", "services", "outsidePolicies")):
            raise state.Refusal("fresh scope changed during restoration")
        _, final_hash = read_receipts()
        if final_hash != receipt_hash:
            raise state.Refusal("original receipts changed during restoration")
        phase = "verified"
        save({**journal, "phase": phase, "after": after, "rawReadbackSHA256": state.fingerprint(raw_after)})
        return {"format": "kanz-tokyo-retained-restore-result-v1", "source": payload["source"],
                "command": receipts.COMMAND, "receiptSHA256": receipt_hash,
                "provenanceSHA256": journal["provenanceSHA256"],
                "beforeSHA256": state.fingerprint(before), "afterSHA256": state.fingerprint(after),
                "rawBeforeSHA256": state.fingerprint(raw_target), "rawAfterSHA256": state.fingerprint(raw_after),
                "targetUID": state.version(response)[0], "targetResourceVersion": state.version(response)[1],
                "result": "VERIFIED", "writeAttempts": 1, "legacyHistoricalSnapshots": "UNKNOWN"}
    except Exception:
        # Never infer attribution from a baseline observed after a lost response.
        # If the journal itself cannot be appended, the durable intent remains.
        save({**journal, "phase": "unknown" if attempted else "refused", "failedPhase": phase})
        raise state.Refusal("retained reconciliation unverified; preserve journal and do not retry") from None


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--tokyo-retained-baseline", action="store_true", required=True)
    parser.add_argument("--source", required=True)
    args = parser.parse_args()
    if sys.platform != "linux" or os.geteuid() != 0:
        raise state.Refusal("fixed Linux root restoration required")
    raw = sys.stdin.buffer.read(524289)
    if len(raw) > 524288:
        raise state.Refusal("restore input exceeds bound")
    payload = receipts.document(raw)
    if payload.get("source") != args.source:
        raise state.Refusal("restore source differs")
    def cancelled(_signum, _frame):
        raise state.Refusal("restore interrupted")
    signal.signal(signal.SIGTERM, cancelled)
    signal.signal(signal.SIGINT, cancelled)
    with tempfile.TemporaryDirectory(prefix="kanz-gateway-restore-", dir="/var/tmp") as directory:
        environment = {"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                       "HOME": directory, "TMPDIR": directory, "LANG": "C.UTF-8"}
        def k(*argv, value=None):
            return planner.inventory_command(argv, value, environment, directory)
        records, receipt_hash = receipts.read_receipts()
        planner.plan(k, payload, records, receipt_hash)
        # Preflight failures do not consume an attempt. Once created, even a
        # prewrite-refused journal remains and requires separate operator review.
        # There is no random-directory fallback or automatic resume.
        journal = create_journal(DIRECTORY)
        try:
            result = reconcile(k, PutOnce(environment, directory), payload, receipts.read_receipts, journal)
            print(json.dumps(result, separators=(",", ":")))
        finally:
            journal.close()


if __name__ == "__main__":
    main()

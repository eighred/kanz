"""Disposable API controls with explicitly synthetic legacy receipts, never Tokyo IO."""
import copy
import hashlib
import json
from pathlib import Path
from unittest.mock import patch

import policy_state as state
import recovery_receipts as receipts
import recovery_restore as restore
from policy_client import KubernetesClient

CASES = ("retained_restore_verified", "retained_restore_response_loss", "retained_restore_stale_cas",
         "retained_restore_metadata_refusal", "retained_restore_birth_projection_refusal",
         "retained_restore_scope_refusal", "retained_restore_receipt_refusal", "retained_restore_exclusive_journal")


def failure_projection(error, progress):
    """Synthetic fixture diagnostics only: no exception text or frame values."""
    modules = {"restore_fixture.py", "recovery_restore.py", "recovery_plan.py", "recovery_receipts.py",
               "rollout_scope.py", "policy_state.py", "policy_client.py", "bounded_process.py", "transaction_fixture.py"}
    classes = {state.Refusal: "refusal", ValueError: "value", TypeError: "type", KeyError: "key",
               AssertionError: "assertion", RuntimeError: "runtime", OSError: "os", TimeoutError: "timeout"}
    phases = {"none", "preflight", "observation_recorded", "restore_intent", "put_response",
              "restore_acknowledged", "verified", "refused", "unknown"}
    contexts, seen, frame_count = [], set(), 0
    truncated = False
    while error is not None and len(contexts) < 8 and id(error) not in seen:
        seen.add(id(error))
        frames, tb = [], error.__traceback__
        while tb is not None and frame_count < 16:
            frame_count += 1
            name = Path(tb.tb_frame.f_code.co_filename).name
            line = tb.tb_lineno
            if name in modules and type(line) is int and 1 <= line <= 1000000:
                frames.append({"module": name, "line": line})
            tb = tb.tb_next
        truncated = truncated or tb is not None
        contexts.append({"category": classes.get(type(error), "other"), "frames": frames})
        error = error.__context__
    truncated = truncated or error is not None
    count = progress.get("receiptCount")
    def closed(value, allowed):
        return value if type(value) is str and value in allowed else "unknown"
    return {"case": closed(progress.get("case"), CASES),
            "lastDurablePhase": closed(progress.get("phase"), phases),
            "failedPhase": closed(progress.get("failedPhase"), phases),
            "writeAttempted": progress.get("writeAttempted") if type(progress.get("writeAttempted")) is bool else None,
            "durableReceiptCount": count if type(count) is int and 0 <= count <= 64 else None,
            "contexts": contexts, "truncated": truncated}


def durable_save(journal, value, progress):
    journal(value)
    # A failed append can consume a sequence ordinal. Count only confirmed
    # append+fsync completions, not the journal's next-file sequence number.
    progress.update(phase=value["phase"], failedPhase=value.get("failedPhase", "none"),
                    writeAttempted=value["writeAttempted"], receiptCount=progress["receiptCount"] + 1)


def validate(value):
    if (not isinstance(value, dict) or set(value) != {"cases", "syntheticLegacyReceipts",
            "historicalProvenanceQualified", "actualAppendOnlyJournals", "singleAttemptEnforced", "actualManagedFieldsProjection"}
            or value["cases"] != list(CASES) or value["syntheticLegacyReceipts"] is not True
            or value["historicalProvenanceQualified"] is not False
            or value["actualManagedFieldsProjection"] is not True
            or value["actualAppendOnlyJournals"] is not True or value["singleAttemptEnforced"] is not True):
        raise ValueError("retained restoration controls incomplete")


def legacy_records(baseline, intended, nonce, desired, probe_records):
    """Construct the old schema using real fixture identities, not historical evidence."""
    entry = {"namespace": state.NAMESPACE, "name": state.NAMES[-1],
             "before": baseline[receipts.TARGET], "intended": intended, "observed": None}
    common = {"format": "kanz-gateway-policy-transaction-v1", "nonce": nonce,
              "baseline": list(baseline.values()), "desiredSHA256": state.fingerprint(desired)}
    def tx(phase, writes, rollback):
        return copy.deepcopy({**common, "phase": phase, "writes": writes, "rollback": rollback})
    result = [tx("prepared", [], "not_needed"), tx("dry_run_passed", [], "not_needed")]
    for phase in ("namespace_create_intent", "namespace_create_observed"):
        result.append(copy.deepcopy(next(r for r in probe_records if r["phase"] == phase)))
    # Current cleanup records more address observations than the old schema.
    # Select the old five-stage creation shape; every selected UID/IP came from
    # the actual disposable probe, which has already been removed.
    for count in range(1, 6):
        candidates = [r for r in probe_records if len(r["pods"]) == count]
        created = next(r for r in candidates if r["phase"] == "pod_create_intent")
        running = next(r for r in candidates if r["phase"] == "pod_running_unready_observed")
        observed = next(r for r in candidates if r["phase"] == "pod_create_observed")
        addresses = [r for r in candidates if r["phase"] == "pod_address_observed"]
        result.extend(copy.deepcopy([created, addresses[0], observed,
                      next(r for r in addresses if r["pods"][-1]["addressState"] == "observed"), running]))
    result.extend([tx("write_intent", [entry], "not_needed"), tx("rollback", [entry], "pending"),
                   tx("rollback_refused", [entry], "unresolved")])
    result.append(copy.deepcopy(next(r for r in probe_records if r["phase"] == "probe_cleanup_intent")))
    result.extend(copy.deepcopy([r for r in probe_records if r["phase"] == "pod_absence_observed"]))
    result.append(copy.deepcopy(probe_records[-1]))
    receipts.retained(result)
    return result


def qualify(k, cluster_uid, source, rendered, governed, probe_records, scratch, progress):
    """k is exclusively the fresh owned-kind executor supplied by run_fixture."""
    client = KubernetesClient(k, cluster_uid)
    desired = [p for p in rendered if p["metadata"]["name"] in state.NAMES]
    nonce = probe_records[-1]["nonce"]
    results = []
    for index, case in enumerate(CASES):
        progress.clear()
        progress.update(case=case, phase="none", failedPhase="none", writeAttempted=False, receiptCount=0)
        # Reset only the disposable owned policy resources. This setup is not
        # reconciliation and never runs against the fixed operator context.
        for obj in client.list_policies():
            if obj["metadata"]["name"] in state.NAMES:
                obj["metadata"].get("annotations", {}).pop(state.TOKEN, None)
                if obj["metadata"]["name"] == state.NAMES[-1]:
                    obj["metadata"].setdefault("annotations", {})[receipts.ANNOTATION] = '{"fixture":"old"}'
                    obj["metadata"]["annotations"]["fixture.example/restore"] = "preserved"
                    obj["spec"] = copy.deepcopy(next(p["spec"] for p in desired if p["metadata"]["name"] == state.NAMES[-1]))
                    for app, port in (("accounting", 8101), ("audit", 8102)):
                        obj["spec"]["egress"].remove({"to": [{"podSelector": {"matchLabels": {"app": app}}}],
                                                    "ports": [{"protocol": "TCP", "port": port}]})
                client.replace(obj)
        baseline = state.inventory(client.list_policies())
        intended = copy.deepcopy(baseline[receipts.TARGET])
        intended["spec"] = copy.deepcopy(next(p["spec"] for p in desired if p["metadata"]["name"] == state.NAMES[-1]))
        intended["metadata"].setdefault("annotations", {})[state.TOKEN] = nonce
        for field in state.SERVER_METADATA - {"uid", "resourceVersion"}:
            intended["metadata"].pop(field, None)
        # Discriminate the actual old kubectl annotation transformation.
        k("replace", "-f", "-", value=intended)
        current = state.inventory(client.list_policies())
        records = legacy_records(baseline, intended, nonce, desired, probe_records)
        receipts.target_matches(current, baseline, records[29]["writes"][0], nonce)
        old_dir = Path(scratch) / ("synthetic-old-" + str(index))
        old = restore.create_journal(str(old_dir))
        try:
            for record in records: old(record)
        finally:
            old.close()
        def read_old():
            raw = [(old_dir / ("receipt-%04d.json" % n)).read_bytes() for n in range(1, 40)]
            return [receipts.document(value) for value in raw], state.fingerprint([hashlib.sha256(value).hexdigest() for value in raw])
        payload = {"source": source, "policies": rendered, "workloads": governed}
        payload["provenance"] = {"command": receipts.COMMAND, "instance": "i-0ee1235614219f089",
            "oldSource": receipts.OLD_SOURCE, "directory": receipts.DIRECTORY,
            "originalPoliciesSHA256": state.fingerprint(rendered), "originalWorkloadsSHA256": state.fingerprint(governed),
            **{key: "d" * 64 for key in ("commandSHA256", "sourceSHA256", "bundleSHA256")}}
        journal_dir = Path(scratch) / ("synthetic-restore-" + str(index))
        journal = restore.create_journal(str(journal_dir))
        writes, acknowledged_before_read, concurrent_state = [], [], []
        initial = copy.deepcopy(current)
        if case == "retained_restore_metadata_refusal":
            changed = copy.deepcopy(current[receipts.TARGET])
            changed["metadata"]["annotations"]["fixture.example/foreign"] = "do-not-overwrite"
            client.replace(changed)
            initial = state.inventory(client.list_policies())
        def read_k(*argv, value=None):
            if argv == restore.PUT:
                raise AssertionError("read phase acquired write authority")
            if writes and case != "retained_restore_response_loss":
                saved = [json.loads(path.read_bytes()) for path in sorted(journal_dir.glob("receipt-*.json"))]
                if any(r["phase"] == "restore_acknowledged" and "response" in r for r in saved):
                    acknowledged_before_read.append(True)
                elif case == "retained_restore_verified":
                    raise AssertionError("after-observation preceded durable acknowledgment")
            raw = k(*argv, value=value)
            if case == "retained_restore_birth_projection_refusal" and argv == ("-n", state.NAMESPACE, "get", "networkpolicies", "-o", "json"):
                result = json.loads(raw)
                next(p for p in result["items"] if p["metadata"]["name"] == state.NAMES[-1])["metadata"]["creationTimestamp"] = "1900-01-01T00:00:00Z"
                return json.dumps(result)
            return raw
        def save(value):
            # Update only after the actual append and fsync complete. A failure
            # while saving therefore leaves the last confirmed durable phase.
            durable_save(journal, value, progress)
            if value["phase"] == "restore_intent":
                if case == "retained_restore_receipt_refusal":
                    with (old_dir / "receipt-0001.json").open("ab") as output: output.write(b" ")
                if case == "retained_restore_scope_refusal":
                    k("-n", state.NAMESPACE, "patch", "service", "probe-endpoint-control", "--type=merge",
                      "-p", '{"spec":{"sessionAffinity":"ClientIP"}}')
        def execute(argv, *, data, **kwargs):
            if argv != ["/usr/local/bin/k3s", "kubectl", *restore.PUT]:
                raise AssertionError("fixture writer escaped fixed PUT")
            pending = [json.loads(path.read_bytes()) for path in sorted(journal_dir.glob("receipt-*.json"))]
            if pending[-1]["phase"] != "restore_intent": raise AssertionError("PUT preceded durable intent")
            obj = json.loads(data)
            writes.append(obj)
            if case == "retained_restore_stale_cas":
                changed = copy.deepcopy(state.inventory(client.list_policies())[receipts.TARGET])
                changed["metadata"].setdefault("annotations", {})["fixture.example/concurrent"] = "retained"
                client.replace(changed)
                concurrent_state.append(state.inventory(client.list_policies()))
            result = k(*restore.PUT, value=obj)
            if case == "retained_restore_response_loss": raise RuntimeError("synthetic lost response after actual PUT")
            return result
        put = restore.PutOnce({}, str(scratch), executor=execute)
        try:
            if case == "retained_restore_exclusive_journal":
                try: restore.create_journal(str(journal_dir))
                except FileExistsError: pass
                else: raise AssertionError("existing journal was reused")
            else:
                with patch.object(restore.scope, "CLUSTER_UID", cluster_uid):
                    try: result = restore.reconcile(read_k, put, payload, read_old, save)
                    except state.Refusal:
                        if case == "retained_restore_verified": raise
                    else:
                        if case != "retained_restore_verified" or result["result"] != "VERIFIED":
                            raise AssertionError("negative restoration control accepted")
                saved = [json.loads(path.read_bytes()) for path in sorted(journal_dir.glob("receipt-*.json"))]
                expected = "verified" if case == "retained_restore_verified" else "unknown" if case in ("retained_restore_response_loss", "retained_restore_stale_cas") else "refused"
                if saved[-1]["phase"] != expected: raise AssertionError("restoration outcome differs")
            after = state.inventory(client.list_policies())
            expected_writes = int(case in ("retained_restore_verified", "retained_restore_response_loss", "retained_restore_stale_cas"))
            if len(writes) != expected_writes: raise AssertionError("restore attempt count differs")
            if case in ("retained_restore_verified", "retained_restore_response_loss"):
                if state.semantic(after[receipts.TARGET]) != state.semantic(baseline[receipts.TARGET]):
                    raise AssertionError("actual baseline not restored")
                if case == "retained_restore_verified" and not acknowledged_before_read:
                    raise AssertionError("server ACK not durable before after-observation")
                if case == "retained_restore_verified":
                    ack = next(r["response"] for r in saved if r["phase"] == "restore_acknowledged")
                    raw_after = json.loads(k(*restore.GET))
                    if (not isinstance(ack["metadata"].get("managedFields"), list)
                            or not ack["metadata"]["managedFields"]
                            or "managedFields" in after[receipts.TARGET]["metadata"]
                            or ack == after[receipts.TARGET] or raw_after != ack
                            or restore.target_projection(ack) != after[receipts.TARGET]):
                        raise AssertionError("actual raw versus printed metadata control differs")
            elif case == "retained_restore_stale_cas":
                if len(concurrent_state) != 1 or after != concurrent_state[0]:
                    raise AssertionError("stale CAS overwrote concurrent metadata")
            elif after != initial:
                raise AssertionError("refused restoration changed policies")
            if any(after[key] != initial[key] for key in initial if key != receipts.TARGET):
                raise AssertionError("restore changed another policy")
            if writes:
                try: put(writes[0])
                except state.Refusal: pass
                else: raise AssertionError("write capability allowed retry")
            results.append(case)
        finally:
            journal.close()
    return {"cases": results, "syntheticLegacyReceipts": True, "historicalProvenanceQualified": False,
            "actualAppendOnlyJournals": True, "singleAttemptEnforced": True, "actualManagedFieldsProjection": True}

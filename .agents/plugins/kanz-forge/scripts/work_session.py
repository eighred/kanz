"""Account for fixed work targets and bounded discovered repairs; never dispatch."""
import re

SHA = re.compile(r"[0-9a-f]{40}")


def text(value):
    return isinstance(value, str) and bool(value.strip())


def keyed(rows, label):
    result = {}
    if not isinstance(rows, list):
        raise ValueError(f"{label} must be a list")
    for row in rows:
        if not isinstance(row, dict) or not text(row.get("id")) or row["id"] in result:
            raise ValueError(f"{label} require unique nonempty IDs")
        result[row["id"]] = row
    return result


def assess_session(session, validation, graph=None, current_main=None):
    """Reduce source-reviewed decisions; the graph and main receipts stay external."""
    count = session["requested_count"]
    if type(count) is not int or count <= 0 or not text(session.get("selection_evidence")):
        raise ValueError("session needs a positive requested count and selection evidence")
    targets = keyed(session["targets"], "initial targets")
    if len(targets) > count or any(not text(t.get("acceptance_evidence")) for t in targets.values()):
        raise ValueError("initial targets exceed the request or lack acceptance evidence")
    findings = keyed(session.get("findings", []), "findings")
    if targets.keys() & findings.keys():
        raise ValueError("discovered findings cannot replace initial targets")
    known = targets.keys() | findings.keys()
    for finding in findings.values():
        if (finding.get("parent") not in known
                or finding.get("relation") not in {"required", "related", "independent"}
                or finding.get("disposition") not in {"repair", "defer"}
                or not text(finding.get("evidence")) or not text(finding.get("tracker"))):
            raise ValueError("finding needs its parent, relation, disposition, source and tracker")
        if finding["disposition"] == "defer" and (
                not text(finding.get("reason")) or not text(finding.get("reentry"))):
            raise ValueError("deferred finding needs a reason and changed-input re-entry condition")
        seen, parent = {finding["id"]}, finding["parent"]
        while parent in findings:
            if parent in seen:
                raise ValueError("cyclic finding ancestry")
            seen.add(parent)
            parent = findings[parent].get("parent")
        if parent not in targets:
            raise ValueError("finding ancestry must end at an initial target")
        if finding["disposition"] == "repair":
            parent_finding = findings.get(finding["parent"])
            if (finding["relation"] == "independent"
                    or (parent_finding and (finding["relation"] != "required"
                                           or parent_finding["disposition"] != "repair"))):
                raise ValueError("only direct related repairs and their required fixes enter this session")

    proofs = keyed(session.get("main_acceptance", []), "main acceptance")
    nodes = (validation or {}).get("nodes", {})
    sources = {n["id"]: n for n in (graph or {}).get("nodes", [])}
    unresolved_states = {"BLOCKED", "VALIDATION_FAILED", "VALIDATION_PENDING"}
    accepted = set()
    unverified = {}
    for key, proof in proofs.items():
        if key not in known or not SHA.fullmatch(proof.get("merge_sha", "")):
            raise ValueError("main acceptance needs a known target and full merge SHA")
        gate = proof.get("validation_node")
        source, current = sources.get(gate, {}), nodes.get(gate, {})
        if key in findings and findings[key]["disposition"] == "defer":
            unverified[key] = "finding remains deferred; reconcile its disposition first"
        elif (proof.get("verified_on_main") is not True or proof.get("all_acceptance_passed") is not True
                or not text(proof.get("evidence"))):
            unverified[key] = "full actual-main acceptance evidence missing"
        elif (not text(gate) or source.get("kind") != "gate" or source.get("subject") != "main"
              or source.get("revision") != proof["merge_sha"]
              or (graph or {}).get("main_revision") != proof["merge_sha"]
              or (current_main or {}).get("sha") != proof["merge_sha"]):
            unverified[key] = "receipt is not bound to the current actual-main gate"
        elif (current_main or {}).get("verified_checks") is not True:
            unverified[key] = "current main check inventory remains unverified"
        elif current.get("state") != "DELIVERABLE":
            unverified[key] = "current main validation is not DELIVERABLE"
        elif nodes.get(key, {}).get("state") in unresolved_states:
            unverified[key] = "current target validation remains unresolved"
        else:
            accepted.add(key)
    repairs = {key for key, f in findings.items() if f["disposition"] == "repair"}
    blocked = {}
    for key in sorted(repairs):
        row = nodes.get(key)
        if not row or row.get("state") == "BLOCKED" or row.get("execution_ready") is not True:
            blocked[key] = row.get("reasons", []) if row else ["reviewed validation node missing"]
            accepted.discard(key)

    # A recorded deferral cannot turn an unmet acceptance dependency into success.
    changed = True
    while changed:
        changed = False
        for key, finding in findings.items():
            if (finding["relation"] == "required" and key not in accepted
                    and finding["parent"] in accepted):
                accepted.remove(finding["parent"])
                changed = True
    required = sorted(key for key, f in findings.items()
                      if f["relation"] == "required" and key not in accepted
                      and (f["parent"] in targets or f["parent"] in repairs))
    verified_targets = sorted(targets.keys() & accepted)
    verified_repairs = sorted(repairs & accepted)
    deferred = {key: {name: f[name] for name in ("reason", "reentry", "evidence", "tracker")}
                for key, f in findings.items() if f["disposition"] == "defer"}
    return {
        "requested_count": count, "selected_count": len(targets),
        "unfilled_count": count - len(targets), "initial_verified_on_main": verified_targets,
        "initial_remaining": sorted(targets.keys() - accepted),
        "same_session_repairs": sorted(repairs), "pending_repairs": sorted(repairs - accepted),
        "repairs_verified_on_main": verified_repairs, "blocked_repairs": blocked,
        "deferred_findings": deferred, "unresolved_required_findings": required,
        "unverified_receipts": unverified,
        "complete": len(verified_targets) == count and len(verified_repairs) == len(repairs)
                    and not required and not blocked,
        "authority": "reviewed evidence only; never dispatch, merge, close issues or clear a hold",
    }

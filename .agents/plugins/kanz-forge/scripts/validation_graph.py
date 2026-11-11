"""Recalculate delivery readiness from reviewed dependencies and current evidence.

No network, persistence, dispatch, merge or production authority. Edges describe
actual validation dependencies; an unrelated red check is never a global lock.
"""
import re

STATES = {"READY", "IN_PROGRESS", "BLOCKED", "VALIDATION_FAILED",
          "VALIDATION_PENDING", "DELIVERABLE"}
KINDS = {"task", "pr", "gate", "runtime"}
PENDING = {"QUEUED", "PENDING", "IN_PROGRESS", "WAITING", "REQUESTED"}
SHA = re.compile(r"[0-9a-f]{40}")


def proof(value):
    return isinstance(value, str) and bool(value.strip())


def validation(node):
    data = node.get("validation")
    if not data:
        return None, ["validation not observed"], []
    revision = node.get("revision", "")
    if not SHA.fullmatch(revision) or data.get("sha") != revision:
        return "BLOCKED", ["validation revision is missing or stale"], []
    expected, observed = data.get("expected", []), data.get("observed", [])
    names = [item["id"] for item in expected]
    if not names or len(set(names)) != len(names):
        return "BLOCKED", ["expected check inventory is missing or ambiguous"], []
    rows = []
    for item in expected:
        matches = [row for row in observed if row.get("id") == item["id"]
                   and row.get("sha") == revision]
        row = matches[0] if len(matches) == 1 else {}
        status = row.get("status")
        evidence = row.get("evidence")
        if len(matches) != 1 or not proof(evidence):
            state = "unavailable"
        elif status == "SUCCESS":
            state = "passed"
        elif status == "SKIPPED" and proof(item.get("skip_reason")):
            state = "expected-skip"
        elif status in PENDING:
            state = "pending"
        else:
            state = "failed"
        rows.append({"id": item["id"], "state": state, "evidence": evidence})
    if any(row["state"] == "failed" for row in rows):
        return "VALIDATION_FAILED", ["required validation failed"], rows
    if any(row["state"] == "unavailable" for row in rows):
        return "BLOCKED", ["required check evidence is missing, stale or ambiguous"], rows
    if any(row["state"] == "pending" for row in rows):
        return "VALIDATION_PENDING", ["required validation is pending"], rows
    if data.get("inventory_complete") is not True or not proof(data.get("inventory_evidence")):
        return "BLOCKED", ["applicable check inventory has not been reviewed"], rows
    return "DELIVERABLE", [], rows


def assess_graph(graph):
    nodes = {}
    for node in graph["nodes"]:
        key = node["id"]
        if not proof(key) or key in nodes or node.get("kind") not in KINDS:
            raise ValueError("graph needs unique node IDs and supported kinds")
        nodes[key] = node
    if len(nodes) > 2000 or len(graph["edges"]) > 10000:
        raise ValueError("validation graph exceeds its bounded scope")
    required = {key: [] for key in nodes}
    execution = {key: [] for key in nodes}
    recovers = {key: [] for key in nodes}
    for edge in graph["edges"]:
        source, target = edge["from"], edge["to"]
        if source not in nodes or target not in nodes or not proof(edge.get("evidence")):
            raise ValueError("dependency must resolve both nodes and carry source evidence")
        relation = edge["kind"]
        if relation not in {"requires", "execution-requires", "recovers"}:
            raise ValueError("unknown validation relation")
        if relation == "recovers":
            if nodes[source]["kind"] != "task" or nodes[target]["kind"] not in {"gate", "runtime"}:
                raise ValueError("recovery must connect a task to a gate or runtime")
            recovers[source].append(target)
        else:
            required[source].append(target)
            if relation == "execution-requires":
                execution[source].append(target)
    # Iterative topological traversal avoids recursion on source-controlled input.
    waiting = {key: set(value) for key, value in required.items()}
    order, ready = [], sorted(key for key, deps in waiting.items() if not deps)
    dependents = {key: [] for key in nodes}
    for key, deps in waiting.items():
        for dependency in deps:
            dependents[dependency].append(key)
    while ready:
        key = ready.pop(0)
        order.append(key)
        for consumer in dependents[key]:
            waiting[consumer].discard(key)
            if not waiting[consumer]:
                ready.append(consumer)
    if len(order) != len(nodes):
        raise ValueError("cyclic validation dependency; readiness cannot be established")
    results = {}
    for key in order:
        node = nodes[key]
        state, reasons, rows = validation(node)
        blocked = [dep for dep in required[key] if results[dep]["state"] != "DELIVERABLE"]
        causes = set()
        for dep in blocked:
            causes.update(results[dep]["root_blockers"] or [dep])
        holds = node.get("holds", [])
        if any(not proof(item.get("evidence")) or not proof(item.get("reason")) for item in holds):
            raise ValueError("each hold needs a reason and evidence")
        if holds:
            state = "BLOCKED"
            reasons.extend(item["reason"] for item in holds)
            causes.add(key)
        elif blocked:
            # Own failure remains visible even when a dependency also failed.
            if state != "VALIDATION_FAILED":
                state = "BLOCKED"
            reasons.extend("requires " + dep + " (" + results[dep]["state"] + ")" for dep in blocked)
        elif state is None:
            stage = node.get("stage", {})
            if stage.get("state") in {"READY", "IN_PROGRESS"} and proof(stage.get("evidence")):
                state = stage["state"]
                reasons = [stage["evidence"]]
            else:
                state = "BLOCKED"
                reasons = ["no evidenced engineering stage or validation result"]
        if state == "DELIVERABLE" and node["kind"] in {"pr", "task"}:
            review = node.get("review", {})
            if (review.get("passed") is not True or review.get("sha") != node.get("revision")
                    or not proof(review.get("evidence"))):
                state = "BLOCKED"
                reasons.append("current-revision review evidence is missing")
                causes.add(key)
            if node["kind"] == "pr":
                integration = node.get("integration", {})
                if (not SHA.fullmatch(graph.get("main_revision", ""))
                        or integration.get("base_sha") != graph.get("main_revision")
                        or integration.get("head_sha") != node.get("revision")
                        or not proof(integration.get("evidence"))):
                    state = "BLOCKED"
                    reasons.append("current-main integration evidence is missing")
                    causes.add(key)
        if state == "VALIDATION_FAILED":
            causes.add(key)
        if state in {"BLOCKED", "VALIDATION_FAILED", "VALIDATION_PENDING"} and not causes:
            causes.add(key)
        execution_ready = not holds and all(results[dep]["state"] == "DELIVERABLE" for dep in execution[key])
        # Missing task scope is not permission to start or to invent dependencies.
        if node["kind"] == "task" and not proof(node.get("scope_evidence")):
            execution_ready = False
            state = "BLOCKED"
            reasons.append("task scope/dependency review is missing")
            causes.add(key)
        results[key] = {"state": state, "reasons": reasons, "checks": rows,
                        "execution_ready": execution_ready,
                        "root_blockers": sorted(causes), "requires": sorted(required[key])}
    failures = {}
    for key, row in results.items():
        if row["state"] not in {"VALIDATION_FAILED", "BLOCKED"}:
            continue
        cause = nodes[key].get("failure")
        if cause:
            if (not proof(cause.get("id")) or not proof(cause.get("evidence"))
                    or cause.get("kind") not in {"code", "infrastructure", "security", "dependency", "permissions", "external", "unknown"}):
                raise ValueError("root failure needs classification and reviewed evidence")
            group = failures.setdefault(cause["id"], {"kind": cause["kind"], "nodes": [], "evidence": []})
            if group["kind"] != cause["kind"]:
                raise ValueError("one root cause cannot have conflicting classifications")
            group["nodes"].append(key)
            group["evidence"].append(cause["evidence"])
    candidates = []
    execution_only = []
    for key, node in nodes.items():
        if node["kind"] != "task" or not results[key]["execution_ready"]:
            continue
        if results[key]["state"] in {"VALIDATION_FAILED", "VALIDATION_PENDING", "DELIVERABLE"}:
            continue
        affected = set()
        for gate in recovers[key]:
            if results[gate]["state"] == "DELIVERABLE":
                continue
            affected.add(gate)
            affected.update(consumer for consumer, row in results.items() if gate in row["root_blockers"])
        if results[key]["state"] == "BLOCKED":
            execution_only.append(key)
            results[key]["selection_reason"] = "coding may proceed; do not enqueue another undeliverable feature"
            continue
        critical = any(nodes[gate].get("critical") is True for gate in recovers[key] if gate in affected)
        candidates.append((not critical, not bool(affected), node.get("priority", 2), -len(affected), key))
        results[key]["selection_reason"] = (
            "recover critical shared validation prerequisite" if critical else
            "recover shared validation prerequisite" if affected else
            "independent execution; delivery prerequisites remain explicit")
    candidates.sort()
    selected = candidates[0][-1] if candidates else None
    previous = graph.get("previous_states", {})
    transitions = [{"id": key, "from": previous[key], "to": row["state"],
                    "evidence": row["checks"] or row["reasons"]}
                   for key, row in results.items() if key in previous and previous[key] != row["state"]]
    if any(value not in STATES for value in previous.values()):
        raise ValueError("invalid previous delivery state")
    return {"nodes": results, "failure_groups": failures, "next_actionable": selected,
            "execution_candidates": [row[-1] for row in candidates], "execution_only": sorted(execution_only), "transitions": transitions,
            "reassessment_required": bool(transitions), "authority": "advisory; never merge or production permission"}

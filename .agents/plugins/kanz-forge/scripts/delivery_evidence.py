"""Reduce a reviewed delivery snapshot. Reads JSON from stdin; writes JSON only."""
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import PurePosixPath
from statistics import median
from issue_backlog import summarize as summarize_backlog
from validation_graph import assess_graph

PATHS = {
    "code": ["Fix the owning code and run its narrow regression", "Use an isolated baseline comparison"],
    "infrastructure": ["Verify existing local prerequisites", "Use an eligible existing runner; price and seek approval before provisioning"],
    "security": ["Qualify a reviewed fix with the unchanged gate", "Escalate to the security owner for a supported fix"],
    "dependency": ["Use a verified upstream release", "Qualify a reproducible build from verified upstream inputs"],
    "permissions": ["Ask the owner to run the read-only check", "Request the minimum scoped access"],
    "external": ["Escalate with the missing observation", "Keep independent work moving while the provider resolves it"],
}
PENDING = {"QUEUED", "PENDING", "IN_PROGRESS", "WAITING", "REQUESTED"}
SHA = re.compile(r"[0-9a-f]{40}")


def checks(sha, expected, observed):
    """Expected includes every applicable context and reviewed skip policy."""
    if not SHA.fullmatch(sha) or not expected or len({e["id"] for e in expected}) != len(expected):
        raise ValueError("need a full SHA and a nonempty, unique expected check inventory")
    rows = []
    for item in expected:
        name = item["id"]
        matches = [c for c in observed if c["id"] == name and c["sha"] == sha]
        evidence = matches[0] if len(matches) == 1 else {}
        status = evidence.get("status")
        if not matches:
            state = "stale" if any(c["id"] == name for c in observed) else "missing"
        elif len(matches) > 1:
            state = "ambiguous"
        elif status == "SUCCESS":
            state = "passed"
        elif status == "SKIPPED" and item.get("skip_reason"):
            state = "expected-skip"
        elif status in PENDING:
            state = "pending"
        else:
            state = "failed"
        row = {"id": name, "state": state}
        if state == "failed":
            # Never infer a cause from a status, workflow name, or log keyword.
            kind = evidence.get("kind")
            diagnosed = kind in PATHS and evidence.get("cause") and evidence.get("evidence")
            row.update(kind=kind if diagnosed else "unknown",
                       cause=evidence["cause"] if diagnosed else None,
                       evidence=evidence.get("evidence"),
                       next=PATHS[kind] if diagnosed else ["Inspect the first failed step and compare current main before retrying"])
        rows.append(row)
    return rows


def green(rows):
    return all(row["state"] in {"passed", "expected-skip"} for row in rows)


def blocker_report(blockers):
    result = {}
    for key, blocker in blockers.items():
        kind = blocker["kind"]
        if kind not in PATHS or not blocker["cause"] or not blocker["evidence"]:
            raise ValueError(f"blocker {key} needs a classification, root cause and evidence")
        result[key] = dict(blocker, next=PATHS[kind])
    return result


def paths(task):
    result = []
    for raw in task["paths"]:
        path = PurePosixPath(raw.replace("\\", "/").casefold())
        if path.is_absolute() or ".." in path.parts or not path.parts or ":" in raw:
            raise ValueError("use repository-relative file or directory ownership")
        result.append(path)
    return result


def plan(tasks, completed, active, rank=None):
    identifiers = [task["id"] for task in (*tasks, *active)]
    if len(set(identifiers)) != len(identifiers):
        raise ValueError("task ownership requires unique IDs")
    selected = []
    deferred = {}
    claimed = [(task, paths(task)) for task in active]
    rank = rank or {}
    for task in sorted(tasks, key=lambda item: (rank.get(item["id"], len(tasks)), item["priority"], item["id"])):
        owned = paths(task)
        missing = sorted(set(task["depends_on"]) - completed)
        overlap = [other["id"] for other, scope in claimed
                   if set(other["resources"]) & set(task["resources"])
                   or any(a == b or a in b.parents or b in a.parents for a in owned for b in scope)]
        reasons = []
        if missing:
            reasons.append({"dependencies": missing})
        if task["blockers"]:
            reasons.append({"blockers": task["blockers"]})
        if not task["owner"] or not owned:
            reasons.append("missing-owner-or-scope")
        if overlap:
            reasons.append({"overlap": overlap})
        if reasons:
            deferred[task["id"]] = reasons
        else:
            selected.append(task["id"])
            claimed.append((task, owned))
    return {"next_actionable": selected[0] if selected else None,
            "independent": selected, "deferred": deferred}


def timestamp(value):
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.utcoffset() is None:
        raise ValueError("report timestamps require a timezone")
    return parsed.astimezone(timezone.utc)


def daily_progress(period, blockers):
    start, end = timestamp(period["start"]), timestamp(period["end"])
    if start >= end:
        raise ValueError("report window must have a positive duration")
    ages = {}
    for key, blocker in blockers.items():
        since = timestamp(blocker["blocked_since"]) if blocker.get("blocked_since") else None
        if since and since > end:
            raise ValueError(f"blocker {key} starts after the report")
        ages[key] = {"owner": blocker.get("owner"),
                     "age_hours": (end - since).total_seconds() / 3600 if since else None}

    merged, verified, cycle_hours, seen = [], [], [], set()
    for item in period["merges"]:
        number = item["number"]
        if number in seen or not SHA.fullmatch(item["merge_sha"]):
            raise ValueError("merge records require a unique PR and full SHA")
        seen.add(number)
        opened, merged_at = timestamp(item["opened_at"]), timestamp(item["merged_at"])
        if not opened <= merged_at <= end:
            raise ValueError("invalid PR lifecycle timestamps")
        if start <= merged_at < end:
            merged.append(number)
            cycle_hours.append((merged_at - opened).total_seconds() / 3600)
        proof = item.get("main_verification")
        if proof:
            checked = timestamp(proof["verified_at"])
            if checked < merged_at:
                raise ValueError("main verification predates merge")
            if start <= checked < end and proof["sha"] == item["merge_sha"] and proof["evidence"] and proof["acceptance_passed"] is True:
                verified.append(number)
    return {"merged": sorted(merged), "verified_on_main": sorted(verified),
            "median_pr_cycle_hours": median(cycle_hours) if cycle_hours else None,
            "open_blockers": ages}


def build_plan(builders):
    requirements = ("platform", "capacity", "inputs", "isolation")
    local = [{"id": b["id"], "missing": [key for key in requirements if b[key] is not True]
              + ([] if b["probe_evidence"] else ["probe-evidence"])}
             for b in builders if b["existing_local"] is True]
    verified = next((b["id"] for b in local if not b["missing"]), None)
    if verified:
        next_step = "use verified local builder"
    elif any(b["missing"] == ["inputs"] for b in local):
        next_step = "verify the missing inputs; moving the build does not supply them"
    else:
        next_step = "resolve missing local proof; then evaluate existing runner or AWS cost and permissions"
    return {"verified_local": verified, "local_candidates": local, "next": next_step}


def assess(snapshot):
    main = snapshot["main"]
    completed = set(snapshot["verified_dependencies"])
    main_checks = checks(main["sha"], main["expected"], main["observed"])
    queue = []
    for pr in sorted(snapshot["prs"], key=lambda item: (item["risk"], item["number"])):
        rows = checks(pr["sha"], pr["expected"], pr["observed"])
        reasons = []
        if pr["base_sha"] != main["sha"]:
            reasons.append("base-changed")
        if pr["draft"] or pr["merge_state"] != "CLEAN":
            reasons.append("review-or-merge-state")
        if pr["depends_on"] and set(pr["depends_on"]) - completed:
            reasons.append("unverified-dependencies")
        if pr["blockers"]:
            reasons.append("open-blockers")
        if pr.get("inventory_complete") is not True:
            reasons.append("incomplete-check-inventory")
        if not green(rows):
            reasons.append("checks")
        queue.append({"number": pr["number"], "sha": pr["sha"], "candidate": not reasons,
                      "reasons": reasons, "checks": rows})
    result = {"main": {"sha": main["sha"],
                     "verified_checks": main.get("inventory_complete") is True and green(main_checks),
                     "checks": main_checks},
            "queue": queue,
            "blockers": blocker_report(snapshot.get("blockers", {})),
            "plan": plan(snapshot["tasks"], completed, snapshot["active_tasks"]),
            "build": build_plan(snapshot["builders"])}
    if "period" in snapshot:
        result["daily_progress"] = daily_progress(snapshot["period"], result["blockers"])
    if "backlog" in snapshot:
        result["issue_backlog"] = summarize_backlog(snapshot["backlog"])
    if "validation_graph" in snapshot:
        result["validation"] = assess_graph(snapshot["validation_graph"])
        validation = result["validation"]
        if any(task["id"] not in validation["nodes"] for task in snapshot["tasks"]):
            raise ValueError("every queued task needs a reviewed validation node")
        rank = {key: i for i, key in enumerate(validation["execution_candidates"])}
        eligible = [task for task in snapshot["tasks"] if task["id"] in rank]
        result["plan"] = plan(eligible, completed, snapshot["active_tasks"], rank)
        for task in snapshot["tasks"]:
            if task["id"] not in rank:
                result["plan"]["deferred"][task["id"]] = validation["nodes"][task["id"]]["reasons"]
        result["plan"]["validation_aware"] = True
    else:
        result["plan"]["ownership_candidate"] = result["plan"]["next_actionable"]
        result["plan"]["next_actionable"] = None
        result["plan"]["validation_aware"] = False
        result["plan"]["limitation"] = "legacy ownership advice only; validation dependencies unverified"
    return result


if __name__ == "__main__":
    try:
        print(json.dumps(assess(json.load(sys.stdin)), indent=2))
    except (KeyError, TypeError, ValueError) as error:
        sys.exit(f"invalid delivery evidence: {error}")

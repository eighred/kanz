"""Summarize the reviewed open-issue inventory without mutating GitHub."""
from collections import Counter
from datetime import datetime, timezone
import re

STATUSES = ("ready to implement", "blocked external", "waiting on dependency",
            "needs investigation", "ready for verification or closure")


def date(value):
    result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if result.utcoffset() is None:
        raise ValueError("issue timestamps require a timezone")
    return result.astimezone(timezone.utc)


def summarize(backlog):
    """Input is a reconciled snapshot; unknown evidence must stay unknown."""
    now = date(backlog["as_of"])
    rows = {}
    for item in backlog["issues"]:
        number = item["number"]
        status = item["status"]
        if number in rows or status not in STATUSES or not item["next_action"]:
            raise ValueError("issue records require unique IDs, a valid status and a next action")
        if item["priority"] not in ("P0", "P1", "P2", "untriaged"):
            raise ValueError("invalid issue priority")
        age = None
        updated = date(item["updated_at"])
        if updated > now:
            raise ValueError("issue update postdates inventory")
        if item.get("blocked_since"):
            since = date(item["blocked_since"])
            if since > now:
                raise ValueError("blocker starts after inventory")
            age = (now - since).total_seconds() / 3600
        proof = item.get("closure_proof") or {}
        closable = (status == STATUSES[4] and item["acceptance_documented"] is True
                    and proof.get("all_acceptance_passed") is True
                    and proof.get("verified_on_main") is True
                    and bool(re.fullmatch(r"[0-9a-f]{40}", proof.get("merge_sha", "")))
                    and bool(proof.get("evidence")) and not item.get("keep_open"))
        rows[number] = dict(item, blocker_age_hours=age,
                            stale_review=(now - updated).days >= 90, closure_candidate=closable)
    complete = backlog.get("inventory_complete") is True and backlog["open_total"] == len(rows)
    ready = [r for r in rows.values() if r["status"] == STATUSES[0]
             and r["acceptance_documented"] is True
             and (not r.get("active_prs") or r.get("independent_scope"))
             and not r.get("unresolved_dependencies")]
    ready.sort(key=lambda r: (r["priority"], r.get("order", 1000), r["number"]))
    counts = {key: dict(Counter(r[key] for r in rows.values()))
              for key in ("status", "priority", "component", "security_impact")}
    return {
        "inventory_complete": complete,
        "open_total": backlog["open_total"],
        "reviewed_count": len(rows),
        "counts": counts,
        "blocked": sorted(r["number"] for r in rows.values() if r["status"] in STATUSES[1:3]),
        "active_prs": sorted(r["number"] for r in rows.values() if r.get("active_prs")),
        "awaiting_verification": sorted(r["number"] for r in rows.values()
                                        if (r["status"] == STATUSES[4] or r.get("landed_scope_evidence"))
                                        and not r["closure_candidate"]),
        "closure_candidates": sorted(r["number"] for r in rows.values() if r["closure_candidate"]) if complete else [],
        "top_five": [r["number"] for r in ready[:5]],
        "issues": [rows[k] for k in sorted(rows)],
    }

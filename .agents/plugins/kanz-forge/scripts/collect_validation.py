"""Collect current GitHub delivery observations through read-only gh API calls.

Print JSON only. No mutation, credentials, logs, cache, persistent state or host
configuration is read into the result. A reviewed contract supplies dependency
edges and expected checks; observation alone never asserts complete coverage.
"""
import argparse
import copy
import datetime
import json
import re
import subprocess
import sys
from urllib.parse import quote

from validation_graph import assess_graph

REPO = re.compile(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+")


def github(path):
    # gh resolves its own credential. Never obtain or print the token.
    process = subprocess.run(["gh", "api", "--method", "GET", "--paginate", "--slurp", path],
                             capture_output=True, text=True, encoding="utf-8", timeout=90)
    if process.returncode:
        # Do not echo transport output that could contain URLs or credentials.
        raise ValueError("GitHub read unavailable: " + path.split("?", 1)[0])
    return json.loads(process.stdout)


def objects(api, path):
    pages = api(path)
    if not isinstance(pages, list) or not pages:
        raise ValueError("missing GitHub response")
    return pages


def collection(api, path, key=None):
    pages = objects(api, path)
    result = []
    for page in pages:
        rows = page.get(key) if key else page
        if not isinstance(rows, list):
            raise ValueError("malformed GitHub collection")
        result.extend(rows)
    if key and any(page.get("total_count") != len(result) for page in pages):
        raise ValueError("incomplete or changing GitHub collection")
    return result


def check_rows(records, sha):
    rows = []
    for check in records:
        if check.get("head_sha") != sha:
            raise ValueError("GitHub returned a check for another revision")
        workflow = check.get("details_url", check.get("html_url", ""))
        rows.append({"id": check["name"], "sha": sha,
                     "status": (check.get("conclusion") or check["status"]).upper(),
                     "evidence": check.get("html_url") or workflow,
                     "check_id": check["id"], "started_at": check.get("started_at"),
                     "completed_at": check.get("completed_at")})
    return rows


def collect(repo, api=github, contract=None):
    if not REPO.fullmatch(repo) or ".." in repo:
        raise ValueError("expected an owner/repository identity")
    base = "repos/" + repo
    metadata = objects(api, base)[0]
    branch = quote(metadata["default_branch"], safe="")
    main = objects(api, base + "/branches/" + branch)[0]["commit"]["sha"]
    prs = collection(api, base + "/pulls?state=open&per_page=100")
    issues = collection(api, base + "/issues?state=open&per_page=100")
    # Repository count includes PRs. A changing inventory must be recollected.
    if metadata["open_issues_count"] != len(issues):
        raise ValueError("open-issue inventory differs from GitHub total")
    observations = {}
    failures = []
    for key, sha in [("main", main)] + [("pr:" + str(pr["number"]), pr["head"]["sha"]) for pr in prs]:
        try:
            checks = collection(api, base + "/commits/" + sha + "/check-runs?filter=latest&per_page=100", "check_runs")
            rows = check_rows(checks, sha)
            observations[key] = {"sha": sha, "observed": rows, "available": True}
        except (ValueError, OSError, subprocess.SubprocessError):
            observations[key] = {"sha": sha, "observed": [], "available": False}
        failures.extend({"subject": key, **row} for row in observations[key]["observed"]
                        if row["status"] not in {"SUCCESS", "SKIPPED", "NEUTRAL"})
    if objects(api, base + "/branches/" + branch)[0]["commit"]["sha"] != main:
        raise ValueError("main moved during collection; reassess on a fresh snapshot")
    latest_prs = collection(api, base + "/pulls?state=open&per_page=100")
    if {p["number"]: p["head"]["sha"] for p in latest_prs} != {p["number"]: p["head"]["sha"] for p in prs}:
        raise ValueError("PR inventory or heads changed during collection")
    result = {"repository": repo, "main": main,
              "collected_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "issues": [{"number": i["number"], "title": i["title"], "url": i["html_url"],
                          "labels": [label["name"] for label in i["labels"]]} for i in issues if not i.get("pull_request")],
              "prs": [{"number": p["number"], "head": p["head"]["sha"], "base": p["base"]["sha"],
                       "draft": p["draft"], "url": p["html_url"]} for p in prs],
              "observations": observations, "unresolved_checks": failures,
              "limitation": "check observations do not establish applicability, root cause, review or merge authority"}
    if contract:
        graph = copy.deepcopy(contract)
        if graph.get("main_revision") != main:
            raise ValueError("dependency contract main revision is missing or stale; review it again")
        for node in graph["nodes"]:
            subject = node.get("subject")
            if (node.get("kind") == "pr" or
                    (node.get("kind") == "gate" and "validation" in node)):
                if not isinstance(subject, str) or not subject:
                    raise ValueError("PR/check gate needs a live GitHub subject")
            if node.get("kind") == "pr" and not re.fullmatch(r"pr:[1-9][0-9]*", subject):
                raise ValueError("PR node needs a live PR subject")
            if subject:
                if subject not in observations:
                    raise ValueError("contract subject is no longer in the live inventory")
                observed = observations[subject]
                if node.get("revision") != observed["sha"]:
                    raise ValueError("dependency contract revision is stale; review it again")
                node.setdefault("validation", {}).update(sha=observed["sha"], observed=observed["observed"])
        result["validation_graph"] = graph
        result["assessment"] = assess_graph(graph)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", default="eighred/kanz")
    parser.add_argument("--contract", help="reviewed dependency graph JSON outside the repository's project-state surface")
    args = parser.parse_args()
    try:
        contract = None
        if args.contract:
            with open(args.contract, encoding="utf-8") as source:
                contract = json.load(source)
        print(json.dumps(collect(args.repo, contract=contract), indent=2))
        return 0
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        print("validation collection incomplete: " + str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

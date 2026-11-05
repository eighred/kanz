"""Exact-commit release evidence from canonical GitHub Actions workflows.

Only read-only REST endpoints are used. No check-name search, ancestor fallback,
or successful older attempt can confer release authority.
"""
import json
from pathlib import Path
import urllib.error
import urllib.parse
import urllib.request

REPO = "eighred/kanz"
API = "https://api.github.com/repos/" + REPO
POLICY = {
    "kanz-ci.yml": {"kanz-ci-required"},
    "architecture-migration.yml": {"compatibility"},
    "julia-calibration.yml": {"shadow-calibration"},
    "rust-bond-shadow.yml": {"bond-shadow"},
    "rust-oms-fold.yml": {"fill-shadow"},
    "feed-identity-proof.yml": {"developmental-identity"},
    "testnet-cni-proof.yml": {"disposable-cni"},
    "redpanda-identity-proof.yml": {"developmental-redpanda-identity"},
}


class EvidenceError(Exception):
    """Required exact-source proof is absent or cannot be trusted."""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise EvidenceError("GitHub API redirect refused")


class GitHub:
    def __init__(self, token):
        if not token:
            raise EvidenceError("read-only GitHub token required")
        self.token = token
        self.opener = urllib.request.build_opener(NoRedirect())

    def get(self, path, **query):
        # Callers supply fixed endpoint templates and validated integer IDs.
        url = API + path + ("?" + urllib.parse.urlencode(query) if query else "")
        request = urllib.request.Request(url, headers={
            "Authorization": "Bearer " + self.token,
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2026-03-10",
        })
        try:
            with self.opener.open(request, timeout=30) as response:
                raw = response.read((8 << 20) + 1)
            if len(raw) > 8 << 20:
                raise EvidenceError("GitHub API response exceeds bound")
            value = json.loads(raw)
            if not isinstance(value, dict):
                raise EvidenceError("GitHub API object required")
            return value
        except (urllib.error.URLError, OSError, ValueError) as error:
            # Never print headers, tokens, response bodies or network diagnostics.
            raise EvidenceError("GitHub evidence request failed") from None


def positive(value):
    return type(value) is int and value > 0


def pages(api, path, key, **query):
    found, ids, total = [], set(), None
    for page in range(1, 11):
        data = api.get(path, per_page=100, page=page, **query)
        count, items = data.get("total_count"), data.get(key)
        if type(count) is not int or not 0 <= count < 1000 or not isinstance(items, list):
            raise EvidenceError("incomplete or oversized GitHub evidence inventory")
        if total is not None and count != total:
            raise EvidenceError("GitHub evidence changed during pagination")
        total = count
        for item in items:
            if not isinstance(item, dict) or not positive(item.get("id")) or item["id"] in ids:
                raise EvidenceError("invalid or duplicate GitHub evidence record")
            ids.add(item["id"])
            found.append(item)
        if len(found) == total:
            return found
        if len(found) > total or not items:
            raise EvidenceError("truncated GitHub evidence inventory")
    raise EvidenceError("GitHub evidence pagination bound exceeded")


def requirements(root):
    directory = Path(root) / ".github/workflows"
    result = {}
    for filename, jobs in POLICY.items():
        path = directory / filename
        if filename == "kanz-ci.yml" or path.exists():
            if path.is_symlink() or not path.is_file():
                raise EvidenceError("required workflow is not a regular source file")
            result[filename] = jobs
    return result


def run_identity(run, workflow_id, path, sha, repo_id):
    if (run.get("workflow_id") != workflow_id or run.get("path") != path
            or run.get("head_sha") != sha or run.get("head_branch") != "main"
            or run.get("event") not in {"push", "workflow_dispatch"}
            or not positive(run.get("id")) or not positive(run.get("run_number"))
            or not positive(run.get("run_attempt"))):
        raise EvidenceError("workflow run identity does not match release source")
    for key in ("repository", "head_repository"):
        repository = run.get(key)
        if not isinstance(repository, dict) or repository.get("id") != repo_id or repository.get("full_name") != REPO:
            raise EvidenceError("workflow run repository does not match canonical repository")


def verify_ci(api, sha, root="."):
    repository = api.get("")
    repo_id = repository.get("id")
    if repository.get("full_name") != REPO or not positive(repo_id):
        raise EvidenceError("canonical repository identity unavailable")
    evidence = []
    for filename, required in requirements(root).items():
        path = ".github/workflows/" + filename
        workflow = api.get("/actions/workflows/" + filename)
        wid = workflow.get("id")
        if (not positive(wid) or workflow.get("path") != path or workflow.get("state") != "active"
                or workflow.get("url") != f"{API}/actions/workflows/{wid}"):
            raise EvidenceError("canonical workflow identity unavailable: " + filename)
        runs = pages(api, f"/actions/workflows/{wid}/runs", "workflow_runs", head_sha=sha, branch="main")
        if not runs:
            raise EvidenceError("missing exact-source workflow proof: " + filename)
        for run in runs:
            run_identity(run, wid, path, sha, repo_id)
        latest = max(runs, key=lambda r: (r["run_number"], r["id"]))
        endpoint = f"/actions/runs/{latest['id']}"
        current = api.get(endpoint)
        run_identity(current, wid, path, sha, repo_id)
        if current["id"] != latest["id"] or current["run_number"] != latest["run_number"] or current["run_attempt"] < latest["run_attempt"]:
            raise EvidenceError("workflow run changed identity")
        if current.get("status") != "completed" or current.get("conclusion") != "success":
            raise EvidenceError("latest exact-source workflow has not succeeded: " + filename)
        attempt = current["run_attempt"]
        jobs = pages(api, endpoint + f"/attempts/{attempt}/jobs", "jobs")
        matched = set()
        for job in jobs:
            if job.get("run_id") != current["id"] or job.get("head_sha") != sha or job.get("run_attempt") != attempt:
                raise EvidenceError("job does not belong to the exact run attempt")
            if job.get("name") in required:
                if job["name"] in matched or job.get("status") != "completed" or job.get("conclusion") != "success":
                    raise EvidenceError("required job did not succeed uniquely: " + filename)
                matched.add(job["name"])
        if matched != required:
            raise EvidenceError("required jobs missing: " + filename)
        # Refuse a rerun begun while jobs were read. A later release rerun must
        # obtain fresh evidence; successful older attempts are never substituted.
        if api.get(endpoint) != current:
            raise EvidenceError("workflow evidence changed while checking jobs")
        refreshed = pages(api, f"/actions/workflows/{wid}/runs", "workflow_runs", head_sha=sha, branch="main")
        for run in refreshed:
            run_identity(run, wid, path, sha, repo_id)
        newest = max(refreshed, key=lambda r: (r["run_number"], r["id"])) if refreshed else {}
        if any(newest.get(key) != current.get(key) for key in ("id", "run_attempt", "status", "conclusion")):
            raise EvidenceError("newer workflow run appeared while checking evidence")
        evidence.append({"workflow": filename, "run_id": current["id"], "attempt": attempt})
    return evidence

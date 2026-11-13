"""Repository identity candidates and public Git refs, independent of language."""

import re
from datetime import datetime, timezone
from urllib.parse import quote, urlsplit

import git_refs


def initial(provider):
    return {"schema_version": 1, "status": "unknown", "provider": provider,
            "checked_at": datetime.now(timezone.utc).isoformat(), "sources": []}


def find(query, fetch, lookup_error):
    result = initial("github_search")
    try:
        if (not isinstance(query, str) or not query.strip() or len(query) > 100
                or any(ord(char) < 32 for char in query)
                or not re.fullmatch(r"[\w .-]+", query)):
            raise lookup_error("invalid_search_query")
        url = "https://api.github.com/search/repositories?q=" + quote(query.strip() + " in:name fork:false archived:false", safe="") + "&per_page=10"
        value, digest = fetch(url)
        result["sources"].append({"url": url, "sha256": digest})
        if (not isinstance(value, dict) or value.get("incomplete_results") is not False
                or not isinstance(value.get("items"), list) or len(value["items"]) > 10
                or not isinstance(value.get("total_count"), int)):
            raise lookup_error("incomplete_repository_search")
        candidates = []
        for item in value["items"]:
            name = item.get("full_name") if isinstance(item, dict) else None
            if (not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", name)
                    or ".." in name or item.get("private") is not False):
                raise lookup_error("invalid_repository_identity")
            candidates.append("https://github.com/" + name)
        result.update(status="candidates" if candidates else "unknown", candidates=candidates,
                      total_count=value["total_count"], identity_verification_required=True)
        if not candidates:
            result["reason"] = "no_public_candidate_observed"
    except lookup_error as exc:
        result["reason"] = str(exc)
    return result


def discover(value, observe, fetch, lookup_error, read=git_refs.read_refs, prefix=None):
    result = initial("git_https")
    try:
        if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", value):
            value = "https://github.com/" + value
        url = git_refs.repository_url(value)
        result["repository"] = url
        parsed = urlsplit(url)
        project = parsed.path.strip("/").removesuffix(".git")
        if parsed.hostname == "github.com" and project.count("/") == 1 and prefix is None:
            release = observe("github", project)
            if release["status"] == "observed":
                return dict(release, repository=url, revision_kind="commit")
            result["sources"].extend(release["sources"])
            if release.get("reason") != "http_404":
                raise lookup_error(release.get("reason", "release_unknown"))
            # Release absence is meaningful only after public repository identity
            # is established. Authentication/rate-limit failures never fall back.
            api = "https://api.github.com/repos/" + project
            repo, digest = fetch(api)
            result["sources"].append({"url": api, "sha256": digest})
            if (not isinstance(repo, dict) or repo.get("full_name", "").lower() != project.lower()
                    or repo.get("private") is not False):
                raise lookup_error("repository_identity_mismatch")
            result["publisher_release"] = "not_found"
        data, sources = read(url)
        result["sources"].extend(sources)
        summary = git_refs.summarize(data, prefix)
        result.update(summary)
        if prefix is not None and summary["tag_status"] == "no_numeric_tag_observed":
            result["reason"] = "tag_family_not_observed"
            return result
        if summary["tag_status"].startswith("ambiguous"):
            result["reason"] = summary["tag_status"]
            return result
        if summary["tag_status"] == "observed":
            result["selection"] = "latest_numeric_tag"
        else:
            result.update(selection="default_branch", revision=summary["head_revision"], revision_kind="commit")
        result["status"] = "observed"
    except (git_refs.RefError, lookup_error) as exc:
        result["reason"] = str(exc)
    return result

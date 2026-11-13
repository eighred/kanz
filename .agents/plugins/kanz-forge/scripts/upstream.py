#!/usr/bin/env python3
"""Observe public publisher release/ref metadata; never install or execute it."""

import argparse
import hashlib
import json
import re
import sys
from datetime import datetime, timezone
from http.client import HTTPException
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

MAX_BYTES = 2 * 1024 * 1024
TIMEOUT = 10
SHA = re.compile(r"[0-9a-f]{40}")
REPO = re.compile(r"[A-Za-z0-9_-][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9_-][A-Za-z0-9_.-]{0,99}")
REF = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/+~-]{0,199}")


class LookupError(ValueError):
    """A lookup is unknown, not evidence of upstream absence."""


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise LookupError("redirect_refused")


def public_json(url):
    # All URLs come from the two fixed publishers below. No ambient proxy,
    # credentials, provider SDK, release-body text or downloaded code is used.
    opener = build_opener(ProxyHandler({}), NoRedirect())
    request = Request(url, headers={"Accept": "application/json", "User-Agent": "kanz-forge-upstream"})
    try:
        with opener.open(request, timeout=TIMEOUT) as response:
            data = response.read(MAX_BYTES + 1)
    except HTTPError as exc:
        raise LookupError(f"http_{exc.code}") from None
    except (URLError, OSError, HTTPException):
        raise LookupError("network_unavailable") from None
    if len(data) > MAX_BYTES:
        raise LookupError("response_too_large")
    try:
        value = json.loads(data)
    except (ValueError, UnicodeError):
        raise LookupError("invalid_json") from None
    return value, hashlib.sha256(data).hexdigest()


def observe(provider, project=None, tag=None, default_branch=False, fetch=public_json):
    result = {"schema_version": 1, "status": "unknown", "provider": provider,
              "checked_at": datetime.now(timezone.utc).isoformat(), "sources": []}

    def get(url):
        value, digest = fetch(url)
        result["sources"].append({"url": url, "sha256": digest})
        return value

    try:
        if provider == "go":
            if project or tag or default_branch:
                raise LookupError("invalid_selection")
            result.update(project="go", selection="latest_stable")
            releases = get("https://go.dev/dl/?mode=json")
            if not isinstance(releases, list):
                raise LookupError("invalid_release_metadata")
            versions = []
            for release in releases:
                if not isinstance(release, dict):
                    raise LookupError("invalid_release_metadata")
                match = re.fullmatch(r"go([0-9]+)\.([0-9]+)\.([0-9]+)", str(release.get("version")))
                if release.get("stable") is True and match:
                    versions.append((tuple(map(int, match.groups())), release["version"]))
            if not versions:
                raise LookupError("no_stable_release_observed")
            version = max(versions)[1]
            result.update(version=version, release_url=f"https://go.dev/doc/devel/release#{version}")
        elif provider == "github":
            if not isinstance(project, str) or not REPO.fullmatch(project) or ".." in project:
                raise LookupError("invalid_repository")
            if tag and (not REF.fullmatch(tag) or ".." in tag):
                raise LookupError("invalid_tag")
            if tag and default_branch:
                raise LookupError("invalid_selection")
            result["project"] = project
            api = f"https://api.github.com/repos/{project}"
            web = f"https://github.com/{project}"
            if default_branch:
                result["selection"] = "default_branch"
                repo = get(api)
                if (not isinstance(repo, dict) or repo.get("full_name", "").lower() != project.lower()
                        or repo.get("private") is not False):
                    raise LookupError("repository_identity_mismatch")
                branch = repo.get("default_branch")
                if not isinstance(branch, str) or not REF.fullmatch(branch) or ".." in branch:
                    raise LookupError("invalid_default_branch")
                ref = get(f"{api}/git/ref/heads/{quote(branch, safe='')}")
                expected_ref = "refs/heads/" + branch
                result["branch"] = branch
            else:
                result["selection"] = "exact_release" if tag else "publisher_latest_release"
                release = get(f"{api}/releases/" + ("tags/" + quote(tag, safe="") if tag else "latest"))
                if (not isinstance(release, dict) or release.get("draft") is not False
                        or release.get("prerelease") is not False):
                    raise LookupError("no_stable_release_observed")
                version = release.get("tag_name")
                if (not isinstance(version, str) or not REF.fullmatch(version) or ".." in version
                        or (tag and version != tag)):
                    raise LookupError("release_identity_mismatch")
                result.update(version=version, release_url=f"{web}/releases/tag/{quote(version, safe='')}")
                ref = get(f"{api}/git/ref/tags/{quote(version, safe='')}")
                expected_ref = "refs/tags/" + version
            if not isinstance(ref, dict) or ref.get("ref") != expected_ref:
                raise LookupError("ref_identity_mismatch")
            obj = ref.get("object")
            # Annotated tags can point at tags. Bound peeling and never follow
            # an object URL supplied by the response to another origin.
            for depth in range(6):
                if (not isinstance(obj, dict) or not isinstance(obj.get("sha"), str)
                        or not SHA.fullmatch(obj["sha"])):
                    raise LookupError("invalid_object_identity")
                if obj.get("type") == "commit":
                    result.update(revision=obj["sha"], source_url=f"{web}/tree/{obj['sha']}")
                    break
                if obj.get("type") != "tag" or depth == 5:
                    raise LookupError("unresolved_commit")
                annotated = get(f"{api}/git/tags/{obj['sha']}")
                if not isinstance(annotated, dict) or annotated.get("sha") != obj["sha"]:
                    raise LookupError("tag_object_identity_mismatch")
                obj = annotated.get("object")
        else:
            raise LookupError("unsupported_provider")
        result["status"] = "observed"
    except LookupError as exc:
        # A partial version without its resolved ref is not a usable candidate.
        for field in ("version", "revision", "branch", "release_url", "source_url"):
            result.pop(field, None)
        result["reason"] = str(exc)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="provider", required=True)
    commands.add_parser("go", help="current stable release from go.dev")
    github = commands.add_parser("github", help="public publisher release or default branch")
    github.add_argument("project", help="official OWNER/REPO, verified from the project's documentation")
    selection = github.add_mutually_exclusive_group()
    selection.add_argument("--tag", help="exact published stable release tag")
    selection.add_argument("--default-branch", action="store_true", help="observe HEAD when maintaining an imported pin")
    args = parser.parse_args()
    result = observe(args.provider, getattr(args, "project", None), getattr(args, "tag", None),
                     getattr(args, "default_branch", False))
    print(json.dumps(result, indent=2))
    return 0 if result["status"] == "observed" else 1


if __name__ == "__main__":
    sys.exit(main())

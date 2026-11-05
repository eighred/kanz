"""Fail closed unless a release tag identifies the checked-out main ancestor.

Protect main and immutable v* tags outside the workflow. Exact-source CI is
required before publishing; historical signer authority remains separate #1406.
"""

import os
import re
import subprocess
import sys
from release_ci import EvidenceError, GitHub, verify_ci


class SourceError(Exception):
    """Release source cannot be established."""


def git(*args):
    result = subprocess.run(
        ["git", *args], capture_output=True, text=True, timeout=30, check=False
    )
    if result.returncode != 0:
        raise SourceError("Git could not establish release source or ancestry")
    return result.stdout.strip()


def verify_source(env):
    if env.get("GITHUB_REPOSITORY") != "eighred/kanz":
        raise SourceError("Release signing belongs to eighred/kanz")
    if env.get("GITHUB_EVENT_NAME") not in {"push", "workflow_dispatch"}:
        raise SourceError("Release requires a tag push or tag workflow dispatch")
    ref = env.get("GITHUB_REF", "")
    # Match Docker metadata's version semantics; permit prereleases and build
    # metadata without accepting shell metacharacters or branch dispatches.
    number = r"(?:0|[1-9][0-9]*)"
    identifier = r"[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*"
    prerelease_id = rf"(?:{number}|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)"
    prerelease = rf"{prerelease_id}(?:\.{prerelease_id})*"
    version = rf"{number}\.{number}\.{number}(?:-{prerelease})?(?:\+{identifier})?"
    if env.get("GITHUB_REF_TYPE") != "tag" or not re.fullmatch(
        rf"refs/tags/v{version}", ref
    ):
        raise SourceError("Release requires a vMAJOR.MINOR.PATCH tag")
    sha = env.get("GITHUB_SHA", "")
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise SourceError("Release event must identify a full commit SHA")
    # Peel annotated tags, but insist the event itself names a commit.
    commit = git("rev-parse", "--verify", f"{sha}^{{commit}}")
    if commit != sha:
        raise SourceError("Release event SHA is not a commit")
    if git("rev-parse", "--verify", "HEAD^{commit}") != sha:
        raise SourceError("Checkout does not match the release event")
    if git("rev-parse", "--verify", f"{ref}^{{commit}}") != sha:
        raise SourceError("Tag does not match the release event")
    main = git("rev-parse", "--verify", "refs/remotes/origin/main^{commit}")
    git("merge-base", "--is-ancestor", sha, main)
    return ref, sha, main


def main():
    try:
        ref, sha, reviewed_main = verify_source(os.environ)
        evidence = verify_ci(GitHub(os.environ.get("GH_TOKEN", "")), sha)
    except (SourceError, EvidenceError, OSError, subprocess.TimeoutExpired) as error:
        print(f"Release source rejected: {error}", file=sys.stderr)
        return 1
    print(f"Verified {ref} at {sha}, ancestor of origin/main {reviewed_main}")
    for item in evidence:
        print(f"Verified exact-source CI {item['workflow']} run {item['run_id']} attempt {item['attempt']}")
    print("Protected-tag and historical signer authority remain separate controls (#1406).")
    return 0


if __name__ == "__main__":
    sys.exit(main())

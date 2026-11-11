"""Fail-closed scope and result checks for the always-present application CI gate.

Only Git and the Python standard library are needed. Paths come from a complete
local Git diff, not the event's truncated file list or a paginated API response.
The small workflow contract reader accepts our explicit block-job layout; it is
not a YAML parser and rejects alternate job declarations instead of ignoring them.
"""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


WORKFLOW = Path(".github/workflows/kanz-ci.yml")
CORE_JOBS = frozenset({
    "release-custody", "go", "go-race", "python", "ts", "web", "sdk-guard",
    "module-graph", "observability", "buf-breaking",
})
SCOPE_IF = "needs.scope.outputs.application == 'true'"


def git(repo, *args):
    return subprocess.run(
        ["git", "-C", str(repo), *args], check=True, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    ).stdout


def commit(repo, value):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{40}", value):
        raise ValueError("event must contain a full lowercase commit SHA")
    if git(repo, "cat-file", "-t", value).strip() != b"commit":
        raise ValueError("event SHA must resolve to a commit")
    return value


def application_path(path):
    # Only known non-runtime material may skip application CI. Unknown new
    # directories run the suite until deliberately classified in this policy.
    if path.startswith((b"kanz/", b"kanz-py/", b"web/", b"kanz-schemas/",
                        b"tools/", b".github/scripts/", b".github/actions/",
                        b".github/workflows/")):
        return True
    return not (path.endswith(b".md") or path == b".gitignore")


def scope(repo, event_name, event, checked_sha):
    checked_sha = commit(repo, checked_sha)
    if git(repo, "rev-parse", "HEAD").decode().strip() != checked_sha:
        raise ValueError("checkout does not match the workflow commit")
    if event_name == "pull_request":
        pr = event["pull_request"]
        base = commit(repo, pr["base"]["sha"])
        head = commit(repo, pr["head"]["sha"])
        # The checked-out merge must contain both event commits. A stale or
        # mismatched checkout cannot classify a different revision as docs-only.
        for ancestor in (base, head):
            git(repo, "merge-base", "--is-ancestor", ancestor, checked_sha)
        base = git(repo, "merge-base", base, head).decode().strip()
    elif event_name == "push":
        head = commit(repo, event["after"])
        if head != checked_sha or event.get("deleted", False):
            raise ValueError("push does not describe the checked-out commit")
        if event["before"] == "0" * 40:
            return True, "new branch: full application suite"
        base = commit(repo, event["before"])
    else:
        raise ValueError("unsupported workflow event")
    # Disable rename detection so moving an application file into a docs path
    # still includes its deleted application path. NUL delimiters preserve names
    # containing whitespace, newlines or non-UTF8 bytes.
    raw = git(repo, "diff", "--name-only", "--no-renames", "-z", base, head, "--")
    if raw and not raw.endswith(b"\0"):
        raise ValueError("incomplete Git path output")
    paths = raw.split(b"\0")[:-1] if raw else []
    needed = any(application_path(path) for path in paths)
    return needed, f"{len(paths)} changed paths; base={base}; head={head}"


def workflow_jobs(text):
    lines = text.splitlines()
    if lines.count("jobs:") != 1:
        raise ValueError("workflow must have one explicit jobs mapping")
    blocks = {}
    current = None
    for line in lines[lines.index("jobs:") + 1:]:
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if line.startswith("    "):
            if current is None:
                raise ValueError("job body without job declaration")
            blocks[current].append(line)
            continue
        match = re.fullmatch(r"  ([a-z][a-z0-9-]*):", line)
        if not match or match[1] in blocks:
            raise ValueError("unsupported or duplicate job declaration")
        current = match[1]
        blocks[current] = []
    expected = set(blocks) - {"scope", "kanz-ci-required"}
    if not CORE_JOBS <= expected or not {"scope", "kanz-ci-required"} <= set(blocks):
        raise ValueError("required application or coordination job removed")
    for name in expected:
        if name == "go":
            if blocks[name].count("    needs: [scope, go-race]") != 1 or blocks[name].count(
                "    if: ${{ always() && needs.scope.outputs.application == 'true' }}"
            ) != 1 or "go-race" not in expected:
                raise ValueError("go must aggregate both race runners even after failure")
            continue
        if blocks[name].count("    needs: scope") != 1 or blocks[name].count(
            f"    if: {SCOPE_IF}"
        ) != 1:
            raise ValueError(f"{name} must depend on the explicit application scope")
    gate = blocks["kanz-ci-required"]
    needs = [line for line in gate if line.startswith("    needs:")]
    if len(needs) != 1 or not re.fullmatch(r"    needs: \[[a-z0-9, -]+\]", needs[0]):
        raise ValueError("gate must have one explicit needs list")
    dependencies = needs[0].removeprefix("    needs: [").removesuffix("]").split(", ")
    if len(dependencies) != len(set(dependencies)) or set(dependencies) != expected | {"scope"}:
        raise ValueError("gate needs must include every application job and scope")
    if gate.count("    if: ${{ always() }}") != 1:
        raise ValueError("required gate must run after failures and skips")
    return expected


def aggregate(needs, expected):
    if not isinstance(needs, dict) or set(needs) != expected | {"scope"}:
        raise ValueError("missing or unexpected prerequisite results")
    scope_result = needs["scope"]
    if scope_result.get("result") != "success":
        raise ValueError("scope did not complete successfully")
    selected = scope_result.get("outputs", {}).get("application")
    if selected not in ("true", "false"):
        raise ValueError("missing or invalid application scope")
    required = "success" if selected == "true" else "skipped"
    for name in sorted(expected):
        if needs[name].get("result") != required:
            raise ValueError(f"{name}: expected {required}, got {needs[name].get('result')}")
    return "application suite passed" if selected == "true" else "explicit non-application scope passed"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("scope", "aggregate"))
    args = parser.parse_args()
    expected = workflow_jobs(WORKFLOW.read_text(encoding="utf-8"))
    if args.mode == "aggregate":
        print(aggregate(json.loads(os.environ["NEEDS_JSON"]), expected))
    else:
        event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
        selected, reason = scope(Path.cwd(), os.environ["GITHUB_EVENT_NAME"], event, os.environ["GITHUB_SHA"])
        value = "true" if selected else "false"
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            output.write(f"application={value}\n")
        print(f"application={value}; {reason}")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, AttributeError, OSError, subprocess.CalledProcessError) as error:
        print(f"Required CI refused: {error}", file=sys.stderr)
        sys.exit(1)

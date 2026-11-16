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
FORGE_JOB = "forge"
FORGE_IF = "needs.scope.outputs.forge == 'true'"

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


def forge_path(path):
    return (path.startswith((b".agents/", b"plugins/kanz-forge/"))
            or path.rsplit(b"/", 1)[-1].lower() == b"readme.md"
            or path in {b"AGENTS.md", b"CONTRIBUTING.md", b".codex/config.toml",
                        b"tools/check_readmes.py", b"tools/test_check_readmes.py",
                        b".github/workflows/kanz-forge.yml",
                        b".github/workflows/kanz-ci.yml", b".github/scripts/required_ci.py",
                        b".github/scripts/test_required_ci.py"})


def application_path(path):
    # Only known non-runtime material may skip application CI. Unknown new
    # directories run the suite until deliberately classified in this policy.
    if path.startswith((b"kanz/", b"python/", b"web/", b"schemas/",
                        b"tools/", b".github/scripts/", b".github/actions/",
                        b".github/workflows/")):
        return True
    return not (path.startswith(b".agents/plugins/kanz-forge/")
                or path == b".agents/plugins/marketplace.json"
                or path.endswith(b".md") or path == b".gitignore")


def position_reclaim_path(path):
    # Any new test publisher must participate before we know its eventual owner.
    # This adds proof; it does not alter application/native workflow scope.
    # The import check compiles all ./... packages. Both passes run the full
    # internal and services trees. Keep those owners and pkg inputs, including
    # embedded/runtime fixtures, prebuilt boundaries and running fixture owners.
    # Cargo resolves workspace member manifests even for a selected package;
    # unrelated member source does not belong to this fixture's build closure.
    return ((path.startswith(b"kanz/") and path.endswith(b".go"))
            or (path.startswith((b"rust/crates/", b"rust/services/")) and path.endswith(b"/Cargo.toml"))
            or path.startswith((b"kanz/internal/", b"kanz/services/", b"kanz/pkg/", b"kanz/test/backing/position_reclaim/",
                                b"rust/crates/oms_store/", b"rust/crates/oms_recovery/", b"kanz/infra/nats/",
                                b"kanz/cmd/kanz_migrate/", b"kanz/cmd/kanz_halt/", b"kanz/test/mtls/",
                                b"schemas/proto/", b".github/actions/setup_buf/",
                                b"rust/src/", b"rust/crates/order_core/", b"rust/crates/proto_wire/", b"rust/crates/browser_render/",
                                b"web/src/", b"web/public/", b"python/kanz_inference/", b"python/kanz_bus/",
                                b".github/scripts/position_reclaim", b".github/scripts/test_position_reclaim"))
            or path in {b".github/workflows/kanz-ci.yml", b".github/scripts/required_ci.py", b".github/scripts/test_required_ci.py",
                        b".github/scripts/go_test_evidence.py", b".github/scripts/go_shard_evidence.py",
                        b"kanz/test/backing/nats-dev.conf", b"kanz/go.mod", b"kanz/go.sum", b"kanz/Makefile",
                        b"kanz/infra/deploy/venue-binance-deploy.yaml", b"kanz/infra/deploy/venue-okx-deploy.yaml",
                        b"kanz/infra/deploy/accounting-deploy.yaml",
                        b"schemas/buf.yaml", b"schemas/buf.gen.yaml", b"schemas/bootstrap-go-sdk.sh",
                        b"rust/Cargo.toml", b"rust/Cargo.lock", b"rust/rust-toolchain.toml", b"rust/build.rs",
                        b"web/.nvmrc", b"web/package.json", b"web/package-lock.json", b"web/tsconfig.json",
                        b"web/vite.config.ts", b"web/index.html", b"web/scripts/build-renderer.mjs",
                        b"web/scripts/build-renderer.test.mjs", b"web/e2e/sessions.cjs", b"web/e2e/mfa.cjs",
                        b"python/requirements.txt", b"python/requirements-codegen.txt"})


def scope(repo, event_name, event, checked_sha, *, images=False, position_reclaim=False):
    checked_sha = commit(repo, checked_sha)
    if git(repo, "rev-parse", "HEAD").decode().strip() != checked_sha:
        raise ValueError("checkout does not match the workflow commit")
    if images and event_name == "workflow_dispatch":
        return {"application": True, "forge": True}, "explicit full image qualification"
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
            selected = {"application": True, "forge": True}
            if position_reclaim: selected["position_reclaim"] = True
            return selected, "new branch: full verification"
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
    selected = {"application": needed, "forge": any(forge_path(path) for path in paths)}
    if position_reclaim: selected["position_reclaim"] = any(position_reclaim_path(path) for path in paths)
    return selected, f"{len(paths)} changed paths; base={base}; head={head}"


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
    scope_job = blocks.get("scope", [])
    for line in (
        "      application: ${{ steps.scope.outputs.application }}",
        "      forge: ${{ steps.scope.outputs.forge }}",
        "      position_reclaim: ${{ steps.scope.outputs.position_reclaim }}",
        "          fetch-depth: 0",
        "        id: scope",
        "        run: python3 .github/scripts/required_ci.py scope",
    ):
        if scope_job.count(line) != 1:
            raise ValueError("scope must bind complete Git history and both verified outputs")
    expected = set(blocks) - {"scope", "kanz-ci-required"}
    if (blocks.get("go-race", []).count("          POSITION_RECLAIM_SCOPE: ${{ needs.scope.outputs.position_reclaim }}") != 1
            or blocks.get("go-race", []).count('          if [ "${{ matrix.shard }}" = "0" ] && [ "$POSITION_RECLAIM_SCOPE" = "true" ]; then') != 1
            or blocks.get("go-race", []).count("              python3 -B ../.github/scripts/position_reclaim_proof.py \\") != 1):
        raise ValueError("test-support changes must retain the scoped same-broker proof")
    if not (CORE_JOBS | {FORGE_JOB}) <= expected or not {"scope", "kanz-ci-required"} <= set(blocks):
        raise ValueError("required application or coordination job removed")
    for name in expected:
        if name == FORGE_JOB:
            if (blocks[name].count("    needs: scope") != 1
                    or blocks[name].count(f"    if: {FORGE_IF}") != 1
                    or blocks[name].count("    uses: ./.github/workflows/kanz-forge.yml") != 1
                    or any(line.startswith(("    continue-on-error:", "    secrets:")) for line in blocks[name])):
                raise ValueError("Forge must use its scoped required read-only workflow")
            continue
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


def forge_workflow(text):
    """The non-runtime exemption always retains its owning Linux checks."""
    commands = [line.strip().removeprefix("run: ") for line in text.splitlines()
                if line.startswith("        run: ")]
    required = [
        "python3 .agents/plugins/kanz-forge/scripts/validate.py --tracked",
        "python3 -B tools/check_readmes.py --inventory",
        "python3 -B -m unittest discover -s tools -p test_check_readmes.py -v",
        "python3 -B .agents/plugins/kanz-forge/scripts/provenance.py",
        "node --test .agents/plugins/kanz-forge/skills/security-audit/*.test.cjs",
        "python3 -B -m unittest discover -s .agents/plugins/kanz-forge/scripts -p 'test_*.py' -v",
    ]
    uses = [line.strip() for line in text.splitlines() if line.strip().startswith("- uses:")]
    if ("on:\n  workflow_call:\n" not in text or "  pull_request:" in text or "  push:" in text
            or "permissions:\n  contents: read\n" not in text
            or "    runs-on: ubuntu-latest" not in text
            or commands != required
            or uses != ["- uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1"]
            or any(line.strip().startswith(("if:", "continue-on-error:", "secrets:", "defaults:", "shell:", "env:", "container:", "strategy:", "working-directory:"))
                   for line in text.splitlines())):
        raise ValueError("Forge owner must retain its unconditional read-only Linux verification")


def aggregate(needs, expected):
    if not isinstance(needs, dict) or set(needs) != expected | {"scope"}:
        raise ValueError("missing or unexpected prerequisite results")
    scope_result = needs["scope"]
    if scope_result.get("result") != "success":
        raise ValueError("scope did not complete successfully")
    selected = scope_result.get("outputs", {}).get("application")
    if selected not in ("true", "false"):
        raise ValueError("missing or invalid application scope")
    forge = scope_result.get("outputs", {}).get("forge")
    if forge not in ("true", "false"):
        raise ValueError("missing or invalid Forge scope")
    required = "success" if selected == "true" else "skipped"
    for name in sorted(expected):
        wanted = ("success" if forge == "true" else "skipped") if name == FORGE_JOB else required
        if needs[name].get("result") != wanted:
            raise ValueError(f"{name}: expected {wanted}, got {needs[name].get('result')}")
    return "application suite passed" if selected == "true" else "explicit non-application scope passed"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("scope", "images", "aggregate"))
    args = parser.parse_args()
    expected = workflow_jobs(WORKFLOW.read_text(encoding="utf-8"))
    forge_workflow(Path(".github/workflows/kanz-forge.yml").read_text(encoding="utf-8"))
    if args.mode == "aggregate":
        print(aggregate(json.loads(os.environ["NEEDS_JSON"]), expected))
    else:
        event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
        selected, reason = scope(Path.cwd(), os.environ["GITHUB_EVENT_NAME"], event, os.environ["GITHUB_SHA"], images=args.mode == "images", position_reclaim=args.mode == "scope")
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            for name, needed in selected.items():
                output.write(f"{name}={str(needed).lower()}\n")
        print(f"scope={selected}; {reason}")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, AttributeError, OSError, subprocess.CalledProcessError) as error:
        print(f"Required CI refused: {error}", file=sys.stderr)
        sys.exit(1)

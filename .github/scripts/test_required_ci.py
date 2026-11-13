"""Real Git scope fixtures and adversarial required-check contract tests."""

import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import required_ci as policy


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = (ROOT / policy.WORKFLOW).read_text(encoding="utf-8")
FORGE_WORKFLOW = (ROOT / ".github/workflows/kanz-forge.yml").read_text(encoding="utf-8")


class GitScopeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        self.git("init", "--quiet")
        self.git("config", "user.name", "CI fixture")
        self.git("config", "user.email", "ci@example.invalid")
        self.git("config", "core.autocrlf", "false")
        self.write("README.md", "baseline\n")
        self.write("kanz/app.go", "package app\n")
        self.base = self.save()

    def git(self, *args):
        return policy.git(self.repo, *args).decode().strip()

    def write(self, name, text):
        path = self.repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def save(self):
        self.git("add", "--all")
        self.git("-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "fixture")
        return self.git("rev-parse", "HEAD")

    def push(self, before, after):
        return policy.scope(self.repo, "push", {"before": before, "after": after}, after)[0]["application"]

    def test_docs_only_and_mixed_changes(self):
        self.write("docs/space name.md", "documentation")
        docs = self.save()
        self.assertFalse(self.push(self.base, docs))
        self.write("kanz/app.go", "package changed")
        code = self.save()
        self.assertTrue(self.push(self.base, code))

    def test_plugin_only_and_mixed_changes_retain_their_owners(self):
        for name in (".agents/plugins/kanz-forge/scripts/new.py", ".agents/plugins/marketplace.json"):
            with self.subTest(name=name):
                self.write(name, "plugin input")
                head = self.save()
                selected, _ = policy.scope(self.repo, "push", {"before": self.base, "after": head}, head)
                self.assertEqual(selected, {"application": False, "forge": True})
                self.write("kanz/app.go", "package mixed")
                mixed = self.save()
                selected, _ = policy.scope(self.repo, "push", {"before": self.base, "after": mixed}, mixed)
                self.assertEqual(selected, {"application": True, "forge": True})
                self.git("reset", "--hard", self.base)

    def test_plugin_exemption_does_not_hide_unknown_or_deleted_runtime(self):
        for name in (".agents/new-runtime.py", "plugins/kanz-forge/commands.py", ".agents/plugins/kanz-forge_other/app.py"):
            with self.subTest(name=name):
                self.write(name, "unknown owner")
                self.assertTrue(self.push(self.base, self.save()))
                self.git("reset", "--hard", self.base)
        self.write(".agents/plugins/kanz-forge/new.py", "plugin")
        self.git("mv", "kanz/app.go", ".agents/plugins/kanz-forge/moved.go")
        selected, _ = policy.scope(self.repo, "push", {"before": self.base, "after": self.save()}, self.git("rev-parse", "HEAD"))
        self.assertEqual(selected, {"application": True, "forge": True})

    def test_image_scope_uses_the_same_history_and_manual_is_explicit_full_scope(self):
        self.write(".agents/plugins/kanz-forge/new.py", "plugin")
        head = self.save()
        event = {"before": self.base, "after": head}
        self.assertEqual(policy.scope(self.repo, "push", event, head, images=True)[0],
                         {"application": False, "forge": True})
        self.assertEqual(policy.scope(self.repo, "workflow_dispatch", {}, head, images=True)[0],
                         {"application": True, "forge": True})

    def test_delete_and_rename_out_of_application_still_require_ci(self):
        self.git("mv", "kanz/app.go", "moved.md")
        renamed = self.save()
        self.assertTrue(self.push(self.base, renamed))
        self.git("reset", "--hard", self.base)
        self.git("rm", "kanz/app.go")
        deleted = self.save()
        self.assertTrue(self.push(self.base, deleted))

    def test_rename_documentation_into_application_requires_ci(self):
        self.git("mv", "README.md", "kanz/document.md")
        head = self.save()
        self.assertTrue(self.push(self.base, head))

    def test_diff_is_not_limited_to_githubs_first_300_paths(self):
        for index in range(305):
            self.write(f"docs/{index:03}.md", "documentation")
        self.write("kanz/last.go", "package last")
        head = self.save()
        self.assertTrue(self.push(self.base, head))

    def test_policy_and_build_helpers_cannot_skip(self):
        for name in (
            ".github/scripts/required_ci.py", ".github/scripts/release_source.py",
            ".github/actions/setup-buf/action.yml", ".github/workflows/kanz-ci.yml",
            "tools/Initialize-TestnetPortfolio.ps1", "kanz-schemas/packaging/go/go.mod",
            "new-runtime/entrypoint", "kanz-web/README.md",
        ):
            with self.subTest(name=name):
                self.write(name, "fixture")
                head = self.save()
                self.assertTrue(self.push(self.base, head))
                self.git("reset", "--hard", self.base)

    def test_pr_uses_merge_base_not_unrelated_base_branch_changes(self):
        self.git("checkout", "-b", "topic")
        self.write("README.md", "topic docs")
        head = self.save()
        self.git("checkout", "--detach", self.base)
        self.write("kanz/app.go", "package base_changed")
        base = self.save()
        self.git("-c", "commit.gpgsign=false", "merge", "--no-ff", "-m", "merge fixture", head)
        checked = self.git("rev-parse", "HEAD")
        event = {"pull_request": {"base": {"sha": base}, "head": {"sha": head}}}
        self.assertFalse(policy.scope(self.repo, "pull_request", event, checked)[0]["application"])

    def test_new_branch_and_force_push_are_conservative(self):
        self.assertTrue(self.push("0" * 40, self.base))
        self.write("kanz/app.go", "package removed_by_force_push")
        previous = self.save()
        self.git("reset", "--hard", self.base)
        self.assertTrue(self.push(previous, self.base))

    def test_missing_commit_malformed_sha_and_checkout_mismatch_fail(self):
        for before in ("f" * 40, "main", "--help", "", None):
            with self.subTest(before=before):
                with self.assertRaises((ValueError, subprocess.CalledProcessError)):
                    self.push(before, self.base)
        self.write("README.md", "next")
        head = self.save()
        with self.assertRaisesRegex(ValueError, "checkout"):
            policy.scope(self.repo, "push", {"before": self.base, "after": self.base}, self.base)
        with self.assertRaisesRegex(ValueError, "push"):
            policy.scope(self.repo, "push", {"before": self.base, "after": self.base}, head)

    def test_noncommit_and_wrong_pr_ancestry_fail(self):
        blob = self.git("rev-parse", "HEAD:README.md")
        with self.assertRaisesRegex(ValueError, "commit"):
            self.push(blob, self.base)
        self.write("README.md", "future")
        future = self.save()
        self.git("reset", "--hard", self.base)
        event = {"pull_request": {"base": {"sha": self.base}, "head": {"sha": future}}}
        with self.assertRaises(subprocess.CalledProcessError):
            policy.scope(self.repo, "pull_request", event, self.base)

    def test_unknown_or_incomplete_event_fails(self):
        for event_name, event in (("workflow_dispatch", {}), ("push", {}), ("pull_request", {})):
            with self.subTest(event=event_name), self.assertRaises((ValueError, KeyError)):
                policy.scope(self.repo, event_name, event, self.base)

    @unittest.skipIf(os.name == "nt", "Windows cannot create newline-containing paths")
    def test_newline_path_cannot_fake_a_documentation_extension(self):
        self.write("kanz/app.go\npretend.md", "runtime")
        head = self.save()
        self.assertTrue(self.push(self.base, head))

    def test_cli_failure_never_emits_a_successful_scope(self):
        self.write(str(policy.WORKFLOW), WORKFLOW)
        self.write(".github/workflows/kanz-forge.yml", FORGE_WORKFLOW)
        head = self.save()
        event_file = self.repo / "event.json"
        event_file.write_text(json.dumps({"before": "missing", "after": head}), encoding="utf-8")
        output_file = self.repo / "outputs"
        result = subprocess.run(
            [sys.executable, str(ROOT / ".github/scripts/required_ci.py"), "scope"],
            cwd=self.repo, capture_output=True, text=True,
            env={**os.environ, "GITHUB_EVENT_PATH": str(event_file), "GITHUB_OUTPUT": str(output_file),
                 "GITHUB_EVENT_NAME": "push", "GITHUB_SHA": head},
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output_file.exists())

    def test_cli_emits_exact_boolean_for_verified_docs_only_change(self):
        self.write(str(policy.WORKFLOW), WORKFLOW)
        self.write(".github/workflows/kanz-forge.yml", FORGE_WORKFLOW)
        base = self.save()
        self.write("README.md", "only docs changed")
        head = self.save()
        event_file = self.repo / "event.json"
        event_file.write_text(json.dumps({"before": base, "after": head}), encoding="utf-8")
        output_file = self.repo / "outputs"
        result = subprocess.run(
            [sys.executable, str(ROOT / ".github/scripts/required_ci.py"), "scope"],
            cwd=self.repo, capture_output=True, text=True,
            env={**os.environ, "GITHUB_EVENT_PATH": str(event_file), "GITHUB_OUTPUT": str(output_file),
                 "GITHUB_EVENT_NAME": "push", "GITHUB_SHA": head},
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output_file.read_text(encoding="utf-8"), "application=false\nforge=true\n")


class AggregateTests(unittest.TestCase):
    def setUp(self):
        self.expected = policy.workflow_jobs(WORKFLOW)
        self.results = {name: {"result": "success"} for name in self.expected}
        self.results["scope"] = {"result": "success", "outputs": {"application": "true", "forge": "true"}}

    def test_all_application_jobs_must_pass(self):
        self.assertEqual(policy.aggregate(self.results, self.expected), "application suite passed")
        for name in self.expected:
            for result in ("failure", "cancelled", "skipped", "pending", None):
                with self.subTest(job=name, result=result):
                    needs = copy.deepcopy(self.results)
                    needs[name]["result"] = result
                    with self.assertRaises(ValueError):
                        policy.aggregate(needs, self.expected)

    def test_explicit_docs_scope_requires_all_jobs_skipped(self):
        self.results["scope"]["outputs"].update(application="false", forge="false")
        for name in self.expected:
            self.results[name]["result"] = "skipped"
        self.assertIn("non-application", policy.aggregate(self.results, self.expected))
        for result in ("success", "failure", "cancelled", None):
            self.results["go"]["result"] = result
            with self.subTest(result=result), self.assertRaises(ValueError):
                policy.aggregate(self.results, self.expected)

    def test_plugin_only_scope_requires_forge_success_and_core_skips(self):
        self.results["scope"]["outputs"].update(application="false", forge="true")
        for name in self.expected:
            self.results[name]["result"] = "success" if name == policy.FORGE_JOB else "skipped"
        policy.aggregate(self.results, self.expected)
        for result in ("failure", "cancelled", "skipped", None):
            self.results[policy.FORGE_JOB]["result"] = result
            with self.subTest(result=result), self.assertRaises(ValueError):
                policy.aggregate(self.results, self.expected)

    def test_forge_scope_missing_or_malformed_cannot_pass(self):
        for value in (None, "", True, "False", "true\nfalse"):
            self.results["scope"]["outputs"]["forge"] = value
            with self.subTest(value=value), self.assertRaises(ValueError):
                policy.aggregate(self.results, self.expected)

    def test_missing_unknown_job_or_scope_cannot_pass(self):
        for missing in self.results:
            needs = copy.deepcopy(self.results)
            del needs[missing]
            with self.subTest(missing=missing), self.assertRaises(ValueError):
                policy.aggregate(needs, self.expected)
        self.results["untracked-job"] = {"result": "success"}
        with self.assertRaises(ValueError):
            policy.aggregate(self.results, self.expected)

    def test_scope_failure_and_bad_outputs_cannot_pass(self):
        for result in ("failure", "cancelled", "skipped", None):
            self.results["scope"]["result"] = result
            with self.subTest(result=result), self.assertRaises(ValueError):
                policy.aggregate(self.results, self.expected)
        self.results["scope"]["result"] = "success"
        for output in (None, "", "False", True, "true\nfalse"):
            self.results["scope"]["outputs"]["application"] = output
            with self.subTest(output=output), self.assertRaises(ValueError):
                policy.aggregate(self.results, self.expected)


class WorkflowContractTests(unittest.TestCase):
    def test_every_pr_and_main_push_gets_the_required_gate(self):
        trigger = WORKFLOW.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertEqual(trigger.strip(), "pull_request:\n  push:\n    branches: [main]")
        self.assertEqual(policy.workflow_jobs(WORKFLOW), policy.CORE_JOBS | {policy.FORGE_JOB})
        self.assertIn("fetch-depth: 0", WORKFLOW.split("  release-custody:")[0])
        self.assertIn("python3 -B -m unittest discover -s .github/scripts -p test_required_ci.py", WORKFLOW)
        self.assertIn("python3 -B -m unittest discover -s .github/scripts -p 'release_*_test.py'", WORKFLOW)
        self.assertIn("NEEDS_JSON: ${{ toJSON(needs) }}", WORKFLOW)
        self.assertIn("application: ${{ steps.scope.outputs.application }}", WORKFLOW)

    def test_forge_workflow_is_called_once_and_checks_cannot_be_disabled(self):
        policy.forge_workflow(FORGE_WORKFLOW)
        for old, new in (("on:\n  workflow_call:", "on:\n  pull_request:"),
                         ("    runs-on: ubuntu-latest", "    runs-on: windows-latest"),
                         ("run: node --test", "run: true # node --test"),
                         ("scripts/provenance.py", "scripts/provenance.py || true"),
                         ("    steps:", "    if: false\n    steps:"),
                         ("    steps:", "    continue-on-error: true\n    steps:")):
            with self.subTest(old=old), self.assertRaises(ValueError):
                policy.forge_workflow(FORGE_WORKFLOW.replace(old, new))
        for old, new in (("    if: " + policy.FORGE_IF, "    if: false"),
                         ("    uses: ./.github/workflows/kanz-forge.yml", "    uses: other/owner@main"),
                         ("application: ${{ steps.scope.outputs.application }}", "application: 'false'"),
                         ("forge: ${{ steps.scope.outputs.forge }}", "forge: 'false'"),
                         ("fetch-depth: 0", "fetch-depth: 1")):
            with self.subTest(old=old), self.assertRaises(ValueError):
                policy.workflow_jobs(WORKFLOW.replace(old, new))

    def test_new_job_without_gate_dependency_fails(self):
        added = WORKFLOW + "\n  added-job:\n    needs: scope\n    if: " + policy.SCOPE_IF + "\n    runs-on: ubuntu-latest\n"
        with self.assertRaisesRegex(ValueError, "every application job"):
            policy.workflow_jobs(added)

    def test_removed_job_wrong_condition_and_incomplete_needs_fail(self):
        mutations = (
            WORKFLOW.replace("  go:\n", "  renamed-go:\n"),
            WORKFLOW.replace("    if: " + policy.SCOPE_IF, "    if: false", 1),
            WORKFLOW.replace("needs: [scope, forge, release-custody, go,", "needs: [scope, forge, release-custody,"),
            WORKFLOW.replace("    if: ${{ always() }}", "    if: success()"),
            WORKFLOW.replace("  go:\n", "  go: &shared\n"),
            WORKFLOW + "\n  go:\n    runs-on: ubuntu-latest\n",
        )
        for index, mutation in enumerate(mutations):
            with self.subTest(mutation=index), self.assertRaises(ValueError):
                policy.workflow_jobs(mutation)


if __name__ == "__main__":
    unittest.main()

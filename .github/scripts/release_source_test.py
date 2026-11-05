"""Local Git fixtures: no release, registry, remote push or signing operations."""

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("release_source.py").resolve()


class ReleaseSourceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.git("init", "-b", "main")
        self.git("config", "user.name", "Release fixture")
        self.git("config", "user.email", "release-fixture@example.invalid")
        self.git("config", "commit.gpgSign", "false")
        self.git("config", "tag.gpgSign", "false")
        self.git("commit", "--allow-empty", "-m", "reviewed source")
        self.sha = self.git("rev-parse", "HEAD")
        self.git("tag", "v1.2.3")
        self.git("update-ref", "refs/remotes/origin/main", self.sha)
        self.env = {
            **os.environ,
            "GITHUB_REPOSITORY": "eighred/kanz",
            "GITHUB_EVENT_NAME": "push",
            "GITHUB_REF_TYPE": "tag",
            "GITHUB_REF": "refs/tags/v1.2.3",
            "GITHUB_SHA": self.sha,
        }

    def git(self, *args):
        return subprocess.run(
            ["git", *args], cwd=self.root, check=True, capture_output=True,
            text=True, timeout=30,
        ).stdout.strip()

    def run_gate(self, accepted, **env):
        result = subprocess.run(
            [sys.executable, "-B", "-c", "import sys,os; sys.path.insert(0,sys.argv[1]); "
             "from release_source import verify_source,SourceError; "
             "\ntry: print(verify_source(os.environ))"
             "\nexcept (SourceError,OSError) as e: print('Release source rejected:',e,file=sys.stderr); sys.exit(1)", str(SCRIPT.parent)], cwd=self.root,
            env={**self.env, **env}, capture_output=True, text=True, timeout=30,
        )
        self.assertEqual(result.returncode, 0 if accepted else 1, result.stderr)
        if accepted:
            self.assertIn(self.sha, result.stdout)
        else:
            self.assertIn("Release source rejected:", result.stderr)

    def test_reviewed_tag_push_and_manual_dispatch(self):
        for event in ("push", "workflow_dispatch"):
            with self.subTest(event=event):
                self.run_gate(True, GITHUB_EVENT_NAME=event)

    def test_valid_source_cli_still_requires_ci_token(self):
        result = subprocess.run([sys.executable, "-B", str(SCRIPT)], cwd=self.root,
            env={**self.env, "GH_TOKEN": ""}, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 1)
        self.assertIn("read-only GitHub token required", result.stderr)
        self.assertNotIn("Verified", result.stdout)

    def test_annotated_tag_on_older_reviewed_commit(self):
        self.git("tag", "-a", "v1.2.3-rc.1+build.2", "-m", "release candidate")
        self.git("commit", "--allow-empty", "-m", "new reviewed source")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.git("checkout", "--detach", self.sha)
        self.run_gate(True, GITHUB_REF="refs/tags/v1.2.3-rc.1+build.2")

    def test_rejects_invalid_event_identity(self):
        cases = [
            {"GITHUB_REPOSITORY": "someone/kanz"},
            {"GITHUB_REPOSITORY": ""},
            {"GITHUB_EVENT_NAME": "pull_request"},
            {"GITHUB_REF_TYPE": "branch"},
            {"GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch"},
            {"GITHUB_REF": "refs/tags/vnext"},
            {"GITHUB_REF": "refs/tags/v1.2.3/other"},
            {"GITHUB_REF": "refs/tags/v1.2.3;other"},
            {"GITHUB_REF": "refs/tags/v01.2.3"},
            {"GITHUB_REF": "refs/tags/v1.2.3-01"},
            {"GITHUB_SHA": ""},
            {"GITHUB_SHA": self.sha[:7]},
            {"GITHUB_SHA": "0" * 40},
        ]
        for case in cases:
            with self.subTest(case=case):
                self.run_gate(False, **case)

    def test_rejects_unreviewed_tag(self):
        self.git("checkout", "-b", "unreviewed")
        self.git("commit", "--allow-empty", "-m", "unreviewed source")
        self.git("tag", "v1.2.4")
        self.run_gate(False, GITHUB_REF="refs/tags/v1.2.4",
                      GITHUB_SHA=self.git("rev-parse", "HEAD"))

    def test_rejects_tag_moved_after_event(self):
        self.git("commit", "--allow-empty", "-m", "different source")
        self.git("tag", "-f", "v1.2.3")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.git("checkout", "--detach", self.sha)
        self.run_gate(False)

    def test_rejects_checkout_different_from_event(self):
        self.git("commit", "--allow-empty", "-m", "different checkout")
        self.run_gate(False)

    def test_rejects_missing_tag(self):
        self.git("tag", "-d", "v1.2.3")
        self.run_gate(False)

    def test_rejects_missing_main(self):
        self.git("update-ref", "-d", "refs/remotes/origin/main")
        self.run_gate(False)

    def test_rejects_tag_object_as_event_sha(self):
        self.git("tag", "-a", "v1.2.4", "-m", "annotated")
        self.run_gate(False, GITHUB_REF="refs/tags/v1.2.4",
                      GITHUB_SHA=self.git("rev-parse", "refs/tags/v1.2.4"))


if __name__ == "__main__":
    unittest.main()

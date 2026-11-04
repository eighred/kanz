"""No Codex calls, network, or persistent user database in these tests."""
import contextlib
from contextlib import closing
import io
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import receipts as core


class ReceiptTests(unittest.TestCase):
    def setUp(self):
        tmp=tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root=Path(tmp.name)/"repo"
        self.root.mkdir()
        self.db=Path(tmp.name)/"local"/"receipts.sqlite"
        self.patch=patch.object(core,"database",return_value=self.db)
        self.patch.start(); self.addCleanup(self.patch.stop)
        subprocess.run(["git","init","-q",self.root],check=True)
        self.source=self.root/"kanz/internal/risk/termsource/adapter.go"
        self.source.parent.mkdir(parents=True)
        self.source.write_text("package adapter\n")
        core.git(self.root,"add",".")
        core.git(self.root,"-c","user.name=test","-c","user.email=test@example.invalid","commit","-qm","baseline")

    def collect(self,*_):
        return {"packages_passed":["github.com/eighred/kanz/internal/risk/"+p for p in ("pricing","benchmarks")],
                "skips":[],"failures":[]}

    def test_read_does_not_create_cache_and_adapter_change_invalidates(self):
        self.assertEqual(core.recall(self.root)["status"],"empty")
        self.assertFalse(self.db.parent.exists())
        core.remember(self.root,self.collect)
        self.assertEqual(core.recall(self.root)["receipts"][0]["status"],"source_unchanged_not_retested")
        self.source.write_text("package altered\n")
        self.assertEqual(core.recall(self.root)["receipts"][0]["status"],"stale")

    def test_skip_or_edit_during_run_never_certifies(self):
        def edit(*args):
            self.source.write_text("package changed\n")
            return self.collect()
        for collector in (lambda *_:dict(self.collect(),skips=["missing fixture"]),edit):
            with self.assertRaises(ValueError):core.remember(self.root,collector)
        self.assertFalse(self.db.exists())

    def test_linked_checkout_shares_history_but_checks_its_own_sources(self):
        core.remember(self.root,self.collect)
        other=self.root.parent/"linked"
        core.git(self.root,"worktree","add","-q","--detach",str(other),"HEAD")
        self.source.write_text("package dirty\n")
        self.assertEqual(core.recall(other)["receipts"][0]["status"],"source_unchanged_not_retested")
        self.assertEqual(core.recall(self.root)["receipts"][0]["status"],"stale")

    def test_duplicate_or_missing_package_does_not_certify(self):
        package = "github.com/eighred/kanz/internal/risk/pricing"
        output = "\n".join(json.dumps({"Action": "pass", "Package": package}) for _ in range(2))
        with patch.object(core.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, output)):
            with self.assertRaisesRegex(ValueError, "incomplete"):
                core.pricing(self.root)

    def test_distinct_clone_does_not_inherit_receipts(self):
        core.remember(self.root, self.collect)
        other = self.root.parent / "clone"
        subprocess.run(["git", "clone", "-q", str(self.root), str(other)], check=True)
        self.assertEqual(core.recall(other)["receipts"], [])

    def test_financial_failure_truncation_and_skip_never_write(self):
        names = ["github.com/eighred/kanz/internal/risk/" + p for p in ("pricing", "benchmarks")]
        passed = [{"Action": "pass", "Package": p} for p in names]
        cases = [
            (0, passed[:1]),  # Harness truncation can still return zero.
            (0, passed + [{"Action": "skip", "Package": names[0], "Test": "Dependency"}]),
            (1, passed),
            (0, passed + [{"Action": "fail", "Package": names[0], "Test": "Broken"}]),
        ]
        for code, events in cases:
            output = "\n".join(json.dumps(event) for event in events)
            with self.subTest(code=code, events=events):
                def collect(root):
                    with patch.object(core.subprocess, "run", return_value=subprocess.CompletedProcess([], code, output)):
                        return core.pricing(root)
                with self.assertRaises(ValueError):
                    core.remember(self.root, collect)
                self.assertFalse(self.db.exists())

    def test_timeout_and_malformed_output_never_write(self):
        def timeout(root):
            with patch.object(core.subprocess, "run", side_effect=subprocess.TimeoutExpired("go", 900)):
                return core.pricing(root)
        with self.assertRaises(subprocess.TimeoutExpired):
            core.remember(self.root, timeout)
        def malformed(root):
            with patch.object(core.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, '{"Action":')):
                return core.pricing(root)
        with self.assertRaises(ValueError):
            core.remember(self.root, malformed)
        self.assertFalse(self.db.exists())

    def test_cli_reports_timeout_as_failure(self):
        with patch.object(sys, "argv", ["receipts", "record"]), \
             patch.object(core.subprocess, "check_output", side_effect=subprocess.TimeoutExpired("git", 45)), \
             contextlib.redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(core.main(), 1)
        self.assertIn("timed out", errors.getvalue())
        self.assertFalse(self.db.exists())

    def test_existing_v1_schema_is_read_without_migration(self):
        self.db.parent.mkdir()
        with closing(sqlite3.connect(self.db)) as connection, connection:
            connection.execute("CREATE TABLE receipts (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, sha TEXT NOT NULL, digest TEXT NOT NULL, observed TEXT NOT NULL)")
            connection.execute("INSERT INTO receipts VALUES(1,?,?,?,?)",
                               (core.identity(self.root), "historical-sha", core.fingerprint(self.root), "2026-01-01"))
        before = self.db.read_bytes()
        result = core.recall(self.root)
        self.assertEqual(result["receipts"][0]["sha"], "historical-sha")
        self.assertEqual(result["receipts"][0]["status"], "source_unchanged_not_retested")
        self.assertFalse(result["authorization"])
        self.assertEqual(self.db.read_bytes(), before)

    def test_added_deleted_scoped_source_and_revision_change(self):
        core.remember(self.root, self.collect)
        added = self.source.with_name("new.go")
        added.write_text("package adapter")
        self.assertEqual(core.recall(self.root)["receipts"][0]["status"], "stale")
        added.unlink()
        self.source.unlink()
        self.assertEqual(core.recall(self.root)["receipts"][0]["status"], "stale")
        def commit(_):
            core.git(self.root, "-c", "user.name=test", "-c", "user.email=test@example.invalid",
                     "commit", "--allow-empty", "-qm", "new revision")
            return self.collect()
        with self.assertRaisesRegex(ValueError, "inputs changed"):
            core.remember(self.root, commit)
        self.assertEqual(len(core.recall(self.root)["receipts"]), 1)

if __name__ == "__main__":
    unittest.main()

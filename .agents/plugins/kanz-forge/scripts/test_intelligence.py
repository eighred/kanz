"""No Codex calls, network, or persistent user database in these tests."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import intelligence as core


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

    def test_routes_deduplicate_and_reject_windows_and_posix_absolute_paths(self):
        self.assertEqual(list(core.route(["a/risk/pricing/bond.go"])["suggestions"]),["/financial"])
        for path in ("../tmp","/tmp","C:/tmp","C:relative"):
            with self.assertRaises(ValueError):core.route([path])

    def test_labels_never_override_protected_item(self):
        def issue(n,*labels):return dict(number=n,title="fixture",labels=[{"name":s} for s in labels])
        result=core.prioritize([issue(1316,"P0","open-work"),issue(2,"P1","open-work","security"),
                                issue(1,"P0","blocked-external")])
        self.assertEqual([i["number"] for i in result["shortlist"]],[2])

    def test_github_dependencies_cover_entire_registry(self):
        base=Path(__file__).resolve().parents[1]
        doc=(base/"DEPENDENCIES.md").read_text(encoding="utf-8")
        for entry in json.loads((base/"upstreams.json").read_text()):
            self.assertIn(entry["url"]+"/tree/"+entry["pin"],doc)
            for lic in entry["license_sources"]: self.assertIn(lic["path"],doc)

if __name__=="__main__":unittest.main()

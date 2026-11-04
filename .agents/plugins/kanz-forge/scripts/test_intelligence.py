"""No Codex calls, network, or persistent user database in these tests."""
import json
import os
from pathlib import Path
import subprocess
import sys
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

    def test_doctor_requires_live_discovery_and_does_not_certify_chat_delegation(self):
        from evidence import PLUGIN
        expected={"kanz-forge:"+p.parent.name for p in (PLUGIN/"skills").glob("*/SKILL.md")}
        version=json.loads((PLUGIN/".codex-plugin/plugin.json").read_text())["version"]
        cache=Path(os.environ.get("CODEX_HOME",Path.home()/".codex"))/"plugins/cache/kanz-local/kanz-forge"/version
        discovered=[{"name":name,"enabled":True,"path":str(cache/"skills"/name.split(":",1)[1]/"SKILL.md")} for name in expected]
        probed=[]
        def probe(args, *_args):
            probed.append(args)
            if "plugin" in args:
                payload={"installed":[{"name":"kanz-forge","enabled":True,"version":version,
                                       "source":{"path":str(PLUGIN)}}]}
                return subprocess.CompletedProcess(args,0,json.dumps(payload),""),None
            if any(str(arg).endswith("command.py") for arg in args):
                output=("$kanz-forge:commands /work\n" if "--preview" in args else
                        json.dumps([{"repo":"cloudflare/security-audit-skill"}]))
                return subprocess.CompletedProcess(args,0,output,""),None
            if "api" in args:
                return subprocess.CompletedProcess(args,0,"eighred/kanz\n",""),None
            return subprocess.CompletedProcess(args,0,"",""),None
        with patch.object(core,"_probe",side_effect=probe), patch.object(core.shutil,"which",side_effect=lambda name:name+".exe"), \
             patch.object(core,"discover_skills",return_value=discovered) as discovery:
            result=core.doctor(self.root)
        self.assertEqual(result["features"]["plugin_structure"]["status"],"implemented")
        self.assertEqual(result["features"]["installed_skills"]["status"],"operational")
        self.assertEqual(result["features"]["native_subagent_delegation"]["status"],"unverified")
        self.assertEqual(result["features"]["ci_aware_coordination"]["status"],"unverified")
        self.assertEqual(result["features"]["receipt_memory"]["status"],"implemented")
        self.assertEqual(result["features"]["command_preview"]["status"],"operational")
        self.assertEqual(result["features"]["upstream_inventory"]["status"],"operational")
        self.assertEqual(result["features"]["github_repo_read"]["status"],"operational")
        if not sys.platform.startswith("linux"):
            self.assertEqual(result["features"]["security_audit_validator"]["status"],"unverified")
            self.assertFalse(any("--test" in args for args in probed))
        discovery.assert_called_once()

        with patch.object(core,"_probe",side_effect=probe), patch.object(core.shutil,"which",side_effect=lambda name:name+".exe"), \
             patch.object(core,"discover_skills",return_value=[dict(skill,path=str(PLUGIN/"skills")) for skill in discovered]):
            missing=core.doctor(self.root)
        self.assertEqual(missing["features"]["installed_skills"]["status"],"incomplete")

    def test_doctor_failed_preview_inventory_and_repo_read_do_not_claim_broad_capability(self):
        def probe(args, *_args):
            if "validate.py" in " ".join(args):
                return subprocess.CompletedProcess(args,0,"validated",""),None
            return subprocess.CompletedProcess(args,1,"","failed"),None
        with patch.object(core,"_probe",side_effect=probe), \
             patch.object(core.shutil,"which",side_effect=lambda name: "gh" if name=="gh" else None):
            features=core.doctor(self.root)["features"]
        for name in ("command_preview","upstream_inventory","github_repo_read"):
            self.assertEqual(features[name]["status"],"incomplete")
        for overbroad in ("command_launcher","evidence_commands","external_github_access"):
            self.assertNotIn(overbroad,features)

if __name__=="__main__":unittest.main()

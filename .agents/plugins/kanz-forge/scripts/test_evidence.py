import contextlib
import io
import json
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import command
import evidence


class ForgeTests(unittest.TestCase):
    def test_changed_reference_is_proposal_only(self):
        old, new = "a"*40, "b"*40
        row = dict(repo="example/project", pin=old, release=None,
                   imports=[{"source":"skill.md"}], compatibility="untested")
        replies = {"/commits?per_page=1":[{"sha":new}], "/releases?per_page=1":[{"tag_name":"v2"}],
                   "/compare/"+old+"..."+new:{"status":"ahead","total_commits":2,
                   "files":[{"filename":"LICENSE"},{"filename":"skill.md"}],
                   "commits":[{"commit":{"message":"security fix"}}]}}
        with patch("subprocess.run",side_effect=AssertionError("unexpected execution")):
            result=evidence.check(row,lambda path: replies[path.removeprefix("repos/example/project")])
        self.assertEqual(result["license_review"],["LICENSE"])
        self.assertEqual(result["changed_imports"],["skill.md"])
        self.assertTrue(result["new_release"] and result["limited_comparison"])
        self.assertEqual(row["pin"],old)

    def test_inventory_and_provenance(self):
        entries=evidence.registry()
        self.assertGreaterEqual(len(entries),12)
        for row in entries.values():
            self.assertTrue(row["license_sources"])
            self.assertTrue(evidence.OID.fullmatch(row["pin"]))
        self.assertEqual(len(entries["cloudflare/security-audit-skill"]["imports"]),21)

    def test_invalid_reference_cannot_be_an_api_route(self):
        with patch.object(evidence,"github",side_effect=AssertionError):
            with self.assertRaises(ValueError): evidence.run("/upstream","../secrets")

    def test_every_command_preview_and_agent_dispatch(self):
        catalog=json.loads((evidence.PLUGIN/"skills/commands/commands.json").read_text())
        for name, item in catalog.items():
            args=[name]+(["sample"] if item["target"]=="required" else [])
            with self.subTest(name=name),patch.object(sys,"argv",["forge","--preview",*args]),contextlib.redirect_stdout(io.StringIO()),patch("subprocess.call",side_effect=AssertionError):
                self.assertEqual(command.main(),0)
            if item.get("mode")!="evidence":
                with patch.object(sys,"argv",["forge",*args]),patch("shutil.which",return_value="codex.exe"),patch("subprocess.call",return_value=17) as invoke:
                    self.assertEqual(command.main(),17)
                    self.assertEqual(invoke.call_args.args[0], ["codex.exe", "$kanz-forge:commands " + " ".join(args)])

    def test_installed_skills_referenced_by_manifest(self):
        base=evidence.PLUGIN
        manifest=json.loads((base/".codex-plugin/plugin.json").read_text())
        names={p.parent.name for p in (base/manifest["skills"]).glob("*/SKILL.md")}
        self.assertEqual(names,{"commands","engineering-standard","financial-verification",
            "issue-archaeology","kanz-verify","pr-delivery","security-audit","stack-routing"})

    def test_tampered_import_is_rejected(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            row=evidence.registry()["cloudflare/security-audit-skill"].copy()
            item=row["imports"][0]
            row["imports"]=[item]
            path=root/item["local"]
            path.parent.mkdir(parents=True)
            path.write_text("changed")
            (root/"upstreams.json").write_text(json.dumps([row]))
            with patch.object(evidence,"PLUGIN",root), self.assertRaisesRegex(ValueError,"import drift"):
                evidence.registry()

    def test_invalid_command(self):
        with patch.object(sys,"argv",["forge","/typo"]),contextlib.redirect_stderr(io.StringIO()),self.assertRaises(SystemExit) as ex:
            command.main()
        self.assertEqual(ex.exception.code,2)

if __name__=="__main__":
    unittest.main()

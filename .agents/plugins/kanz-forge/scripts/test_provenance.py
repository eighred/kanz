"""Offline import integrity and native command boundary regression checks."""
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

import provenance
import validate


class ProvenanceTests(unittest.TestCase):
    def test_inventory_and_provenance(self):
        entries = provenance.registry()
        self.assertEqual(set(entries), {"cloudflare/security-audit-skill"})
        row = entries["cloudflare/security-audit-skill"]
        self.assertTrue(row["license_sources"])
        self.assertEqual(len(row["imports"]), 21)
        doc = (provenance.PLUGIN / "DEPENDENCIES.md").read_text(encoding="utf-8")
        self.assertIn(row["url"] + "/tree/" + row["pin"], doc)
        for license in row["license_sources"]:
            self.assertIn(license["path"], doc)

    def test_tampered_import_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            row = provenance.registry()["cloudflare/security-audit-skill"].copy()
            item = row["imports"][0]
            row["imports"] = [item]
            path = root / item["local"]
            path.parent.mkdir(parents=True)
            path.write_text("changed")
            (root / "upstreams.json").write_text(json.dumps([row]))
            with patch.object(provenance, "PLUGIN", root):
                with self.assertRaisesRegex(ValueError, "import drift"):
                    provenance.registry()

    def test_escaping_import_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            row = provenance.registry()["cloudflare/security-audit-skill"].copy()
            row["imports"] = [{"local": "../outside"}]
            (root / "upstreams.json").write_text(json.dumps([row]))
            with patch.object(provenance, "PLUGIN", root):
                with self.assertRaisesRegex(ValueError, "outside skills"):
                    provenance.registry()

    def test_catalog_has_no_terminal_dispatch(self):
        catalog = json.loads((provenance.PLUGIN / "skills/commands/commands.json").read_text())
        for name, item in catalog.items():
            with self.subTest(command=name):
                self.assertNotIn("mode", item)
                self.assertIn(item["target"], {"optional", "required"})
                self.assertTrue(item["description"] and item["workflow"])
                self.assertNotIn("forge.py", item["workflow"])
                self.assertNotIn("command.py", item["workflow"])
        self.assertIn("references/parallel-work.md", catalog["/work"]["workflow"])
        self.assertIn("security-audit", catalog["/security"]["workflow"])


    def test_empty_inventory_and_invalid_identity_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for rows in ([], [{"repo": "../outside", "pin": "a" * 40, "url": "https://github.com/../outside"}]):
                (root / "upstreams.json").write_text(json.dumps(rows))
                with patch.object(provenance, "PLUGIN", root), self.assertRaises(ValueError):
                    provenance.registry()

    def test_missing_skill_and_optional_reference_break_validation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "plugin"
            shutil.copytree(provenance.PLUGIN, root, ignore=shutil.ignore_patterns("__pycache__"))
            with patch.object(validate, "PLUGIN", root):
                validate.command_files()
                catalog_path = root / "skills/commands/commands.json"
                catalog = json.loads(catalog_path.read_text())
                catalog["/doctor"]["workflow"] += " Read scripts/removed_probe.py."
                catalog_path.write_text(json.dumps(catalog))
                with self.assertRaisesRegex(ValueError, "missing or escaping command reference"):
                    validate.command_files()
                catalog["/doctor"]["workflow"] = "Inspect native host tools."
                catalog_path.write_text(json.dumps(catalog))
                (root / "skills/kanz-verify/SKILL.md").unlink()
                with self.assertRaisesRegex(ValueError, "missing mapped skill"):
                    validate.command_files()

if __name__ == "__main__":
    unittest.main()

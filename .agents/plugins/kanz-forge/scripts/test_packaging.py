"""Offline packaging regressions; no Codex process or user profile is used."""
from pathlib import Path
import runpy
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import test_install as install


class PackagingTests(unittest.TestCase):
    def test_import_has_no_cli_or_installation_side_effect(self):
        with patch.object(subprocess, "check_output", side_effect=AssertionError("CLI called")), \
             patch.object(install.shutil, "which", side_effect=AssertionError("CLI inspected")):
            runpy.run_path(install.__file__, run_name="forge_import_probe")

    def test_snapshot_includes_new_files_and_omits_deleted_and_ignored_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "repo"
            root.mkdir()
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            plugin = root / install.PLUGIN_PATH
            plugin.mkdir(parents=True)
            retired = plugin / "retired.py"
            retired.write_text("old")
            marketplace = root / ".agents/plugins/marketplace.json"
            marketplace.write_text("{}")
            (root / ".gitignore").write_text("*.pyc\n")
            subprocess.run(["git", "add", "."], cwd=root, check=True)
            retired.unlink()
            (plugin / "new.py").write_text("new")
            (plugin / "ignored.pyc").write_text("ignored")
            snapshot = Path(directory) / "snapshot"
            copied = install.snapshot_checkout(root, snapshot)
            self.assertEqual(install.package_bytes(copied), {"new.py": b"new"})
            self.assertEqual((snapshot / marketplace.relative_to(root)).read_text(), "{}")
            staged = subprocess.check_output(["git", "ls-files"], cwd=root, text=True)
            self.assertIn("retired.py", staged)
            self.assertNotIn("new.py", staged)

    def test_install_rejects_missing_extra_or_changed_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            source, installed = (Path(directory) / name for name in ("source", "installed"))
            source.mkdir()
            installed.mkdir()
            (source / "skill.md").write_bytes(b"current\n")
            with self.assertRaisesRegex(ValueError, "missing=.*skill.md"):
                install.verify_installed(source, installed)
            (installed / "skill.md").write_bytes(b"current\r\n")
            self.assertEqual(install.verify_installed(source, installed), 1)
            (installed / "retired.py").write_text("stale")
            with self.assertRaisesRegex(ValueError, "extra=.*retired.py"):
                install.verify_installed(source, installed)
            (installed / "retired.py").unlink()
            (installed / "skill.md").write_text("different")
            with self.assertRaisesRegex(ValueError, "changed=.*skill.md"):
                install.verify_installed(source, installed)

    def test_missing_cli_is_explicit(self):
        with patch.object(install.shutil, "which", return_value=None):
            with self.assertRaisesRegex(ValueError, "CLI unavailable"):
                install.smoke_test()


if __name__ == "__main__":
    unittest.main()

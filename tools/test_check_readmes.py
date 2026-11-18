"""Regression cases for documentation moves and complete effective inventories."""
from pathlib import Path
import subprocess
import tempfile
import unittest

from check_readmes import check, check_document, source_files


class ReadmeChecks(unittest.TestCase):
    def test_move_links_anchors_and_fenced_examples(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "component").mkdir()
            (root / "component/README.md").write_text(
                '# Component\n\n[Guide](../Guide%20One.md#run-it)\n'
                '[Repeat][guide]\n[guide]: ../Guide%20One.md#run-it-1\n'
                '```md\n[Example](missing)\n```\n', encoding="utf-8")
            guide = root / "Guide One.md"
            guide.write_text('# Guide\n## Run it\n## Run it\n', encoding="utf-8")
            self.assertEqual(check_document(root, "component/README.md"), (2, []))
            guide.rename(root / "moved.md")
            self.assertEqual(len(check_document(root, "component/README.md")[1]), 2)

    def test_missing_heading_and_escape_are_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "README.md").write_text(
                '# Component\n[Bad](#invented)\n[Outside](../README.md)\n', encoding="utf-8")
            count, issues = check_document(root, "README.md")
            self.assertEqual(count, 2)
            self.assertTrue(any("missing heading" in item for item in issues))
            self.assertTrue(any("escapes repository" in item for item in issues))

    def test_ignored_local_artifacts_cannot_supply_a_link(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            subprocess.run(["git", "init", "--quiet", str(root)], check=True)
            (root / ".gitignore").write_text("generated/\n", encoding="utf-8")
            for directory in ("generated", "guide"):
                (root / directory).mkdir()
                (root / directory / "index.md").write_text("# Setup\n", encoding="utf-8")
            (root / "README.md").write_text(
                "# Project\n[Generated](generated/index.md#setup)\n"
                "[Generated directory](generated/)\n"
                "[New guide](guide/index.md#setup)\n[Guides](guide/)\n", encoding="utf-8")
            result = check(root, source_files(root))
            self.assertEqual(result["local_links"], 4)
            self.assertEqual(len(result["issues"]), 2)
            self.assertTrue(all("outside effective source: generated/" in item
                                for item in result["issues"]))

    def test_new_readmes_count_and_deleted_inputs_do_not(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "README.md").write_text('# Root\n', encoding="utf-8")
            (root / "new").mkdir()
            (root / "new/README.md").write_text('# New\n', encoding="utf-8")
            result = check(root, ["README.md", "new/README.md", "deleted/README.md"])
            self.assertEqual(result["readmes"], ["README.md", "new/README.md"])
            self.assertEqual(result["issues"], [])


if __name__ == "__main__":
    unittest.main()

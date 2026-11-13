"""Exercise the actual developer Makefile in disposable Git repositories."""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


@unittest.skipUnless(shutil.which("make") and shutil.which("gofmt"),
                     "requires make and gofmt; CI runs the actual Makefile")
class LocalFormatGateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        self.module = self.repo / "kanz"
        self.module.mkdir()
        shutil.copyfile(ROOT / "kanz/Makefile", self.module / "Makefile")
        self.run_command("git", "init", "--quiet")
        self.run_command("git", "config", "core.autocrlf", "false")
        (self.repo / ".gitignore").write_text(".gotmp/\ngen/\n", encoding="utf-8")
        self.write("tracked.go", "package fixture\n\nvar Value = 1\n")
        self.run_command("git", "add", ".")
        self.assert_gate(True)

    def run_command(self, *command):
        return subprocess.run(command, cwd=self.repo, text=True,
                              capture_output=True, check=True)

    def write(self, name, source):
        file = self.module / name
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text(source, encoding="utf-8")
        return file

    def assert_gate(self, success):
        result = subprocess.run(["make", "--no-print-directory", "fmt"],
                                cwd=self.module, text=True, capture_output=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result.stdout + result.stderr

    def test_tracked_and_untracked_files_both_refuse_then_recover(self):
        self.write("tracked.go", "package fixture;var Value=1\n")
        self.write("new file.go", "package fixture;var New=2\n")
        output = self.assert_gate(False)
        self.assertIn("tracked.go", output)
        self.assertIn("new file.go", output)
        self.run_command("gofmt", "-w", "kanz/tracked.go", "kanz/new file.go")
        self.assert_gate(True)

    def test_untracked_file_alone_cannot_hide_behind_clean_tracked_files(self):
        self.write("untracked.go", "package fixture;var New=2\n")
        self.assertIn("untracked.go", self.assert_gate(False))
        self.run_command("gofmt", "-w", "kanz/untracked.go")
        self.assert_gate(True)

    def test_ignored_generated_test_mains_are_not_selected(self):
        self.write(".gotmp/_testmain.go", "deliberately invalid generated input")
        self.write("gen/ignored.go", "deliberately invalid generated input")
        self.assert_gate(True)

    def test_formatter_error_cannot_look_clean(self):
        self.write("invalid.go", "package fixture\nfunc (\n")
        self.assert_gate(False)
        (self.module / "invalid.go").unlink()
        self.assert_gate(True)


if __name__ == "__main__":
    unittest.main()

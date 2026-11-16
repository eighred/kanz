"""Exercise the real schema gate across the old and current source layouts."""

from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "schemas/check-breaking.sh"
BASH = shutil.which("bash")
CONFIG = "version: v2\nmodules:\n  - path: proto\nbreaking:\n  use:\n    - FILE\n"
PROTO = 'syntax = "proto3";\npackage example.v1;\nmessage Value { int64 value = 1; }\n'


@unittest.skipUnless(BASH and shutil.which("buf"), "requires Bash and Buf")
class SchemaBaselineTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="kanz-schema-baseline-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git("init", "-q")
        self.git("config", "core.autocrlf", "false")

    def git(self, *args):
        return subprocess.run(
            ["git", "-C", str(self.root), *args], check=True,
            capture_output=True, text=True,
        ).stdout.strip()

    def module(self, name, proto=PROTO):
        directory = self.root / name
        (directory / "proto").mkdir(parents=True, exist_ok=True)
        (directory / "buf.yaml").write_text(CONFIG, encoding="utf-8")
        (directory / "proto/value.proto").write_text(proto, encoding="utf-8")

    def baseline(self, *names):
        for name in names:
            self.module(name)
        (self.root / "marker").write_text("baseline\n", encoding="utf-8")
        self.git("add", ".")
        self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "commit", "-qm", "baseline")
        self.git("update-ref", "refs/remotes/origin/main", self.git("rev-parse", "HEAD"))
        self.module("schemas")
        shutil.copyfile(SCRIPT, self.root / "schemas/check-breaking.sh")

    def gate(self):
        return subprocess.run(
            [BASH, "schemas/check-breaking.sh"], cwd=self.root,
            capture_output=True, text=True, timeout=60,
        )

    def test_pre_move_main(self):
        self.baseline("kanz-schemas")
        result = self.gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(":kanz-schemas", result.stdout)

    def test_current_main(self):
        self.baseline("schemas")
        result = self.gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(":schemas", result.stdout)

    def test_missing_baseline_refuses(self):
        self.baseline()
        result = self.gate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("refusing missing schema baseline", result.stderr)

    def test_ambiguous_baseline_refuses(self):
        self.baseline("schemas", "kanz-schemas")
        result = self.gate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("refusing ambiguous schema baseline", result.stderr)

    def test_wire_break_still_refuses(self):
        self.baseline("kanz-schemas")
        self.module("schemas", PROTO.replace("int64 value", "int32 value"))
        result = self.gate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('changed type from "int64" to "int32"', result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()

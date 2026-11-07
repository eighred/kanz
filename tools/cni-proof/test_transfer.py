"""Exercise the checked CNI transfer recipe without AWS or cluster calls."""
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
WRAPPER = ROOT / "tools/Test-TestnetCNI.ps1"
SOURCES = ("tools/cni-proof/proof.py", "tools/gateway-policy-proof/network_observation.py")


class CNITransferTests(unittest.TestCase):
    def test_helper_missing_or_changed_cannot_pass_tracked_input_gate(self):
        text = WRAPPER.read_text(encoding="utf-8")
        for verb in ("ls-files --error-unmatch", "diff --quiet $releaseCommit --"):
            line = next(line for line in text.splitlines() if "git -C $repoRoot " + verb in line)
            for source in SOURCES:
                self.assertIn(source, line)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            def git(*args):
                return subprocess.run(["git", "-C", str(root), *args], capture_output=True, timeout=10)
            self.assertEqual(git("init", "--quiet").returncode, 0)
            for source in SOURCES:
                path = root / source; path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes((ROOT / source).read_bytes())
            self.assertEqual(git("add", *SOURCES).returncode, 0)
            self.assertEqual(git("diff", "--quiet", "--", *SOURCES).returncode, 0)
            helper = root / SOURCES[1]
            helper.write_bytes(helper.read_bytes() + b"\n# changed\n")
            self.assertNotEqual(git("diff", "--quiet", "--", *SOURCES).returncode, 0)
            helper.unlink()
            self.assertNotEqual(git("diff", "--quiet", "--", *SOURCES).returncode, 0)
            self.assertNotEqual(git("ls-files", "--error-unmatch", "missing-helper.py").returncode, 0)

    @unittest.skipUnless(sys.platform == "linux", "actual Linux exclusive shell and cleanup")
    def test_fixed_file_collision_and_owned_cleanup(self):
        wrapper = WRAPPER.read_text(encoding="utf-8")
        # Extract the actual literal shell boundary, not a second cleanup recipe.
        def literal(prefix):
            line = next(line.strip() for line in wrapper.splitlines() if line.strip().startswith("'" + prefix))
            return line.rstrip(",")[1:-1].replace("''", "'")
        trap = literal("trap ")
        mkdir = literal("mkdir ")
        exclusive = literal("set -C")
        for collision in (False, True):
            with tempfile.TemporaryDirectory() as temp:
                script = 'set -eu\nwork=$(mktemp -d "$WORK_ROOT/owned.XXXXXXXX")\n' + trap + "\n" + mkdir + "\n" + exclusive + "\n"
                if collision:
                    script += 'printf original > "$work/gateway-policy-proof/network_observation.py"\n'
                script += 'printf checked > "$work/gateway-policy-proof/network_observation.py"\nprintf checked > "$work/cni-proof/proof.py"\n'
                result = subprocess.run(["bash", "--noprofile", "--norc", "-c", script],
                                        env={"PATH": os.environ["PATH"], "WORK_ROOT": temp},
                                        capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, not collision)
                self.assertEqual(list(Path(temp).iterdir()), [])

    @unittest.skipUnless(os.environ.get("KANZ_REQUIRE_NETWORK_FRAMING") == "1", "mandatory dedicated Linux framing step")
    def test_actual_powershell_32_cni_variants(self):
        self.assertIsNotNone(shutil.which("pwsh"))
        wrapper = WRAPPER.read_text(encoding="utf-8")
        stage = wrapper[wrapper.index("$parameters ="):wrapper.index("$commandId =")]
        for ending in ("\n", "\r\n"):
            sources = [(ROOT / path).read_text(encoding="utf-8").replace("\n", ending).encode("utf-8") for path in SOURCES]
            self.assertLessEqual(sum(map(len, sources)), 24576)
            maximum = 0
            for index in range(16):
                prefix = "$ErrorActionPreference='Stop'\n$payload='" + base64.b64encode(sources[0]).decode() + "'\n$networkPayload='" + base64.b64encode(sources[1]).decode() + "'\n$releaseCommit='" + hashlib.sha256(str(index).encode()).hexdigest()[:40] + "'\n"
                with tempfile.TemporaryDirectory() as temp:
                    path = Path(temp) / "measure.ps1"
                    path.write_text(prefix + stage + "\n@{bytes=[Text.Encoding]::UTF8.GetByteCount($parameters);characters=$parameters.Length}|ConvertTo-Json -Compress", encoding="utf-8")
                    result = subprocess.run(["pwsh", "-NoProfile", "-File", str(path)], capture_output=True, text=True, timeout=20)
                    self.assertEqual(result.returncode, 0, "actual bounded serialization refused")
                    value = json.loads(result.stdout)
                    self.assertLessEqual(value["bytes"], 30000); self.assertLessEqual(value["characters"], 30000)
                    maximum = max(maximum, value["bytes"])
            print(json.dumps({"ending": "LF" if ending == "\n" else "CRLF", "sourceBytes": sum(map(len, sources)), "maxNativeParameters": maximum}), flush=True)

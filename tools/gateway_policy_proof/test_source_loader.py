import os
from pathlib import Path
import py_compile
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
LOADER = (HERE / "source_loader.py").read_text()


class SourceLoaderTests(unittest.TestCase):
    def run_python(self, args, cwd):
        return subprocess.run([sys.executable, *args], cwd=cwd, capture_output=True,
                              text=True, timeout=10, check=True).stdout

    def test_valid_dependency_cache_executes_with_B_but_not_source_loader(self):
        for entry, dependency in (("preflight", "bounded_process"), ("image_identity", "rollout_probe"),
                                  ("recovery_host", "recovery_receipts"), ("recovery_plan", "recovery_receipts"), ("recovery_restore", "recovery_receipts")):
            with self.subTest(entry=entry), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                if entry == "image_identity":
                    for name in ("policy_state", "transaction", "rollout_scope"):
                        (root / (name + ".py")).write_bytes((HERE / (name + ".py")).read_bytes())
                if entry in ("recovery_host", "recovery_plan", "recovery_restore"):
                    for name in ("network_observation", "bounded_process", "policy_state", "transaction", "transport_codec", "policy_client", "rollout_scope", "recovery_provenance", "recovery_plan"):
                        (root / (name + ".py")).write_bytes((HERE / (name + ".py")).read_bytes())
                    (root / "json.py").write_text('raise RuntimeError("SHADOW_SENTINEL")\n')
                source = (HERE / (dependency + ".py")).read_bytes()
                path = root / (dependency + ".py")
                malicious = b'print("CACHE_SENTINEL")\n'
                path.write_bytes(malicious + b"#" * (len(source) - len(malicious)))
                stamp = path.stat().st_mtime_ns
                cache = Path(py_compile.compile(str(path), doraise=True))
                path.write_bytes(source)
                os.utime(path, ns=(stamp, stamp))
                cached_bytes = cache.read_bytes()
                (root / (entry + ".py")).write_text("import " + dependency + '\nprint("CHECKED_SOURCE")\n')
                legacy = self.run_python(["-E", "-s", "-B", str(root / (entry + ".py"))], root)
                self.assertIn("CACHE_SENTINEL", legacy)
                isolated = self.run_python(["-I", "-B", "-c", LOADER, str(root), entry], root)
                self.assertEqual(isolated.strip(), "CHECKED_SOURCE")
                self.assertEqual(cache.read_bytes(), cached_bytes)

    def test_repo_and_cwd_stdlib_shadow_cannot_execute(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repo = root / "repo"
            repo.mkdir()
            for path in (root / "json.py", repo / "json.py"):
                path.write_text('raise RuntimeError("SHADOW_SENTINEL")\n')
            (repo / "bounded_process.py").write_text((HERE / "bounded_process.py").read_text())
            (repo / "preflight.py").write_text('import json\nprint(json.dumps({"checked": True}))\n')
            output = self.run_python(["-I", "-B", "-c", LOADER, str(repo), "preflight"], root)
            self.assertEqual(output.strip(), '{"checked": true}')
            self.assertTrue((root / "json.py").exists())
            self.assertTrue((repo / "json.py").exists())

    def test_remote_dependency_closure_ignores_cache_and_shadow(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ("network_observation", "bounded_process", "policy_state", "transaction", "policy_client", "rollout_probe", "rollout_scope"):
                (root / (name + ".py")).write_text((HERE / (name + ".py")).read_text())
            (root / "rollout_engine.py").write_text('import policy_client, rollout_probe, json\nprint("REMOTE_SOURCE")\n')
            (root / "json.py").write_text('raise RuntimeError("SHADOW_SENTINEL")\n')
            self.assertEqual(self.run_python(["-I", "-B", "-c", LOADER, str(root), "rollout_engine"], root).strip(), "REMOTE_SOURCE")

    def test_wrapper_uses_only_isolated_closed_launches(self):
        text = (HERE.parent / "Invoke-TokyoGatewayPolicy.ps1").read_text()
        self.assertNotIn("-E -s -B", text)
        for entry in ("preflight", "image_identity"):
            self.assertIn("& $python -I -B -c $sourceLoader $proofRoot " + entry, text)
        self.assertIn('python3 -I -B `"`$work/source_loader.py`" `"`$work`" rollout_engine', text)
        self.assertIn("if ($encoded.Length -gt 20000)", text)

    def test_actual_entrypoint_import_closures_without_external_calls(self):
        with tempfile.TemporaryDirectory() as directory:
            for entry in ("preflight", "rollout_engine", "recovery_plan", "recovery_restore"):
                output = self.run_python(["-I", "-B", "-c", LOADER, str(HERE), entry, "--help"], directory)
                self.assertIn("usage:", output)
            for args in (["-B", "-c", LOADER, str(HERE), "preflight"],
                         ["-I", "-B", "-c", LOADER, str(HERE), "unapproved"]):
                with self.assertRaises(subprocess.CalledProcessError):
                    self.run_python(args, directory)


if __name__ == "__main__":
    unittest.main()

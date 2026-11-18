"""Failure controls for batch verification, provenance and worker lifecycle."""
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

import compare


class ComparisonTests(unittest.TestCase):
    @unittest.skipUnless(os.name == "nt", "Windows delete-sharing semantics")
    def test_checkpoint_survives_a_brief_external_reader(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "receipt.json"
            compare.checkpoint(path, {"completed": 1})
            reader = path.open()
            release = threading.Timer(0.15, reader.close)
            release.start()
            try:
                compare.checkpoint(path, {"completed": 2})
            finally:
                release.join()
                reader.close()
            self.assertEqual(json.loads(path.read_bytes()), {"completed": 2})
            self.assertEqual(list(path.parent.iterdir()), [path])

    def test_missing_nonfinite_false_zero_and_nondeterminism(self):
        expected = [2.0, 3.0]
        for actual in ([2.0], [float("nan"), 3.0], [0.0, 3.0]):
            with self.subTest(actual=actual), self.assertRaises(ValueError):
                compare.check(actual, expected, [1e-10] * 2, [])
        original = compare.check(expected, expected, [1e-10] * 2, [])
        with self.assertRaisesRegex(ValueError, "nondeterministic"):
            compare.check([2.0 + 1e-12, 3.0], expected, [1e-10] * 2, original)

    def test_cpu_build_binding(self):
        identity = {"Path": "github.com/eighred/kanz/tools/option_gpu_comparison", "GoVersion": "go1.27.4",
                    "Settings": [{"Key": k, "Value": v} for k, v in
                                 (("vcs", "git"), ("vcs.revision", "abc"), ("vcs.modified", "false"))]}
        for field, wrong in (("Path", "another/pkg"), ("GoVersion", "go1.0"), ("Settings", [])):
            info = {**identity, field: wrong}
            with self.subTest(field=field), mock.patch.object(subprocess, "check_output", return_value=json.dumps(info)):
                with self.assertRaisesRegex(ValueError, "mismatch"):
                    compare.cpu_identity(Path("test"), "abc")
        with mock.patch.object(subprocess, "check_output", return_value=json.dumps(identity)):
            with self.assertRaisesRegex(ValueError, "mismatch"):
                compare.cpu_identity(Path("test"), "old")

    def test_process_failure_keeps_input_and_partial_batch_results(self):
        # Exit with the allocation-failure code instead of exhausting host memory.
        # Each child first returns all eight prices to exercise the cold checkpoint.
        original = compare.sample
        _, expected, _ = compare.workload()
        first = "import sys,time; sys.stdin.readline(); print(" + repr(json.dumps(expected)) + ",flush=True);"
        for name, fault in (("exit", "sys.exit(12)"), ("timeout", "time.sleep(30)")):
            def failing_sample(command, request, expected, bounds, previous, gpu, observe):
                return original([sys.executable, "-c", first + fault], request, expected, bounds,
                                previous, False, observe)

            with self.subTest(reason=name), tempfile.TemporaryDirectory() as tmp:
                path = Path(tmp) / "receipt.json"
                with (mock.patch.object(compare, "DEADLINE", 1),
                      mock.patch.object(compare, "cpu_identity", return_value={"package": "fixture"}),
                      mock.patch.object(subprocess, "check_output", side_effect=["a" * 40, b""]),
                      mock.patch.object(compare, "sample", failing_sample),
                      mock.patch.object(sys, "argv", ["compare", "--cpu-worker", sys.executable, "--device",
                                                      "test", "--output", str(path), "--samples", "1"])):
                    self.assertEqual(compare.main(), 1)
                record = json.loads(path.read_bytes())
                self.assertEqual(record["status"], "NONFINAL")
                self.assertEqual(len(record["inputs_sha256"]), 5)
                self.assertEqual(len(record["request_sha256"]), 64)
                self.assertEqual([b["phase"] for b in record["batches"]], ["cold"])
                self.assertEqual(len(record["prices_hex"]["cpu"]), 8)
                self.assertNotIn("p99_ms", record)
                fail = record["failure"]
                self.assertEqual((fail["backend"], fail["sample"], fail["phase"]), ("cpu", 1, "warm"))
                if name == "exit":
                    self.assertEqual(fail["returncode"], 12)
                else:
                    self.assertEqual(fail["type"], "TimeoutError")


if __name__ == "__main__":
    unittest.main()

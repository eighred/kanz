"""Failure controls for batch verification and worker lifecycle."""
import contextlib
import io
import json
import sys
import unittest
from unittest import mock

import compare


class ComparisonTests(unittest.TestCase):
    def test_missing_nonfinite_false_zero_and_nondeterminism(self):
        expected = [2.0, 3.0]
        for actual in ([2.0], [float("nan"), 3.0], [0.0, 3.0]):
            with self.subTest(actual=actual), self.assertRaises(ValueError):
                compare.check(actual, expected, [1e-10] * 2, [])
        original = compare.check(expected, expected, [1e-10] * 2, [])
        with self.assertRaisesRegex(ValueError, "nondeterministic"):
            compare.check([2.0 + 1e-12, 3.0], expected, [1e-10] * 2, original)

    def test_process_failure_keeps_input_and_model_provenance(self):
        # Use a child to exercise EOF/error and real timeout cleanup. Do not
        # exhaust host memory to test a worker that exits on allocation failure.
        for reason, code in (("allocation failure", "raise SystemExit(12)"),
                             ("timeout", "import time; time.sleep(30)")):
            def failing_sample(command, request, expected, bounds, previous, gpu):
                return original([sys.executable, "-c", code], request, expected, bounds, previous, False)

            original = compare.sample
            out = io.StringIO()
            with (self.subTest(reason=reason), mock.patch.object(compare, "DEADLINE", 1),
                  mock.patch.object(compare, "sample", failing_sample),
                  mock.patch.object(sys, "argv", ["compare", "--cpu-worker", sys.executable,
                                                  "--device", "test", "--samples", "1"]),
                  contextlib.redirect_stdout(out)):
                self.assertEqual(compare.main(), 1)
                record = json.loads(out.getvalue())
                self.assertEqual(record["status"], "NONFINAL")
                self.assertEqual(len(record["inputs_sha256"]), 5)
                self.assertEqual(len(record["request_sha256"]), 64)
                self.assertNotIn("p99_ms", record)


if __name__ == "__main__":
    unittest.main()

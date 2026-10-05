import json
from contextlib import ExitStack, redirect_stderr, redirect_stdout
import io
import subprocess
import unittest
from unittest.mock import Mock

from go_mod_download import download_errors, fetch, retryable


RESET = 'read "https://proxy.golang.org/example/module/@v/v1.0.0.zip": stream error: stream ID 1; INTERNAL_ERROR; received from peer'


def result(error=None, stderr=""):
    body = {"Path": "example/module", "Version": "v1.0.0"}
    if error:
        body["Error"] = error
    return subprocess.CompletedProcess([], 1 if error else 0, json.dumps(body), stderr)


class DownloadTests(unittest.TestCase):
    def setUp(self):
        # Injected failures must not create real Actions error annotations.
        output = ExitStack()
        output.enter_context(redirect_stdout(io.StringIO()))
        output.enter_context(redirect_stderr(io.StringIO()))
        self.addCleanup(output.close)

    def test_reset_recovers_with_bounded_retry(self):
        run = Mock(side_effect=[result(RESET), result()])
        delay = Mock()
        self.assertEqual(fetch(run, delay), 0)
        self.assertEqual(run.call_count, 2)
        delay.assert_called_once_with(5)
        self.assertEqual(run.call_args.args[0], ["go", "mod", "download", "-json"])

    def test_exhausted_transfers_fail(self):
        run = Mock(return_value=result(RESET))
        self.assertEqual(fetch(run, Mock()), 1)
        self.assertEqual(run.call_count, 3)

    def test_checksum_auth_and_unknown_errors_are_terminal(self):
        for error in (
            "checksum mismatch SECURITY ERROR", "https://proxy.example: 403 Forbidden",
            "https://proxy.example: 401 Unauthorized", "invalid version", "compilation failed",
            RESET + "; checksum mismatch",
        ):
            with self.subTest(error=error):
                run = Mock(return_value=result(error))
                self.assertEqual(fetch(run, Mock()), 1)
                self.assertEqual(run.call_count, 1)

    def test_mixed_failures_and_extra_diagnostics_are_terminal(self):
        self.assertFalse(retryable([RESET, "invalid module"], ""))
        self.assertFalse(retryable([RESET], "SECURITY ERROR"))

    def test_download_timeouts_are_bounded(self):
        run = Mock(side_effect=subprocess.TimeoutExpired(["go", "mod", "download"], 180))
        self.assertEqual(fetch(run, Mock()), 1)
        self.assertEqual(run.call_count, 3)

    def test_malformed_output_fails_without_retry(self):
        run = Mock(return_value=subprocess.CompletedProcess([], 1, "not json", ""))
        self.assertEqual(fetch(run, Mock()), 1)
        self.assertEqual(run.call_count, 1)

    def test_json_objects_are_not_assumed_to_be_one_per_line(self):
        output = json.dumps({"Error": RESET}, indent=2) + "\n" + json.dumps({"Path": "ok"})
        self.assertEqual(download_errors(output), [RESET])


if __name__ == "__main__":
    unittest.main()

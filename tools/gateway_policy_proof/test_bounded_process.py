import os
import sys
import tempfile
import time
import unittest
import io
import json
import subprocess
from unittest.mock import patch

import bounded_process as bounded
from bounded_process import run, command_failure


class BoundedProcessTests(unittest.TestCase):
    def fake_failure(self, mode):
        clock = [0.0]
        kills = []
        threads = []
        class Process:
            pid = 12345
            returncode = None
            def __init__(self):
                self.stdin = io.BytesIO()
                self.stdout = io.BytesIO(b"private-output" if mode == "nonzero" else b"x" * 65537)
                self.stderr = io.BytesIO(b"x" * 65537 if mode == "stderr" else b"private-stderr")
                self.waits = 0
            def wait(self, timeout):
                self.waits += 1
                if mode in ("deadline", "cleanup", "cleanup_later", "unreaped") and (self.waits == 1 or mode == "unreaped"):
                    clock[0] += timeout
                    raise subprocess.TimeoutExpired("private-command", timeout)
                self.returncode = 7 if mode == "nonzero" else (0 if mode == "joined" else -9)
                return self.returncode
            def poll(self):
                return self.returncode
            def kill(self):
                kills.append("kill")
        class Thread:
            def __init__(self, target, args=(), daemon=False):
                self.target, self.args, self.ident = target, args, None
                self.index = len(threads)
                threads.append(self)
            def start(self):
                self.ident = 1
                self.target(*self.args)
            def join(self, timeout):
                if mode == "cleanup" or (mode == "cleanup_later" and self.index == 1):
                    raise OSError("private-cleanup")
            def is_alive(self):
                return mode == "joined"
        process = Process()
        limit = 65536 if mode == "stdout" else 4 << 20
        with patch.object(bounded.subprocess, "Popen", return_value=process), \
             patch.object(bounded.threading, "Thread", Thread), \
             patch.object(bounded.time, "monotonic", side_effect=lambda: clock[0]), \
             patch.object(bounded.os, "killpg", side_effect=lambda *args: kills.append("killpg"), create=True):
            with self.assertRaises(RuntimeError) as raised:
                run(["private-command"], cwd="private-cwd", env={"PRIVATE": "private-env"}, timeout=1, stdout_limit=limit)
        self.assertIs(type(raised.exception), RuntimeError)
        value = command_failure(raised.exception)
        for private in ("private-command", "private-output", "private-stderr", "private-cleanup", "private-cwd", "private-env"):
            self.assertNotIn(private, json.dumps(value))
        return raised.exception, value, kills

    def test_fake_deadline_preserves_error_and_observed_child_facts(self):
        error, value, kills = self.fake_failure("deadline")
        self.assertEqual(str(error), "bounded command deadline exceeded")
        self.assertEqual((value["outcome"], value["timeoutUs"], value["elapsedUs"]), ("deadline", 1000000, 1000000))
        self.assertTrue(value["launched"] and value["killRequested"] and value["reaped"])
        self.assertEqual((value["signal"], value["exitCode"], value["joinedStreams"]), (9, None, 3))
        self.assertFalse(value["cleanupFailed"])
        self.assertEqual(len(kills), 1)

    def test_fake_nonzero_retains_counts_without_kill_or_raw_streams(self):
        error, value, kills = self.fake_failure("nonzero")
        self.assertEqual(str(error), "bounded command failed or exceeded output limits")
        self.assertEqual((value["outcome"], value["exitCode"], value["signal"]), ("nonzero", 7, None))
        self.assertEqual((value["stdoutBytes"], value["stderrBytes"]), (14, 14))
        self.assertFalse(value["killRequested"])
        self.assertEqual(kills, [])

    def test_fake_overflow_distinguishes_request_from_reap(self):
        for mode in ("stdout", "stderr"):
            with self.subTest(mode=mode):
                _, value, kills = self.fake_failure(mode)
                self.assertEqual(value["outcome"], mode + "_limit")
                self.assertEqual((value[mode + "Bytes"], value[mode + "LimitBytes"]), (65537, 65536))
                self.assertTrue(value["killRequested"] and value["reaped"])
                self.assertEqual(len(kills), 1)

    def test_fake_cleanup_failure_keeps_initial_deadline_and_uncertainty(self):
        for mode, observed in (("cleanup", 0), ("cleanup_later", 1)):
            with self.subTest(mode=mode):
                error, value, _ = self.fake_failure(mode)
                self.assertEqual(str(error), "bounded command deadline exceeded")
                self.assertEqual(value["outcome"], "deadline")
                self.assertTrue(value["cleanupFailed"])
                self.assertEqual(value["joinedStreams"], observed)

    def test_kill_request_without_reap_is_not_reported_as_child_exit(self):
        error, value, kills = self.fake_failure("unreaped")
        self.assertEqual(str(error), "bounded command deadline exceeded")
        self.assertTrue(value["killRequested"] and value["cleanupFailed"])
        self.assertFalse(value["reaped"])
        self.assertEqual(value["joinedStreams"], 0)
        self.assertEqual((value["exitCode"], value["signal"]), (None, None))
        self.assertEqual(len(kills), 2)

    def test_child_reap_does_not_claim_stream_join_or_process_group_absence(self):
        error, value, _ = self.fake_failure("joined")
        self.assertEqual(str(error), "bounded command failed or exceeded output limits")
        self.assertEqual(value["outcome"], "stream_join_timeout")
        self.assertTrue(value["reaped"] and value["cleanupFailed"])
        self.assertEqual(value["joinedStreams"], 0)

    def test_fake_launch_failure_preserves_os_error_without_inventing_child(self):
        original = PermissionError(13, "private-launch")
        with patch.object(bounded.subprocess, "Popen", side_effect=original):
            with self.assertRaises(PermissionError) as raised:
                run(["private-command"], cwd="private-cwd", env={})
        self.assertIs(raised.exception, original)
        value = command_failure(original)
        self.assertEqual(value["outcome"], "launch_error")
        self.assertFalse(value["launched"] or value["killRequested"] or value["reaped"])
        self.assertEqual((value["joinedStreams"], value["exitCode"], value["signal"]), (0, None, None))
        self.assertNotIn("private", json.dumps(value))

    def test_projection_ignores_untyped_error_payload(self):
        error = RuntimeError("private-error")
        error._bounded_command_observation = {"stdout": "private-output"}
        self.assertIsNone(command_failure(error))

    def test_private_projection_rejects_raw_and_non_scalar_payload_before_retention(self):
        error, value, _ = self.fake_failure("deadline")
        for key, replacement in (("rawArgv", ["private"]), ("stdoutBytes", True),
                                 ("elapsedUs", float("inf")), ("outcome", "private")):
            values = {**value, key: replacement}
            error._bounded_command_observation = bounded._CommandObservation(**values)
            with self.subTest(key=key), self.assertRaises(ValueError): command_failure(error)

    def execute(self, program, **kwargs):
        with tempfile.TemporaryDirectory(prefix="kanz-policy-cli-") as directory:
            env = {key: os.environ[key] for key in ("PATH", "SYSTEMROOT") if key in os.environ}
            env.update(HOME=directory, TMPDIR=directory)
            return run([sys.executable, "-I", "-B", "-c", program], cwd=directory, env=env, **kwargs)

    def test_source_capture_preserves_exact_whitespace_when_requested(self):
        program = "import sys; sys.stdout.buffer.write(b' first\\r\\nlast\\n')"
        self.assertEqual(self.execute(program, strip=False), " first\r\nlast\n")
        self.assertEqual(self.execute(program), "first\r\nlast")

    def test_complete_bounded_input_output(self):
        self.assertEqual(self.execute("import sys; print(sys.stdin.read())", data="fixture", timeout=3), "fixture")

    def test_stdout_and_stderr_overflow_refuse(self):
        for stream in ("stdout", "stderr"):
            with self.subTest(stream=stream), self.assertRaises(RuntimeError):
                self.execute("import sys; sys." + stream + ".write('x'*100000)", timeout=3, stdout_limit=1024, stderr_limit=1024)

    def test_deadline_also_bounds_blocked_stdin(self):
        start = time.monotonic()
        with self.assertRaises(RuntimeError):
            self.execute("import time; time.sleep(30)", data="x" * 1000000, timeout=.2)
        self.assertLess(time.monotonic() - start, 5)


if __name__ == "__main__":
    unittest.main()

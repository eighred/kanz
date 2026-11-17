"""Bounded local subprocess controls; run inside the reviewed Linux sandbox."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import qualify


class CommandBoundary(unittest.TestCase):
    def child(self, source, **kwargs):
        return qualify.command(sys.executable, "-B", "-I", "-c", source, **kwargs)

    def test_missing_scope_refuses_before_starting_any_process(self):
        with patch.object(qualify.subprocess, "Popen") as start:
            with self.assertRaises(RuntimeError):
                self.child("pass")
            start.assert_not_called()

    def test_stdio_is_separate_bounded_bytes_and_stdin_is_closed(self):
        with tempfile.TemporaryDirectory() as directory, qualify.command_scope(directory):
            result = self.child("import sys; assert sys.stdin.buffer.read() == b''; "
                                "sys.stdout.buffer.write(b'out'); sys.stderr.buffer.write(b'err')")
            self.assertEqual((result.returncode, result.stdout, result.stderr), (0, b"out", b"err"))

    def test_environment_is_an_explicit_scratch_local_allowlist(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.dict(os.environ, {"KANZ_TEST_AMBIENT_MARKER": "dummy-not-forwarded"}), \
                qualify.command_scope(directory):
            actual = json.loads(self.child("import json,os; print(json.dumps(dict(os.environ)))").stdout)
            self.assertEqual(set(actual), {"PATH", "LC_ALL", "HOME", "TMPDIR", "XDG_CACHE_HOME",
                                           "DOCKER_CONFIG", "GIT_CONFIG_NOSYSTEM"})
            for key, name in [("HOME", "home"), ("TMPDIR", "tmp"),
                              ("XDG_CACHE_HOME", "cache"), ("DOCKER_CONFIG", "docker_config")]:
                self.assertEqual(actual[key], str(Path(directory) / name))
                self.assertEqual((Path(directory) / name).stat().st_mode & 0o777, 0o700)

    def test_stdout_stderr_and_combined_overflow_each_refuse(self):
        cases = ["os.write(1,b'x'*9000)", "os.write(2,b'x'*9000)",
                 "os.write(1,b'x'*5000); os.write(2,b'x'*5000)"]
        for source in cases:
            with self.subTest(source=source), tempfile.TemporaryDirectory() as directory, \
                    qualify.command_scope(directory), patch.object(qualify, "OUTPUT_LIMIT", 8192):
                with self.assertRaisesRegex(ValueError, "byte limit"):
                    self.child("import os; " + source)

    def test_timeout_reaps_the_owned_child_and_closes_pipes(self):
        processes = []
        original = subprocess.Popen

        def start(*args, **kwargs):
            process = original(*args, **kwargs)
            processes.append(process)
            return process

        with tempfile.TemporaryDirectory() as directory, qualify.command_scope(directory), \
                patch.object(qualify.subprocess, "Popen", side_effect=start):
            with self.assertRaises(subprocess.TimeoutExpired):
                self.child("import time; time.sleep(10)", timeout=.15)
        self.assertEqual(len(processes), 1)
        self.assertIsNotNone(processes[0].returncode)
        self.assertTrue(processes[0].stdout.closed)
        self.assertTrue(processes[0].stderr.closed)

    def test_exited_leader_does_not_leave_descendant_holding_pipes(self):
        # The grandchild retains both pipes for ten seconds. Group cleanup must
        # close them before this one-second command deadline, without a retry.
        source = ("import subprocess,sys; "
                  "subprocess.Popen([sys.executable,'-B','-I','-c','import time; time.sleep(10)']); "
                  "print('parent-exited')")
        with tempfile.TemporaryDirectory() as directory, qualify.command_scope(directory):
            try:
                result = self.child(source, timeout=1)
            except subprocess.TimeoutExpired:
                self.fail("owned descendant retained the command pipes after its leader exited")
            self.assertEqual((result.returncode, result.stdout), (0, b"parent-exited\n"))

    def test_checked_errors_do_not_copy_raw_stderr_into_exception(self):
        source = "import sys; sys.stderr.write('dummy-private-error'); sys.exit(7)"
        with tempfile.TemporaryDirectory() as directory, qualify.command_scope(directory):
            with self.assertRaisesRegex(RuntimeError, "exit 7") as error:
                self.child(source)
            self.assertNotIn("dummy-private-error", str(error.exception))
            result = self.child(source, check=False)
            self.assertEqual((result.returncode, result.stderr), (7, b"dummy-private-error"))

    def test_scope_reset_and_invalid_deadlines_refuse(self):
        with tempfile.TemporaryDirectory() as directory, qualify.command_scope(directory):
            with patch.object(qualify.subprocess, "Popen") as start:
                for timeout in [0, -1, 61, float("nan")]:
                    with self.subTest(timeout=timeout), self.assertRaises(ValueError):
                        self.child("pass", timeout=timeout)
                start.assert_not_called()
        self.assertIsNone(qualify.COMMAND_ENV.get())


if __name__ == "__main__":
    unittest.main()

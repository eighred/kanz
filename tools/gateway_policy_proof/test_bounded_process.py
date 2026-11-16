import os
import sys
import tempfile
import time
import unittest

from bounded_process import run


class BoundedProcessTests(unittest.TestCase):
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

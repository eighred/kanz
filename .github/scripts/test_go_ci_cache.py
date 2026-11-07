import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import go_ci_cache as cache
from go_ci_cache import GO_ENV, identify


class CompilerIdentityTests(unittest.TestCase):
    def setUp(self):
        self.env = {"RUNNER_OS": "Linux", "ImageOS": "ubuntu24", "ImageVersion": "20261101.1.0", "GITHUB_WORKSPACE": str(Path.cwd())}
        self.go = dict.fromkeys(GO_ENV, "")
        self.go.update(GOVERSION="go1.26.10", GOOS="linux", GOMODCACHE=str(Path.home() / "go/pkg/mod"), GOCACHE=str(Path.home() / ".cache/go-build"))

    def test_native_or_compiler_change_separates_prefix(self):
        original = identify(self.env, self.go, "libc=1\ngcc=2\n")
        self.assertEqual(original, identify(self.env, self.go, "gcc=2\nlibc=1\n"))
        self.assertNotEqual(original["fingerprint"], identify(self.env, self.go, "libc=2\ngcc=2\n")["fingerprint"])
        for key in GO_ENV:
            changed = dict(self.go)
            changed[key] += "changed"
            if key in ("GOVERSION", "GOOS", "GOMODCACHE", "GOCACHE"):
                continue
            self.assertNotEqual(original["fingerprint"], identify(self.env, changed, "libc=1\ngcc=2\n")["fingerprint"])

    def test_unknown_identity_and_unsafe_paths_disable_reuse(self):
        for key in ("ImageOS", "ImageVersion", "RUNNER_OS", "GITHUB_WORKSPACE"):
            env = dict(self.env)
            del env[key]
            with self.assertRaises((ValueError, KeyError)):
                identify(env, self.go, "libc=1")
        for value in ("relative", str(Path.cwd()), str(Path.home()), str(Path.home() / ".aws"), str(Path.home() / ".docker"), os.path.abspath(os.sep), str(Path.home()) + "\nextra"):
            for key in ("GOCACHE", "GOMODCACHE"):
                with self.assertRaises(ValueError):
                    identify(self.env, dict(self.go, **{key: value}), "libc=1")
        for packages in ("", "\n"):
            with self.assertRaises(ValueError):
                identify(self.env, self.go, packages)

    def test_unknown_host_and_native_drift_cannot_enable_save(self):
        import json
        for fail, expected, stable in ((True, "", "false"), (False, "incorrect", "false"), (False, identify(self.env, self.go, "libc=1")["fingerprint"], "true")):
            with tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "output"
                env = dict(self.env, GITHUB_OUTPUT=str(output), EXPECTED_FINGERPRINT=expected, HOME=str(Path.home()), USERPROFILE=str(Path.home()))
                with patch.dict(os.environ, env, clear=True), patch.object(cache.sys, "argv", ["go_ci_cache.py", "check"]), patch.object(cache.subprocess, "check_output", side_effect=OSError("unknown") if fail else [json.dumps(self.go).encode(), "libc=1"]):
                    cache.main()
                self.assertIn("stable=" + stable + "\n", output.read_text())
                self.assertNotIn("enabled=true", output.read_text())


if __name__ == "__main__":
    unittest.main()

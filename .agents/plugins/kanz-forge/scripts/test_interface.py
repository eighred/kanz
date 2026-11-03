import contextlib
import io
import sys
import unittest
from unittest.mock import patch

import command
from gaps import missing


class InterfaceTests(unittest.TestCase):
    def help(self, *args):
        out = io.StringIO()
        with patch.object(sys, "argv", ["forge", "/help", *args]), contextlib.redirect_stdout(out):
            self.assertEqual(command.main(), 0)
        return out.getvalue()

    def test_everyday_help_does_not_expose_specialist_tests(self):
        short = self.help()
        for name in ("/work", "/ship", "/gaps", "/update"):
            self.assertIn(name+" ", short)
        self.assertNotIn("/bench ", short)
        self.assertIn("/bench ", self.help("all"))
        self.assertNotIn("/financial ", self.help("all"))

    def test_agent_setup_preserves_custom_profiles(self):
        import tempfile
        from pathlib import Path
        from specialists import profiles
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            self.assertTrue(all(p["state"] == "missing" for p in profiles(root, "")["profiles"]))
            self.assertFalse((root/".codex").exists())
            ready = profiles(root, "setup")
            self.assertEqual(ready, profiles(root, "setup"))
            self.assertTrue(all(p["state"] == "ready" for p in ready["profiles"]))
            role = root/".codex/agents/kanz_mapper.toml"
            role.write_text("user-owned")
            with self.assertRaises(ValueError): profiles(root, "setup")
            self.assertEqual(role.read_text(), "user-owned")

    def test_only_missing_relative_file_is_reported(self):
        doc = """[ok](here.md#anchor) [directory](sub/) [url](https://example.org)
[missing](gone.md) [outside](../../external)
```
[example](also-gone.md)
```
"""
        self.assertEqual(missing(["docs/here.md","docs/sub/file"],[("docs/here.md",doc)]),
                         [{"source":"docs/here.md","line":2,"missing":"docs/gone.md"}])

if __name__ == "__main__":
    unittest.main()

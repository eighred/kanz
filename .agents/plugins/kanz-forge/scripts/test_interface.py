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

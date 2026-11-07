import copy
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import check_feed_handoff_evidence as check


class Evidence(unittest.TestCase):
    def events(self):
        return [{"Action": "pass", "Package": check.PREFIX + check.PACKAGE, "Test": n} for n in sorted(check.NAMES)] + [{"Action": "pass", "Package": check.PREFIX + check.PACKAGE}]

    def raw(self, events):
        return "\n".join(json.dumps(e) for e in events).encode()

    def test_inventory(self):
        events = self.events()
        self.assertEqual(check.checked(self.raw(events))["required"], 8)
        bad = [events[:-1], events + [events[0]], events + [{"Action": "pass", "Package": "foreign"}]]
        for action in ("skip", "fail"):
            changed = copy.deepcopy(events)
            changed[0]["Action"] = action
            bad.append(changed)
        for changed in bad:
            with self.assertRaises(ValueError):
                check.checked(self.raw(changed))

    def test_exact_proof(self):
        expected = check.identity("a" * 40, "b" * 40, "123", "1")
        hashes = {n: "c" * 64 for n in check.BINARIES}
        manifest = {**expected, "binaries": hashes, "kind": "controlled-feed-rehearsal"}
        raw = self.raw(self.events())
        def reader(path, _):
            return json.dumps(manifest).encode() if path.name == "manifest.json" else raw
        with patch.object(check, "inventory", return_value=hashes), patch.object(check, "read_bounded", side_effect=reader):
            proof = check.qualify(Path("proof"), Path("bin"), expected)
            self.assertEqual(proof["tests_sha256"], hashlib.sha256(raw).hexdigest())
            self.assertEqual(proof["result"], {"required": 8, "skipped": 0, "packages": 1})
            for key in ("source", "tree", "run", "attempt", "kind", "binaries"):
                original = manifest[key]
                manifest[key] = {} if key == "binaries" else "wrong"
                with self.assertRaises(ValueError):
                    check.qualify(Path("proof"), Path("bin"), expected)
                manifest[key] = original

    def test_duplicate_json(self):
        with self.assertRaises(ValueError):
            check.checked(b'{"Action":"pass","Action":"pass"}')

    def test_binary_bytes_and_inventory(self):
        # Portable inventory/parser test only; actual opener safeguards remain
        # separately mandatory on Linux below.
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            for name in check.BINARIES:
                (root / name).write_bytes(b"\x7fELF" + name.encode())
            with patch.object(check, "read_bounded", side_effect=lambda p, _: p.read_bytes()):
                first = check.inventory(root)
                self.assertEqual(first["market-ingest"], hashlib.sha256(b"\x7fELFmarket-ingest").hexdigest())
                (root / "market-ingest").write_bytes(b"\x7fELFdifferent")
                self.assertNotEqual(first, check.inventory(root))
                (root / "extra").write_bytes(b"\x7fELF")
                with self.assertRaises(ValueError): check.inventory(root)
                (root / "extra").unlink()
                (root / "market-ingest").write_bytes(b"not-linux")
                with self.assertRaises(ValueError): check.inventory(root)

    def test_invalid_identity(self):
        good = ["a" * 40, "b" * 40, "123", "1"]
        for index, invalid in enumerate(("a" * 39, "g" * 40, "0", "-1")):
            changed = list(good)
            changed[index] = invalid
            with self.assertRaises(ValueError): check.identity(*changed)

    @unittest.skipUnless(sys.platform == "linux", "Linux no-follow/nonblocking reader required")
    def test_actual_linux_opener(self):
        import os
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / "real").write_bytes(b"ok")
            (root / "link").symlink_to(root / "real")
            os.mkfifo(root / "fifo")
            self.assertEqual(check.read_bounded(root / "real", 2), b"ok")
            for name in ("link", "fifo"):
                with self.assertRaises(ValueError):
                    check.read_bounded(root / name, 2)


if __name__ == "__main__":
    unittest.main()

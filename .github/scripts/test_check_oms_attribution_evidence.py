import copy
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
from unittest.mock import patch
import unittest
import check_oms_attribution_evidence as c

class AttributionEvidence(unittest.TestCase):
    def events(self):
        p = "github.com/eighred/kanz/" + c.PACKAGE
        return [{"Action": "pass", "Package": p, "Test": n} for n in sorted(c.NAMES)] + [{"Action": "pass", "Package": p}]
    def raw(self, events):
        return b"\n".join(json.dumps(e).encode() for e in events)
    def test_complete(self):
        self.assertEqual(c.checked(self.raw(self.events())), {"required_passes": 72})
    def test_missing_duplicate_skipped_failed_or_other_package_refuses(self):
        original = self.events()
        variants = [original[1:], original + [original[0]], original + [{"Action": "pass", "Package": "foreign"}]]
        for action in ("skip", "fail"):
            variants.append([{**original[0], "Action": action}] + original[1:])
        variants.append([{**e, "Package": "other"} for e in original])
        for events in variants:
            with self.subTest(events=events[0].get("Action")):
                with self.assertRaises(ValueError): c.checked(self.raw(events))
    def test_duplicate_json_key_refuses(self):
        with self.assertRaises(ValueError): c.checked(b'{"Action":"pass","Action":"skip"}')
    def test_independent_long_parent_and_unknown_domain_cases_required(self):
        self.assertIn("TestRustAttributionScheduleEdges/long-parent-last-supported-index", c.NAMES)
        self.assertIn("TestAttributionCanonicalOutputDomainWitness", c.NAMES)

class AttributionProof(unittest.TestCase):
    events = AttributionEvidence.events
    raw = AttributionEvidence.raw
    def fixture(self):
        raw = self.raw(self.events())
        expected = c.metadata("a" * 40, "b" * 40, "123", "1")
        proof = {**expected, "stream_sha256": hashlib.sha256(raw).hexdigest(), "result": {"required_passes": 72},
                 "build": {"profile": "debug", "features": [], "target": "kanz-rs/target/debug/examples/order-attribution-fixture", "sha256": "c" * 64}}
        return expected, proof, {"attribution.jsonl": raw, "binary.sha256": ("c" * 64 + "\n").encode()}
    def test_proof_bindings(self):
        expected, proof, files = self.fixture()
        # Parser/binding test only: the real Linux file opener is tested below.
        def read(path, limit):
            raw = files[path.name]
            self.assertLessEqual(len(raw), limit)
            return raw
        with patch.object(c, "read_bounded", side_effect=read):
            files["proof.json"] = json.dumps(proof).encode()
            self.assertEqual(c.qualify(Path("unused"), expected), {"required_passes": 72})
            changes = [("source_sha", "d" * 40), ("tree_sha", "d" * 40), ("run_id", "124"), ("run_attempt", "2"),
                       ("stream_sha256", "d" * 64), ("version", True), ("result", {"required_passes": 71}),
                       ("result", {"required_passes": 72.0})]
            for field, value in changes:
                changed = copy.deepcopy(proof); changed[field] = value
                files["proof.json"] = json.dumps(changed).encode()
                with self.subTest(field=field), self.assertRaises(ValueError): c.qualify(Path("unused"), expected)
            for field, value in (("sha256", "d" * 64), ("profile", "release"), ("features", ["other"]), ("target", "other")):
                changed = copy.deepcopy(proof); changed["build"][field] = value
                files["proof.json"] = json.dumps(changed).encode()
                with self.subTest(build=field), self.assertRaises(ValueError): c.qualify(Path("unused"), expected)
            files["proof.json"] = json.dumps(proof).encode()
            files["binary.sha256"] = ("d" * 64 + "\n").encode()
            with self.assertRaises(ValueError): c.qualify(Path("unused"), expected)
            files["binary.sha256"] = ("c" * 64 + "\n").encode()
            files["attribution.jsonl"] += b"\n"
            with self.assertRaises(ValueError): c.qualify(Path("unused"), expected)
    @unittest.skipUnless(sys.platform == "linux", "actual nofollow/nonblocking opener requires Linux")
    def test_linux_regular_symlink_fifo(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); regular = root / "regular"; regular.write_bytes(b"ok")
            self.assertEqual(c.read_bounded(regular, 2), b"ok")
            with self.assertRaises(ValueError): c.read_bounded(regular, 1)
            link = root / "link"; link.symlink_to(regular)
            with self.assertRaises(ValueError): c.read_bounded(link, 2)
            fifo = root / "fifo"; os.mkfifo(fifo)
            with self.assertRaises(ValueError): c.read_bounded(fifo, 2)

if __name__ == "__main__": unittest.main()

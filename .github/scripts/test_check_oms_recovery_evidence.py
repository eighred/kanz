import json
import hashlib
import os
import sys
from unittest.mock import patch
from pathlib import Path
import tempfile
import unittest
import check_oms_recovery_evidence as proof

class RecoveryEvidenceTests(unittest.TestCase):
    def setUp(self):
        # Portable tests exercise parsing/binding through a test-only reader.
        # Actual descriptor/FIFO/symlink safety has separate Linux-only tests.
        self.reader = proof.read_bounded
        def portable(path, limit):
            if not path.is_file() or path.stat().st_size > limit: raise ValueError("test input bound")
            raw = path.read_bytes()
            if len(raw) > limit: raise ValueError("test input bound")
            return raw
        patcher = patch.object(proof, "read_bounded", side_effect=portable)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.binding = proof.metadata("a" * 40, "b" * 40, "123", "1")
        large = "TestRustRecoveryComplete8000History"
        for filename in proof.STREAMS:
            names = proof.NAMES - {large, large + "/go", large + "/rust"} if filename == "recovery.jsonl" else {large, large + ("/go" if filename == "large-go.jsonl" else "/rust")}
            events = [{"Action": "pass", "Package": proof.PREFIX + proof.PACKAGE, "Test": name} for name in sorted(names)]
            events.append({"Action": "pass", "Package": proof.PREFIX + proof.PACKAGE})
            for name in sorted(names):
                text = "actual recovery broker limits: server_max_payload=1048576 stream_max_msg_size=-1\n"
                if name.startswith(large + "/"):
                    text += "actual PostgreSQL lock settings: max_locks_per_transaction=1024 max_connections=100 max_prepared_transactions=0\nactual PostgreSQL role: nosuperuser=true nobypassrls=true\nactual PostgreSQL recovery owner tables: forced_rls=12 expected=12\n"
                if name == large + "/rust": text += "actual Rust recovery process memory: peak_rss_kib=1024 current_rss_kib=512\n"
                if name.startswith("TestRustRecoveryDefaultLockCapacityRefusal/"):
                    text += "actual default PostgreSQL lock settings: max_locks_per_transaction=64 max_connections=100 max_prepared_transactions=0\n"
                if name == "TestRustRecoveryDefaultLockCapacityRefusal/go": text += "canonical Go default-capacity SQLSTATE=53200\n"
                events.append({"Action": "output", "Package": proof.PREFIX + proof.PACKAGE, "Test": name, "Output": text})
            self.write(filename, events)
    def write(self, filename, events):
        (self.root / filename).write_text("".join(json.dumps(e) + "\n" for e in events), encoding="utf-8")
    def test_oversized_files_refuse_before_parse(self):
        for name, limit in (("recovery.jsonl", proof.MAX_STREAM_BYTES), ("proof.json", proof.MAX_PROOF_BYTES)):
            with self.subTest(name=name):
                path = self.root / name
                with path.open("wb") as stream: stream.truncate(limit + 1)
                with self.assertRaises(ValueError): proof.read_bounded(path, limit)
        path = self.root / "boundary"
        path.write_bytes(b"x" * 16)
        self.assertEqual(proof.read_bounded(path, 16), b"x" * 16)
        with self.assertRaises(ValueError): proof.read_bounded(path, 15)
        with self.assertRaises((ValueError, OSError)): proof.read_bounded(self.root, 16)

    def test_hashes_bind_the_validated_snapshot_without_reopening(self):
        original = proof.read_bounded
        reads = []
        before = (self.root / "recovery.jsonl").read_bytes()
        def replace_after_read(path, limit):
            raw = original(path, limit)
            reads.append(path.name)
            if path.name == "recovery.jsonl": path.write_bytes(raw + b"\n")
            return raw
        with patch.object(proof, "read_bounded", side_effect=replace_after_read):
            proof.qualify(self.root, self.binding, True)
        saved = json.loads((self.root / "proof.json").read_text())
        self.assertEqual(saved["streams"]["recovery.jsonl"], hashlib.sha256(before).hexdigest())
        self.assertEqual(reads, list(proof.STREAMS))
        with self.assertRaises(ValueError): proof.qualify(self.root, self.binding)

    def test_complete_bound_proof(self):
        result = proof.qualify(self.root, self.binding, True)
        self.assertEqual(result["required_passes"], 68)
        self.assertEqual(proof.qualify(self.root, self.binding), result)
    def test_missing_or_skipped_case(self):
        for mode in ("missing", "skip"):
            with self.subTest(mode=mode):
                original = proof.events(self.root / "large-rust.jsonl")
                changed = [e for e in original if e.get("Test") != "TestRustRecoveryComplete8000History/rust" or e["Action"] != "pass"]
                if mode == "skip": changed.append({"Action": "skip", "Package": proof.PREFIX + proof.PACKAGE, "Test": "TestRustRecoveryComplete8000History/rust"})
                self.write("large-rust.jsonl", changed)
                with self.assertRaises(ValueError): proof.checked(self.root)
                self.write("large-rust.jsonl", original)
    def test_missing_or_misassigned_artifact_slot(self):
        go = self.root / "large-go.jsonl"
        rust = self.root / "large-rust.jsonl"
        original = go.read_bytes()
        go.write_bytes(rust.read_bytes())
        with self.assertRaises(ValueError): proof.checked(self.root)
        go.write_bytes(original)
        go.rename(self.root / "unexpected-slot.jsonl")
        with self.assertRaises((ValueError, OSError)): proof.checked(self.root)

    def test_duplicate_child(self):
        events = proof.events(self.root / "large-go.jsonl")
        events.append({"Action": "pass", "Package": proof.PREFIX + proof.PACKAGE, "Test": "TestRustRecoveryComplete8000History/go"})
        self.write("large-go.jsonl", events)
        with self.assertRaises(ValueError): proof.checked(self.root)
    def test_resource_role_or_cause_missing(self):
        for marker in ("peak_rss_kib=1024", "nosuperuser=true", "forced_rls=12", "SQLSTATE=53200", "max_locks_per_transaction=1024"):
            with self.subTest(marker=marker):
                old = {name: (self.root / name).read_text() for name in proof.STREAMS}
                for name, text in old.items(): (self.root / name).write_text(text.replace(marker, "removed"))
                with self.assertRaises(ValueError): proof.checked(self.root)
                for name, text in old.items(): (self.root / name).write_text(text)
    def test_source_attempt_content_tamper(self):
        proof.qualify(self.root, self.binding, True)
        for key, value in (("source_sha", "c" * 40), ("tree_sha", "c" * 40), ("run_attempt", "2")):
            with self.subTest(key=key), self.assertRaises(ValueError): proof.qualify(self.root, {**self.binding, key: value})
        with (self.root / "recovery.jsonl").open("a") as stream: stream.write("\n")
        with self.assertRaises(ValueError): proof.qualify(self.root, self.binding)
    def test_duplicate_or_boolean_metadata(self):
        proof.qualify(self.root, self.binding, True)
        path = self.root / "proof.json"
        raw = path.read_text()
        for changed in (raw.replace('"version": 1', '"version": true'), raw.replace('"version": 1', '"version": 1, "version": 1')):
            path.write_text(changed)
            with self.assertRaises(ValueError): proof.qualify(self.root, self.binding)

class RecoveryFileSafetyTests(unittest.TestCase):
    def test_unsupported_platform_refuses(self):
        with patch.object(proof.sys, "platform", "win32"), self.assertRaises(ValueError):
            proof.read_bounded(Path("unused"), 16)

    @unittest.skipUnless(sys.platform == "linux", "Linux no-follow/nonblocking file proof")
    def test_fifo_link_and_bounds(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            regular = root / "regular"
            regular.write_bytes(b"x" * 16)
            self.assertEqual(proof.read_bounded(regular, 16), b"x" * 16)
            with self.assertRaises(ValueError): proof.read_bounded(regular, 15)
            fifo = root / "fifo"
            os.mkfifo(fifo)
            with self.assertRaises(ValueError): proof.read_bounded(fifo, 16)
            linked = root / "linked"
            linked.symlink_to(regular)
            with self.assertRaises(ValueError): proof.read_bounded(linked, 16)

if __name__ == "__main__": unittest.main()

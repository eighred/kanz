import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from go_shard_evidence import SHARD_OWNERS, bounded_bytes, combine, inventory, partition, require_routed_owners, seal, validate_events
from go_test_evidence import BROWSER_PROOF


class ShardEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = {"sha": "a" * 40, "tree": "b" * 40, "run": "123", "attempt": "2"}
        self.packages = [f"github.com/eighred/kanz/pkg/p{i}" for i in range(12)]
        self.packages.extend(SHARD_OWNERS)
        self.packages.sort()
        for shard in range(2):
            d = self.directory(shard)
            d.mkdir()
            selected = partition(self.packages, shard)
            (d / "packages.txt").write_text("\n".join(selected) + "\n", encoding="utf-8")
            rows = []
            for p in selected:
                rows.extend([{"Action": "start", "Package": p},
                             {"Action": "run", "Package": p, "Test": "TestReal"},
                             {"Action": "output", "Package": p, "Test": "TestReal", "Output": BROWSER_PROOF},
                             {"Action": "pass", "Package": p, "Test": "TestReal"},
                             {"Action": "pass", "Package": p}])
            (d / "events.jsonl").write_text("\n".join(map(json.dumps, rows)) + "\n", encoding="utf-8")
            seal(d, shard, self.source)

    def directory(self, shard):
        return self.root / f"go-race-123-2-{shard}"

    def test_complete_disjoint_union(self):
        self.assertEqual(combine(self.root, self.packages, self.source), (len(self.packages), 0))
        self.assertEqual(set(partition(self.packages, 0)) | set(partition(self.packages, 1)), set(self.packages))
        self.assertFalse(set(partition(self.packages, 0)) & set(partition(self.packages, 1)))

    def test_routed_work_is_present_once_and_cannot_become_a_dead_entry(self):
        expected = {"github.com/eighred/kanz/pkg/bus": 1,
                    "github.com/eighred/kanz/services/risk-engine/internal/app": 0}
        routed = set(expected)
        self.assertEqual(SHARD_OWNERS, expected)
        for package, owner in expected.items():
            self.assertIn(package, partition(self.packages, owner))
            self.assertNotIn(package, partition(self.packages, 1 - owner))
        for package in routed:
            incomplete = [p for p in self.packages if p != package]
            with self.assertRaisesRegex(ValueError, "routed package disappeared"):
                require_routed_owners(incomplete)
            with self.assertRaisesRegex(ValueError, "routed package disappeared"):
                combine(self.root, incomplete, self.source)
        for invalid in (True, -1, 2, "0"):
            with patch.dict(SHARD_OWNERS, {next(iter(routed)): invalid}):
                with self.assertRaisesRegex(ValueError, "invalid shard"):
                    require_routed_owners(self.packages)

    def test_source_run_attempt_shard_binding(self):
        p = self.directory(0) / "manifest.json"
        original = json.loads(p.read_text())
        for field, value in {"sha": "c" * 40, "tree": "d" * 40, "run": "456", "attempt": "1", "shard": 1, "count": 3}.items():
            with self.subTest(field=field):
                bad = dict(original, **{field: value})
                p.write_text(json.dumps(bad))
                with self.assertRaises(ValueError):
                    combine(self.root, self.packages, self.source)
        p.write_text(json.dumps(original))

    def test_missing_artifact(self):
        (self.directory(1) / "manifest.json").unlink()
        with self.assertRaises(ValueError):
            combine(self.root, self.packages, self.source)

    def test_duplicate_or_overlap_inventory(self):
        with self.assertRaises(ValueError):
            inventory(self.packages[0] + "\n" + self.packages[0])
        p = self.directory(0) / "packages.txt"
        p.write_text(p.read_text() + partition(self.packages, 1)[0] + "\n")
        with self.assertRaises(ValueError):
            combine(self.root, self.packages, self.source)

    def test_resealed_wrong_partition_is_not_a_hash_only_check(self):
        for name in ("packages.txt", "events.jsonl"):
            (self.directory(0) / name).write_bytes((self.directory(1) / name).read_bytes())
        seal(self.directory(0), 0, self.source)
        with self.assertRaisesRegex(ValueError, "incorrect or overlapping shard inventory"):
            combine(self.root, self.packages, self.source)

    def test_extra_and_missing_shard_directory(self):
        extra = self.root / "go-race-123-1-0"
        extra.mkdir()
        with self.assertRaisesRegex(ValueError, "missing or unexpected shard artifacts"):
            combine(self.root, self.packages, self.source)
        extra.rmdir()
        self.directory(1).rename(self.root / "wrong-shard")
        with self.assertRaisesRegex(ValueError, "missing or unexpected shard artifacts"):
            combine(self.root, self.packages, self.source)

    def test_failed_cached_and_truncated_results(self):
        d = self.directory(0)
        rows = [json.loads(line) for line in (d / "events.jsonl").read_text().splitlines()]
        bad_cases = [rows[:-1], rows + [rows[-1]], rows + [{"Action": "fail", "Package": rows[0]["Package"]}],
                     rows + [{"Action": "output", "Package": rows[0]["Package"], "Output": "ok pkg (cached)"}]]
        for bad in bad_cases:
            with self.subTest(bad=bad[-1]):
                with self.assertRaises(ValueError):
                    validate_events("\n".join(map(json.dumps, bad)), partition(self.packages, 0))

    def test_changed_content_hash(self):
        p = self.directory(0) / "events.jsonl"
        p.write_text(p.read_text() + "\n")
        with self.assertRaises(ValueError):
            combine(self.root, self.packages, self.source)

    def test_bounds_refuse_whole_input(self):
        path = self.directory(0) / "manifest.json"
        path.write_bytes(b" " * 4097)
        with self.assertRaises(ValueError):
            combine(self.root, self.packages, self.source)
        path.write_bytes(b"123456789")
        with self.assertRaises(ValueError):
            bounded_bytes(path, 8)
        with self.assertRaises(ValueError):
            inventory("\n".join(f"github.com/eighred/kanz/p{i}" for i in range(10001)))

    def test_shard_identity_must_be_an_explicit_integer(self):
        for shard in (None, True, -1, 2, "0"):
            with self.assertRaises(ValueError):
                partition(self.packages, shard)

    def test_supplied_dependency_skip_and_missing_browser(self):
        for kind in ("skip", "browser"):
            with self.subTest(kind=kind):
                originals = {}
                for shard in range(2):
                    d = self.directory(shard)
                    p = d / "events.jsonl"
                    originals[shard] = p.read_text()
                    rows = [json.loads(line) for line in originals[shard].splitlines()]
                    for e in rows:
                        if e["Action"] == "output":
                            e["Output"] = "TEST_POSTGRES_URL missing" if kind == "skip" else "ordinary output"
                        if kind == "skip" and e["Action"] == "pass" and e.get("Test"):
                            e["Action"] = "skip"
                    p.write_text("\n".join(map(json.dumps, rows)) + "\n")
                    seal(d, shard, self.source)
                with self.assertRaises(ValueError):
                    combine(self.root, self.packages, self.source)
                for shard, text in originals.items():
                    (self.directory(shard) / "events.jsonl").write_text(text)
                    seal(self.directory(shard), shard, self.source)


if __name__ == "__main__":
    unittest.main()

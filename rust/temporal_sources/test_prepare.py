import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import prepare


class SourcePreparationTests(unittest.TestCase):
    def setUp(self):
        owned = tempfile.TemporaryDirectory()
        self.addCleanup(owned.cleanup)
        self.root = Path(owned.name)
        self.recipe = self.root / "recipe"
        (self.recipe / "patches").mkdir(parents=True)
        self.key = "example-1.2.3"
        self.input = self.root / f"{self.key}.tar.gz"
        self.manifest = b'[package]\nname = "example"\nversion = "1.2.3"\n'
        self.archive({"Cargo.toml": self.manifest, "lib.rs": b"old\n"})
        delta = self.recipe / "patches" / f"{self.key}.patch"
        delta.write_bytes(b"--- a/lib.rs\n+++ b/lib.rs\n@@ -1 +1 @@\n-old\n+new\n")
        self.row = {
            "name": "example", "version": "1.2.3",
            "manifestPath": f"rust/temporal_sources/generated/{self.key}/Cargo.toml",
            "upstream": {"sha256": prepare.digest(self.input), "fileTreeSHA256": self.tree(b"old\n")},
            "patchSHA256": prepare.digest(delta), "treeSHA256": self.tree(b"new\n"),
        }
        self.work = self.root / "work"
        self.work.mkdir()
        here = patch.object(prepare, "HERE", self.recipe)
        here.start()
        self.addCleanup(here.stop)

    def tree(self, lib):
        return hashlib.sha256(b"Cargo.toml\0" + hashlib.sha256(self.manifest).digest()
                              + b"lib.rs\0" + hashlib.sha256(lib).digest()).hexdigest()

    def archive(self, files):
        with tarfile.open(self.input, "w:gz") as tar:
            for path, data in files.items():
                member = tarfile.TarInfo(f"{self.key}/{path}")
                member.size = len(data)
                tar.addfile(member, io.BytesIO(data))

    def test_offline_exact_source_and_changed_final_tree(self):
        prepare.materialize(self.row, self.work, self.root)
        prepare.verify(self.work, [self.row])
        (self.work / self.key / "lib.rs").write_bytes(b"changed\n")
        with self.assertRaisesRegex(ValueError, "tree differs"):
            prepare.verify(self.work, [self.row])

    def test_unknown_source_trees_and_coordinates_refuse(self):
        for location in ("source", "delta", "result", "coordinate"):
            with self.subTest(location=location):
                row = json.loads(json.dumps(self.row))
                work = self.root / location
                work.mkdir()
                if location == "source":
                    row["upstream"]["sha256"] = "0" * 64
                elif location == "delta":
                    row["patchSHA256"] = "0" * 64
                elif location == "result":
                    row["treeSHA256"] = "0" * 64
                else:
                    row["manifestPath"] = "elsewhere/Cargo.toml"
                with self.assertRaises(ValueError):
                    prepare.materialize(row, work, self.root)

    def test_archive_path_cannot_escape_the_owned_directory(self):
        self.archive({"../outside": b"escape"})
        with self.assertRaisesRegex(ValueError, "unsafe"):
            prepare.unpack(self.input, self.work)
        self.assertFalse((self.root / "outside").exists())

    def test_reusing_prepared_sources_rechecks_the_reviewed_patch(self):
        prepare.materialize(self.row, self.work, self.root)
        (self.recipe / "patches" / f"{self.key}.patch").write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "patch differs"):
            prepare.verify(self.work, [self.row])


if __name__ == "__main__":
    unittest.main()

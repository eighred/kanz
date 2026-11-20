import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from chisel_verify import PACKAGES, names, scanner_packages, verify_packages


class ChiselTests(unittest.TestCase):
    def test_each_reviewed_archive_identity_is_required(self):
        reviewed = {name: {"arch": arch, "version": version, "sha256": digest}
                    for name, (arch, version, digest) in PACKAGES.items()}
        verify_packages(reviewed)
        for name, row in reviewed.items():
            for field, other in (("arch", "arm64"), ("version", "old"), ("sha256", "0" * 64)):
                with self.subTest(name=name, field=field), self.assertRaisesRegex(ValueError, "archive identity"):
                    verify_packages({**reviewed, name: {**row, field: other}})
        with self.assertRaisesRegex(ValueError, "package set"):
            verify_packages({name: row for name, row in reviewed.items() if name != "openssl"})

    def test_duplicate_layer_paths_are_rejected(self):
        packed = io.BytesIO()
        with tarfile.open(fileobj=packed, mode="w") as out:
            for name in ("./var/lib/chisel/manifest.wall", "var/lib/chisel/manifest.wall"):
                out.addfile(tarfile.TarInfo(name))
        packed.seek(0)
        with tarfile.open(fileobj=packed) as data, self.assertRaisesRegex(ValueError, "duplicate"):
            names(data)

    def test_both_scanners_must_cover_the_actual_package_revision(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            version = "2.39-0ubuntu8.12"
            packages = {"libc6": {"version": version}}
            trivy = {"Type": "ubuntu", "Class": "os-pkgs", "Packages": [{"Name": "libc6", "ID": "libc6@" + version}]}
            syft = {"type": "deb", "name": "libc6", "version": version}
            (directory / "image.json").write_text(json.dumps({"Results": [trivy]}))
            for rows in ([syft], [], [dict(syft, version="2.39")], [syft, syft]):
                with self.subTest(rows=rows):
                    (directory / "syft.json").write_text(json.dumps({"artifacts": rows}))
                    if rows == [syft]:
                        scanner_packages(directory, packages)
                    else:
                        with self.assertRaisesRegex(ValueError, "coverage"):
                            scanner_packages(directory, packages)
            (directory / "syft.json").write_text(json.dumps({"artifacts": [syft]}))
            trivy["Packages"] = []
            (directory / "image.json").write_text(json.dumps({"Results": [trivy]}))
            with self.assertRaisesRegex(ValueError, "coverage"):
                scanner_packages(directory, packages)

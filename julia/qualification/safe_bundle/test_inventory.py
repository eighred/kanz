import io
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

from inventory import inspect_tar


class BundleInventoryTests(unittest.TestCase):
    def inventory(self, entries):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            archive = root / "candidate.tar"
            with tarfile.open(archive, "w") as packed:
                for name, content in entries:
                    member = tarfile.TarInfo(name)
                    if isinstance(content, bytes):
                        member.size = len(content)
                        packed.addfile(member, io.BytesIO(content))
                    else:
                        member.type = tarfile.SYMTYPE
                        member.linkname = content
                        packed.addfile(member)
            return inspect_tar(archive, root / "elf")

    def test_canonical_internal_link(self):
        app, native = self.inventory([("bin/curve-worker", Path("/bin/true").read_bytes()),
                                      ("bin/worker", "curve-worker")])
        self.assertIn("bin/curve-worker", native)
        self.assertEqual(app["bin/worker"], ["symlink", "curve-worker"])

    def test_unexpected_library_or_link(self):
        for name, content in [("lib/libLLVM.so", b"x"), ("lib/alias", "libjulia-codegen.so"),
                              ("bin/alias", "../../outside"), ("bin/alias", "alias")]:
            with self.subTest(name=name, content=content), self.assertRaises(ValueError):
                self.inventory([("bin/curve-worker", b"stub"), (name, content)])

    def test_renamed_compiler_soname(self):
        with tempfile.TemporaryDirectory() as tmp:
            library = Path(tmp) / "other.so"
            subprocess.run(["cc", "-shared", "-x", "c", "-Wl,-soname,libLLVM.so.fixture",
                            "-o", library, "-"], input=b"int fixture(void){return 0;}", check=True)
            with self.assertRaisesRegex(ValueError, "compiler library"):
                self.inventory([("bin/curve-worker", b"stub"), ("lib/other.so", library.read_bytes())])

import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import qualify as q


class PayloadTests(unittest.TestCase):
    def payload(self):
        files = {"manifest.json": json.dumps(q.manifest("a" * 40)).encode(),
                 "tools/cargo-audit": b"binary-a", "tools/rust-audit-info": b"binary-b"}
        for name, version in q.TOOLS.items():
            prefix = f"notices/{name}-{version}/"
            files[prefix + "package.toml"] = f'[package]\nname="{name}"\nversion="{version}"\nlicense="MIT OR Apache-2.0"\n'.encode()
            files[prefix + "LICENSE-MIT"] = b"public test notice"
        for name in ("COPYRIGHT.html", "COPYRIGHT-library.html", "licenses/Apache-2.0.txt", "licenses/MIT.txt"):
            files["notices/rust-1.90.0/" + name] = b"public Rust notice"
        return files

    def inspect(self, files, platform="amd64", linked=False):
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode="w") as tar:
            for name, data in files.items():
                item = tarfile.TarInfo(name)
                if linked and name == "tools/cargo-audit":
                    item.type = tarfile.SYMTYPE
                    item.linkname = "/outside"
                else:
                    item.size = len(data)
                tar.addfile(item, io.BytesIO(data))
        info = [{"Os": "linux", "Architecture": platform, "Id": "sha256:" + "b" * 64,
                 "Config": {"Labels": {"org.opencontainers.image.revision": "a" * 40}}}]
        def invoke(args, **kwargs):
            if args[1] == "export":
                kwargs["stdout"].write(buffer.getvalue())
            return subprocess.CompletedProcess(args, 0)
        with tempfile.TemporaryDirectory() as directory, patch.object(q, "output", side_effect=[json.dumps(info), "c" * 64]), patch.object(q, "run", side_effect=invoke):
            return q.extract("image", Path(directory), "a" * 40)

    def test_actual_inventory_parser(self):
        _, files = self.inspect(self.payload())
        self.assertEqual(len(files), 11)

    def test_leaks_links_platform_and_source_refuse(self):
        for name in ("kanz/go.mod", "notices/hidden/private.rs", ".cargo/registry/source", "../private", "/absolute"):
            with self.subTest(name=name):
                files = self.payload(); files[name] = b"private"
                with self.assertRaises(ValueError): self.inspect(files)
        with self.assertRaises(ValueError): self.inspect(self.payload(), linked=True)
        with self.assertRaises(ValueError): self.inspect(self.payload(), platform="arm64")
        files = self.payload(); files["manifest.json"] = json.dumps(q.manifest("d" * 40)).encode()
        with self.assertRaises(ValueError): self.inspect(files)

    def test_missing_tools_notices_and_duplicate_manifest_refuse(self):
        for name in ("tools/cargo-audit", "notices/rust-audit-info-0.5.4/LICENSE-MIT", "notices/rust-1.90.0/COPYRIGHT-library.html"):
            files = self.payload(); del files[name]
            with self.assertRaises(ValueError): self.inspect(files)
        files = self.payload(); files["manifest.json"] = b'{"version":1,"version":1}'
        with self.assertRaises(ValueError): self.inspect(files)

    @unittest.skipUnless(sys.platform == "linux", "actual Linux opener required")
    def test_linux_regular_fifo_symlink_and_size(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); regular = root / "regular"; regular.write_bytes(b"abc")
            self.assertEqual(q.read_bounded(regular, 3), b"abc")
            with self.assertRaises(ValueError): q.read_bounded(regular, 2)
            fifo = root / "fifo"; os.mkfifo(fifo)
            with self.assertRaises(ValueError): q.read_bounded(fifo, 3)
            link = root / "link"; link.symlink_to(regular)
            with self.assertRaises(OSError): q.read_bounded(link, 3)


if __name__ == "__main__":
    unittest.main()

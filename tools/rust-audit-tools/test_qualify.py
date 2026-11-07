import io
import hashlib
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest

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

    def archive(self, files, platform="amd64", linked=False, fault=None, oci=False):
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
        layer = buffer.getvalue()
        config = {"os": "linux", "architecture": platform,
                  "config": {"Labels": {"org.opencontainers.image.revision": "a" * 40}},
                  "rootfs": {"type": "layers", "diff_ids": ["sha256:" + hashlib.sha256(layer).hexdigest()]},
                  "history": [{"created_by": "COPY /payload/ / # buildkit"}]}
        if fault == "diffid": config["rootfs"]["diff_ids"] = ["sha256:" + "0" * 64]
        if fault == "history": config["history"] = [{"created_by": "RUN hidden source"}]
        config_bytes = json.dumps(config).encode()
        image = "sha256:" + hashlib.sha256(config_bytes).hexdigest()
        m = [{"Config": "config.json", "Layers": ["layer.tar"], "RepoTags": [q.local_tag("a" * 40)]}]
        if fault == "tag": m[0]["RepoTags"] = ["untrusted:latest"]
        if fault == "layers": m[0]["Layers"].append("layer.tar")
        if fault == "manifests": m.append(m[0])
        entries = {"manifest.json": json.dumps(m).encode(), "config.json": config_bytes, "layer.tar": layer}
        if fault == "config": entries["config.json"] += b" "
        if fault == "layer": entries["layer.tar"] += b" "
        if fault == "repositories": entries["repositories"] = json.dumps({"untrusted": {"latest": "0" * 64}}).encode()
        if fault == "repositories-valid": entries["repositories"] = json.dumps({"kanz-rust-audit-tools-local": {"a" * 40: hashlib.sha256(layer).hexdigest()}}).encode()
        if fault == "unreachable": entries["hidden-source.tar"] = b"private"
        if oci:
            def blob(data, media):
                digest = hashlib.sha256(data).hexdigest()
                entries["blobs/sha256/" + digest] = data
                return {"digest": "sha256:" + digest, "size": len(data), "mediaType": media}
            entries = {}
            c = blob(config_bytes, "application/vnd.oci.image.config.v1+json")
            l = blob(layer, "application/vnd.oci.image.layer.v1.tar")
            graph = json.dumps({"schemaVersion": 2, "config": c, "layers": [l]}).encode()
            descriptor = blob(graph, "application/vnd.oci.image.manifest.v1+json")
            if fault == "oci-size": descriptor["size"] += 1
            if fault == "oci-media": descriptor["mediaType"] = "unknown"
            entries["index.json"] = json.dumps({"schemaVersion": 2, "manifests": [descriptor]}).encode()
            entries["oci-layout"] = b'{"imageLayoutVersion":"1.0.0"}'
            entries["manifest.json"] = json.dumps([{"Config": "blobs/sha256/" + c["digest"][7:], "Layers": ["blobs/sha256/" + l["digest"][7:]], "RepoTags": [q.local_tag("a" * 40)]}]).encode()
        outer = io.BytesIO()
        with tarfile.open(fileobj=outer, mode="w") as tar:
            for name, data in entries.items():
                item = tarfile.TarInfo(name); item.size = len(data)
                tar.addfile(item, io.BytesIO(data))
        return outer.getvalue(), image

    def inspect(self, files, **kwargs):
        raw, image = self.archive(files, **kwargs)
        with tempfile.TemporaryDirectory() as directory:
            return q.extract_saved(raw, image, Path(directory), "a" * 40)

    def test_oci_saved_graph(self):
        self.inspect(self.payload(), oci=True)
        for fault in ("oci-size", "oci-media"):
            with self.subTest(fault=fault), self.assertRaises(ValueError):
                self.inspect(self.payload(), oci=True, fault=fault)

    def test_saved_graph_refusals(self):
        for fault in ("tag", "config", "layer", "diffid", "layers", "manifests", "history", "unreachable", "repositories"):
            with self.subTest(fault=fault), self.assertRaises(ValueError):
                self.inspect(self.payload(), fault=fault)

    def test_closed_repository_tag(self):
        self.inspect(self.payload(), fault="repositories-valid")

    def test_actual_inventory_parser(self):
        _, files = self.inspect(self.payload())
        self.assertEqual(len(files), 11)

    def test_leaks_links_platform_and_source_refuse(self):
        for name in ("kanz/go.mod", "notices/hidden/private.rs", "notices/pkg/.wh.LICENSE", ".cargo/registry/source", "../private", "/absolute"):
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

    def test_distinct_build_root_and_config(self):
        root, config = "sha256:" + "a" * 64, "sha256:" + "b" * 64
        value = {"containerimage.digest": root, "containerimage.config.digest": config}
        self.assertEqual(q.build_identity(json.dumps(value).encode(), root), (root, config))
        with self.assertRaises(ValueError): q.build_identity(json.dumps(value).encode(), config)
        for field in value:
            bad = dict(value); bad[field] = "invalid"
            with self.assertRaises(ValueError): q.build_identity(json.dumps(bad).encode(), root)

    def test_registry_descriptors(self):
        image = "sha256:" + "a" * 64
        good = {"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": {"digest": image, "size": 100, "mediaType": "application/vnd.oci.image.config.v1+json"}, "layers": [{"digest": "sha256:" + "b" * 64, "size": 200, "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip"}]}
        q.registry_manifest(good, image)
        mutations = [lambda m: m.update(schemaVersion=True), lambda m: m.update(mediaType="unknown"), lambda m: m["layers"].append(m["layers"][0]), lambda m: m["config"].update(digest="sha256:" + "c" * 64), lambda m: m["layers"][0].update(digest="invalid"), lambda m: m["layers"][0].update(mediaType="unknown"), lambda m: m["layers"][0].update(size=True), lambda m: m["layers"][0].update(size=0), lambda m: m["layers"][0].update(size=q.MAX_PAYLOAD + 1)]
        for mutate in mutations:
            value = json.loads(json.dumps(good)); mutate(value)
            with self.assertRaises(ValueError): q.registry_manifest(value, image)

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

"""Produce a local OCI with Canonical's Chisel rootfs and the reviewed application layers."""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import tarfile

from blas_owners import blob, exact_file


# The independent profile consumes this already qualified, compiler-free application.
REFERENCE = "sha256:a0b0d9f1e6dd6f94c6ad5208d38ae86e42c5d69912b3b935bed935b97b829a39"
DEFINITIONS = "a84c5eb70f22c34212c6582f603467498061c978"
APP_LAYER = "sha256:f3bcc7aa8826ad370c337b876c489ad72b14a68d4a8fbc45dd8ddd41889f1128"
PREFIX = "application/vnd.oci.image."


def sha(data):
    return hashlib.sha256(data).hexdigest()


def encode(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def descriptor(data, kind):
    return {"mediaType": PREFIX + kind, "digest": "sha256:" + sha(data), "size": len(data)}


def reference(archive):
    with tarfile.open(archive) as source:
        top, = json.loads(exact_file(source, "index.json"))["manifests"]
        index = json.loads(blob(source, top))
        approved, = [d for d in index["manifests"] if d["digest"] == REFERENCE]
        image = json.loads(blob(source, approved))
        config = json.loads(blob(source, image["config"]))
        app, empty = image["layers"][-2:]
        if app["digest"] != APP_LAYER:
            raise ValueError("unexpected application layer")
        return config, [(d, blob(source, d)) for d in (app, empty)]


def build(original, base, output):
    config, application = reference(original)
    data = base.read_bytes()
    compressed = gzip.compress(data, mtime=0)
    base_layer = descriptor(compressed, "layer.v1.tar+gzip")
    config["rootfs"]["diff_ids"] = ["sha256:" + sha(data)] + config["rootfs"]["diff_ids"][-2:]
    config["history"] = [{"created_by": "Canonical Chisel Ubuntu 24.04 standard slices (disposable qualification)"}] + config["history"][-2:]
    config["config"]["Labels"]["org.eighred.calibration.test-chisel"] = DEFINITIONS
    config_bytes = encode(config)
    layers = [(base_layer, compressed), *application]
    manifest = encode({"schemaVersion": 2, "mediaType": PREFIX + "manifest.v1+json",
                       "config": descriptor(config_bytes, "config.v1+json"), "layers": [d for d, _ in layers]})
    selected = descriptor(manifest, "manifest.v1+json")
    selected["platform"] = {"architecture": "amd64", "os": "linux"}
    selected["annotations"] = {"org.opencontainers.image.ref.name": "kanz-chisel-test-1536-" + sha(manifest)[:12] + ":local"}
    index = encode({"schemaVersion": 2, "mediaType": PREFIX + "index.v1+json", "manifests": [selected]})

    with tarfile.open(output, "x") as archive:
        members = {"index.json": index, "oci-layout": encode({"imageLayoutVersion": "1.0.0"})}
        for content in [config_bytes, manifest, *(data for _, data in layers)]:
            members["blobs/sha256/" + sha(content)] = content
        for name, content in members.items():
            member = tarfile.TarInfo(name)
            member.size = len(content)
            archive.addfile(member, io.BytesIO(content))
    return selected["digest"]


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("original", "base", "output"):
        parser.add_argument(name, type=Path)
    args = parser.parse_args()
    print(build(args.original, args.base, args.output))

"""Materialize the reviewed worker-specific ProtoBuf.jl source from a local object store."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess


SOURCE = Path(__file__).resolve().parent
UPSTREAM = "https://github.com/JuliaIO/ProtoBuf.jl"
REVISION = "b3d83a7ce236ce44d8de6e688080069f176f4884"
BASE_TREE = "da18083a52d9d57bbe6dadaacad39731e5f7be39"
PATCHED_TREE = "72afd74d71ca3bc51e6678fca28cd6f9b3f1085a"
PATCH_DIGEST = "ab4747b4c9baff7eb6a7c434674cc47875e3a1bb8091ba48f1df659edc98c57d"


def git(directory, *args):
    return subprocess.run(
        ["git", "-c", "core.autocrlf=false", "-C", str(directory), *args],
        check=True, stdout=subprocess.PIPE, text=True,
    ).stdout.strip()


def materialize(source, target):
    patch = SOURCE / "protobuf.patch"
    digest = hashlib.sha256(patch.read_bytes()).hexdigest()
    if digest != PATCH_DIGEST:
        raise ValueError("ProtoBuf.jl patch digest differs")
    if git(source, "rev-parse", REVISION + "^{tree}") != BASE_TREE:
        raise ValueError("registered ProtoBuf.jl source tree differs")
    target.mkdir(parents=True, exist_ok=False)
    git(target, "init", "--quiet")
    git(target, "fetch", "--quiet", "--no-tags", "--update-shallow", str(source.resolve()), REVISION)
    git(target, "checkout", "--quiet", "--detach", REVISION)
    git(target, "apply", "--check", "--index", str(patch))
    git(target, "apply", "--index", str(patch))
    tree = git(target, "write-tree")
    if tree != PATCHED_TREE:
        raise ValueError("materialized ProtoBuf.jl source tree differs")
    return {
        "upstream": UPSTREAM, "revision": REVISION, "registered_tree": BASE_TREE,
        "patch_sha256": digest, "worker_source_tree": tree,
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path, help="existing upstream Git checkout")
    parser.add_argument("target", type=Path, help="new disposable package directory")
    args = parser.parse_args()
    print(json.dumps(materialize(args.source, args.target), sort_keys=True))

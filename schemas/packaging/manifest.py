"""Bind SDK archives to one source revision and Go's native module checksums."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
from zipfile import ZipFile


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def module_sums(bundle, module, tag):
    with tempfile.TemporaryDirectory() as directory:
        scratch = Path(directory)
        with ZipFile(bundle) as archive:
            archive.extractall(scratch / "proxy")
        env = dict(os.environ, GOWORK="off", GOFLAGS="", GOTOOLCHAIN="local",
                   GOMODCACHE=str(scratch / "modules"), GOPROXY=(scratch / "proxy").as_uri(),
                   GONOSUMDB=module, GONOPROXY="none")
        result = json.loads(subprocess.check_output(
            ["go", "mod", "download", "-json", f"{module}@{tag}"], cwd=scratch, env=env
        ))
        return {key: result[key] for key in ("Path", "Version", "Sum", "GoModSum")}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["write", "check", "assets"])
    parser.add_argument("version")
    parser.add_argument("source_sha")
    parser.add_argument("assets", type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r"[01](?:\.(?:0|[1-9][0-9]*)){2}", args.version):
        parser.error("expected a stable Go v0 or v1 compatible SDK version")
    if not re.fullmatch(r"[0-9a-f]{40}", args.source_sha):
        parser.error("expected a source commit SHA")
    if args.mode != "assets" and command("git", "rev-parse", "HEAD") != args.source_sha:
        parser.error("source SHA differs from the checkout")
    tag = "v" + args.version
    go_name = f"kanz-schemas-go-proxy-{tag}.zip"
    names = [go_name, f"kanz_schemas-{args.version}-py3-none-any.whl", f"eighred-kanz-schemas-{args.version}.tgz"]
    expected = {}
    for name in names:
        path = args.assets / name
        if path.is_symlink() or not path.is_file():
            parser.error(f"missing regular release asset {name}")
        expected[name] = sha256(path)
    manifest = args.assets / "sdk-manifest.json"
    if args.mode == "assets":
        recorded = json.loads(manifest.read_bytes())
        if (recorded.get("assets") != expected or recorded.get("source_repository") != "eighred/kanz"
                or recorded.get("source_sha") != args.source_sha or recorded.get("version") != args.version):
            parser.error("published SDK assets differ from the pinned manifest")
        return

    locks = [
        "schemas/buf.gen.yaml", "kanz/go.mod", "kanz/go.sum",
        "python/requirements-codegen.txt", "python/requirements.txt",
        "schemas/packaging/ts/public-consumer-lock.json",
    ]
    identity = {
        "source_repository": "eighred/kanz",
        "source_sha": args.source_sha,
        "version": args.version,
        "assets": expected,
        "inputs": {name: sha256(Path(name)) for name in locks},
    }
    if args.mode == "write":
        identity["go_module"] = module_sums(args.assets / go_name, "github.com/eighred/kanz/kanz-schemas-go", tag)
        identity["toolchains"] = {
            "go": command("go", "version"), "buf": command("buf", "--version"),
            "protoc-gen-go": command("protoc-gen-go", "--version"),
            "protoc-gen-go-grpc": command("protoc-gen-go-grpc", "--version"),
            "node": command("node", "--version"), "npm": command("npm", "--version"),
            "python": command("python3", "--version"),
        }
        with manifest.open("x", encoding="utf-8") as out:
            json.dump(identity, out, indent=2, sort_keys=True)
            out.write("\n")
    else:
        recorded = json.loads(manifest.read_bytes())
        for field, value in identity.items():
            if recorded.get(field) != value:
                parser.error(f"release manifest differs in {field}")


if __name__ == "__main__":
    main()

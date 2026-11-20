"""Assemble a private SDK from the existing code generator's output."""

import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
from zipfile import ZIP_DEFLATED, ZipFile, ZipInfo


GO_MODULE = "github.com/eighred/kanz/kanz-schemas-go"


def add_bytes(archive, name, data):
    # ZipInfo's fixed timestamp keeps proxy bytes independent of checkout time.
    archive.writestr(ZipInfo(name), data, compress_type=ZIP_DEFLATED)


def go_proxy(generated, version, output, epoch):
    module_file = (generated / "go.mod").read_bytes()
    if module_file.splitlines()[0] != f"module {GO_MODULE}".encode():
        raise ValueError("generated go.mod has the wrong module identity")
    sources = sorted(generated.rglob("*.pb.go"))
    if not sources:
        raise ValueError("generated Go SDK has no protobuf sources")
    sdk_version = "v" + version
    prefix = f"{GO_MODULE}@{sdk_version}/"
    module_zip = io.BytesIO()
    with ZipFile(module_zip, "w") as archive:
        add_bytes(archive, prefix + "go.mod", module_file)
        add_bytes(archive, prefix + "go.sum", (generated / "go.sum").read_bytes())
        for source in sources:
            add_bytes(archive, prefix + source.relative_to(generated).as_posix(), source.read_bytes())
    info = {"Version": sdk_version, "Time": datetime.fromtimestamp(epoch, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")}
    package = output / f"kanz-schemas-go-proxy-{sdk_version}.zip"
    with ZipFile(package, "x") as archive:
        path = f"{GO_MODULE}/@v/"
        add_bytes(archive, path + "list", (sdk_version + "\n").encode())
        add_bytes(archive, path + sdk_version + ".info", json.dumps(info).encode())
        add_bytes(archive, path + sdk_version + ".mod", module_file)
        add_bytes(archive, path + sdk_version + ".zip", module_zip.getvalue())
    return package


def python_wheel(generated, version, output):
    manifest = Path(__file__).parent / "python" / "pyproject.toml"
    with tempfile.TemporaryDirectory() as directory:
        stage = Path(directory)
        for source in sorted(generated.rglob("*")):
            if source.name.endswith(("_pb2.py", "_pb2.pyi", "_pb2_grpc.py")):
                target = stage / source.relative_to(generated)
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(source, target)
        if not any(stage.rglob("*_pb2_grpc.py")):
            raise ValueError("generated Python SDK has no gRPC stubs")
        metadata = manifest.read_text(encoding="utf-8")
        marker = 'version = "0.0.0"'
        if metadata.count(marker) != 1:
            raise ValueError("expected one package version placeholder")
        (stage / "pyproject.toml").write_text(metadata.replace(marker, f'version = "{version}"'), encoding="utf-8")
        subprocess.run(
            [sys.executable, "-m", "pip", "wheel", "--no-index", "--no-build-isolation",
             "--no-deps", "--wheel-dir", str(stage / "dist"), directory],
            check=True,
        )
        wheel, = (stage / "dist").glob("*.whl")
        package = output / wheel.name
        with wheel.open("rb") as src, package.open("xb") as dest:
            shutil.copyfileobj(src, dest)
    return package


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("language", choices=["go", "python"])
    parser.add_argument("--version", required=True)
    parser.add_argument("--generated", required=True, type=Path)
    parser.add_argument("--descriptors", type=Path, help="canonical FileDescriptorSet for the Go service inventory")
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r"(?:0|[1-9][0-9]*)(?:\.(?:0|[1-9][0-9]*)){2}", args.version):
        parser.error("a stable X.Y.Z SDK version is required")
    if args.language == "go" and int(args.version.split(".")[0]) > 1:
        parser.error("major versions above v1 require a versioned Go module path")
    schema_root = Path(__file__).resolve().parents[1] / "proto"
    protos = {path.relative_to(schema_root).with_suffix("").as_posix() for path in schema_root.rglob("*.proto")}
    suffixes = (".pb.go",) if args.language == "go" else ("_pb2.py", "_pb2.pyi", "_pb2_grpc.py")
    for suffix in suffixes:
        files = {path.relative_to(args.generated).as_posix() for path in args.generated.rglob("*" + suffix)}
        expected = {stem + suffix for stem in protos}
        if suffix == ".pb.go":
            files = {name for name in files if not name.endswith("_grpc.pb.go")}
        if not expected or expected != files:
            parser.error(f"generated {suffix} files differ from the canonical schema inventory")
    args.output.mkdir(parents=True, exist_ok=True)
    if args.language == "go":
        from google.protobuf.descriptor_pb2 import FileDescriptorSet

        if args.descriptors is None:
            parser.error("Go assembly needs the canonical service descriptors")
        descriptors = FileDescriptorSet.FromString(args.descriptors.read_bytes()).file
        if {Path(file.name).with_suffix("").as_posix() for file in descriptors} != protos:
            parser.error("descriptor file inventory differs from the canonical schemas")
        stubs = {file.name.removesuffix(".proto") + "_grpc.pb.go" for file in descriptors if file.service}
        actual = {path.relative_to(args.generated).as_posix() for path in args.generated.rglob("*_grpc.pb.go")}
        if not stubs or stubs != actual:
            parser.error("generated Go gRPC stubs differ from the canonical service descriptors")
        epoch = int(os.environ["SOURCE_DATE_EPOCH"])
        package = go_proxy(args.generated, args.version, args.output, epoch)
    else:
        package = python_wheel(args.generated, args.version, args.output)
    print(f"{hashlib.sha256(package.read_bytes()).hexdigest()}  {package.name}")


if __name__ == "__main__":
    main()

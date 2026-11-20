"""Materialize reviewed Apache sources for the Rust workspace, preserving upstream package identities."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import tarfile
import tempfile
import tomllib
import urllib.request

HERE = Path(__file__).resolve().parent
LIMIT = 64 * 1024 * 1024


def digest(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def tree(root):
    result = hashlib.sha256()
    files = []
    for item in root.rglob("*"):
        if item.is_symlink() or not (item.is_dir() or item.is_file()):
            raise ValueError(f"special file in Cargo source: {item}")
        if item.is_file():
            files.append((item.relative_to(root).as_posix(), item))
    for name, item in sorted(files):
        result.update(name.encode() + b"\0" + bytes.fromhex(digest(item)))
    return result.hexdigest()


def unpack(archive, folder):
    seen, roots, total = set(), set(), 0
    with tarfile.open(archive, "r:gz") as tar:
        for item in tar:
            parts = PurePosixPath(item.name).parts
            if not parts or any(p in ("/", ".", "..") or "\\" in p or ":" in p for p in parts):
                raise ValueError("unsafe Apache archive path")
            roots.add(parts[0])
            if len(roots) != 1 or not (item.isdir() or item.isfile()):
                raise ValueError("unexpected root or file type in Apache archive")
            if item.isdir():
                continue
            if len(parts) < 2 or parts in seen or len(seen) >= 10000:
                raise ValueError("ambiguous or oversized Apache archive")
            seen.add(parts)
            total += item.size
            if total > LIMIT:
                raise ValueError("Apache source exceeds size bound")
            destination = folder.joinpath(*parts[1:])
            destination.parent.mkdir(parents=True, exist_ok=True)
            with tar.extractfile(item) as src, destination.open("xb") as dst:
                shutil.copyfileobj(src, dst)
            destination.chmod(0o755 if item.mode & 0o111 else 0o644)


def materialize(row, work, cached):
    name, version = row["name"], row["version"]
    if not re.fullmatch(r"[a-z][a-z0-9_-]*", name) or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("unexpected Cargo coordinate")
    key = f"{name}-{version}"
    if row["manifestPath"] != f"rust/temporal_sources/generated/{key}/Cargo.toml":
        raise ValueError(f"wrong source path for {key}")
    folder = work / key
    folder.mkdir()
    patch = HERE / "patches" / f"{key}.patch"
    if digest(patch) != row["patchSHA256"]:
        raise ValueError(f"patch digest differs for {key}")
    upstream = row["upstream"]
    if cached is None:
        file = work / f"{key}.tar.gz"
        with urllib.request.urlopen(upstream["url"], timeout=60) as response, file.open("xb") as out:
            body = response.read(LIMIT + 1)
            if len(body) > LIMIT:
                raise ValueError("Apache source archive exceeds size bound")
            out.write(body)
    else:
        file = cached / f"{key}.tar.gz"
    if digest(file) != upstream["sha256"]:
        raise ValueError(f"upstream archive differs for {key}")
    unpack(file, folder)
    if cached is None:
        file.unlink()
    if tree(folder) != upstream["fileTreeSHA256"]:
        raise ValueError(f"upstream file tree differs for {key}")
    # The temporary directory is outside the checkout so git apply operates
    # on these extracted files, independent of a parent worktree's prefix.
    subprocess.run(["git", "-c", "core.autocrlf=false", "apply", "--no-index", "--whitespace=nowarn",
                    str(patch)], cwd=folder, check=True)
    package = tomllib.loads((folder / "Cargo.toml").read_text(encoding="utf-8"))["package"]
    if (package["name"], package["version"]) != (name, version) or tree(folder) != row["treeSHA256"]:
        raise ValueError(f"result source identity differs for {key}")


def verify(destination, rows):
    expected = {f"{r['name']}-{r['version']}": r["treeSHA256"] for r in rows}
    if destination.is_symlink() or len(expected) != len(rows) or {p.name for p in destination.iterdir()} != set(expected):
        raise ValueError("generated Cargo source set differs")
    for row in rows:
        delta = HERE / "patches" / f"{row['name']}-{row['version']}.patch"
        if delta.is_symlink() or digest(delta) != row["patchSHA256"]:
            raise ValueError(f"prepared source patch differs: {delta.name}")
    for name, checksum in expected.items():
        folder = destination / name
        if folder.is_symlink() or not folder.is_dir() or tree(folder) != checksum:
            raise ValueError(f"generated Cargo tree differs for {name}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archives", type=Path, help="offline directory of checksum-pinned original archives")
    parser.add_argument("--check", action="store_true", help="verify the prepared source set without fetching")
    args = parser.parse_args()
    recipe = json.loads((HERE / "manifest.json").read_bytes())
    if recipe["schema"] != "kanz.apache-temporal-sources.v1":
        raise ValueError("unknown Apache source recipe")
    rows = recipe["packages"]
    destination = HERE / "generated"
    if destination.exists() or args.check:
        verify(destination, rows)
    else:
        with tempfile.TemporaryDirectory(prefix="kanz-apache-temporal-") as unpacked:
            work = Path(unpacked)
            for row in rows:
                materialize(row, work, args.archives)
            verify(work, rows)
            # Stage on the destination filesystem, then publish all inputs in one rename.
            with tempfile.TemporaryDirectory(dir=HERE, prefix=".prepared-") as temp:
                stage = Path(temp) / "generated"
                shutil.copytree(work, stage)
                try:
                    stage.rename(destination)
                except OSError:
                    if not destination.is_dir():
                        raise
                    verify(destination, rows)
    print(f"Verified {len(rows)} Apache source packages for the Rust workspace.")


if __name__ == "__main__":
    main()

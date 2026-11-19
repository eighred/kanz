"""Reproduce the isolated Jiff migration qualification from pinned public inputs."""
import argparse
import hashlib
import pathlib
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
IMAGE = "ghcr.io/eighred/base/rust@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f"
GO_TZDATA = "https://raw.githubusercontent.com/golang/go/e87b10ea2a2c6c65b80c4374af42b9c02ac9fb20/lib/time/zoneinfo.zip"
CRATES = {
    "jiff": ("1.0.1", 705302, "ed5d126d72eb017f10b302e73bdeca446bea405ee449af4d9e7866d6b082d83e"),
    "jiff-core": ("1.0.1", 131557, "b6b46a105a9874634dd96231021526bf24ca04955a5998c59a9c7666e634b7b8"),
    "jiff-tzdb": ("1.0.0", 62763, "12e71a1b68a875a44a33bb7a1dd84392cb77145a3367a552a3cfe6a4f5cf3afc"),
}


def checked_input(directory, name, url, size, expected):
    source = (directory / name).open("rb") if directory else urllib.request.urlopen(url, timeout=30)
    with source:
        data = source.read(size + 1)
    if len(data) != size or hashlib.sha256(data).hexdigest() != expected:
        raise ValueError(f"input size or SHA-256 differs for {name}")
    return data


def prepare_crate(root, name, version, file):
    # The compiler needs src and its doc inclusions. The database crate uses
    # top-level .rs and .dat files. No downloaded build scripts are executed.
    with tarfile.open(file, "r:gz") as archive:
        for entry in archive:
            path = pathlib.PurePosixPath(entry.name)
            if not path.parts or path.parts[0] != f"{name}-{version}" or ".." in path.parts:
                raise ValueError(f"invalid {name} package path")
            relative = pathlib.PurePosixPath(*path.parts[1:])
            if not relative.parts:
                continue
            if relative.parts[0] != "src" and relative.suffix not in {".rs", ".dat", ".md"}:
                continue
            if not entry.isfile() or entry.size > 4 << 20:
                raise ValueError(f"unexpected {name} source member")
            target = root / "src" / name / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            with archive.extractfile(entry) as contents:
                target.write_bytes(contents.read())


def qualify(inputs):
    with tempfile.TemporaryDirectory(prefix="kanz-jiff-") as scratch:
        root = pathlib.Path(scratch)
        for name in ("build.sh", "closed.c", "compare.rs", "duration_contract.rs", "goldens.py"):
            shutil.copyfile(HERE / name, root / name)
        for crate, (version, size, expected) in CRATES.items():
            filename = f"{crate}.crate"
            url = f"https://static.crates.io/crates/{crate}/{crate}-{version}.crate"
            archive = root / filename
            archive.write_bytes(checked_input(inputs, filename, url, size, expected))
            prepare_crate(root, crate, version, archive)
        archive = checked_input(
            inputs, "zoneinfo.zip", GO_TZDATA, 408125,
            "8f55634d05f8bca1f7bc7c69c5933428c69357e0bdf565e5ba224e3f88ff12e8",
        )
        (root / "zoneinfo.zip").write_bytes(archive)
        for version in ("2025c", "2026f"):
            subprocess.run([sys.executable, "-B", "goldens.py", version], cwd=root, check=True)
        # The Linux bind mount is read by UID 65534. Only authenticated public
        # inputs and this component's code reside in the disposable directory.
        for path in root.rglob("*"):
            path.chmod(0o755 if path.is_dir() else 0o644)
        root.chmod(0o755)
        subprocess.run([
            "docker", "run", "--rm", "--pull", "never", "--network", "none", "--read-only",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
            "--platform", "linux/amd64", "--memory", "1024m", "--cpus", "2", "--pids-limit", "128",
            "--user", "65534:65534", "--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=400m",
            "--mount", f"type=bind,src={root},dst=/src,readonly", "-w", "/src", IMAGE,
            "sh", "build.sh",
        ], check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inputs", type=pathlib.Path, help="read the four named archives locally instead of downloading")
    qualify(parser.parse_args().inputs)

"""Explicit install smoke test; importing this module never launches the CLI."""
from contextlib import contextmanager
import time
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
from skill_discovery import discover_skills

ROOT = Path(__file__).resolve().parents[4]
PLUGIN_PATH = Path(".agents/plugins/kanz-forge")


@contextmanager
def disposable_profile():
    temp = tempfile.TemporaryDirectory(prefix="forge-install-test-")
    directory = Path(temp.name)
    try:
        yield temp.name
    finally:
        try:
            temp.cleanup()
        except PermissionError as exc:
            # The Windows CLI may briefly hold the already-empty directory.
            # Retry only its final rmdir; never hide a leaked file or active lock.
            if os.name != "nt" or exc.winerror != 32 or any(directory.iterdir()):
                raise
            for _ in range(20):
                time.sleep(0.25)
                try:
                    directory.rmdir()
                    break
                except PermissionError:
                    continue
            else:
                raise


def snapshot_checkout(root, destination):
    """Test current bytes, including new files and staged/unstaged deletions.

    A clean disposable source prevents ignored build/cache files from entering
    the package. The live index, marketplace and installed cache are untouched.
    """
    raw = subprocess.check_output(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--",
         PLUGIN_PATH.as_posix(), ".agents/plugins/marketplace.json"], cwd=root, timeout=30)
    for name in sorted(set(filter(None, raw.split(b"\0")))):
        relative = Path(os.fsdecode(name))
        path = root / relative
        if path.is_symlink() or not path.resolve().is_relative_to(root.resolve()):
            raise ValueError(f"non-local package input: {relative}")
        if not path.exists():
            continue
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(path, target)
    return destination / PLUGIN_PATH


def package_bytes(directory):
    result = {}
    for path in directory.rglob("*"):
        if path.is_symlink() or not path.resolve().is_relative_to(directory.resolve()):
            raise ValueError("non-local installed package file")
        if path.is_file():
            result[path.relative_to(directory).as_posix()] = path.read_bytes().replace(b"\r\n", b"\n")
    return result


def verify_installed(source, installed):
    expected, actual = package_bytes(source), package_bytes(installed)
    missing, extra = expected.keys() - actual.keys(), actual.keys() - expected.keys()
    changed = {name for name in expected.keys() & actual.keys() if expected[name] != actual[name]}
    if missing or extra or changed:
        raise ValueError(f"installed package differs: missing={sorted(missing)}, "
                         f"extra={sorted(extra)}, changed={sorted(changed)}")
    return len(expected)


def smoke_test(root=ROOT):
    cli = shutil.which("codex.exe") or shutil.which("codex")
    if not cli:
        raise ValueError("Codex CLI unavailable; install smoke test was not run")
    with disposable_profile() as workspace:
        directory = Path(workspace)
        profile = directory / "profile"
        profile.mkdir()
        checkout = directory / "checkout"
        source = snapshot_checkout(root, checkout)
        env = {**os.environ, "CODEX_HOME": str(profile)}

        def call(*args):
            return json.loads(subprocess.check_output(
                [cli, *args, "--json"], cwd=checkout, env=env, timeout=90))

        call("plugin", "marketplace", "add", ".")
        result = call("plugin", "add", "kanz-forge@kanz-local")
        installed = Path(result["installedPath"]).resolve()
        if not installed.is_relative_to(profile.resolve()):
            raise ValueError("install escaped disposable profile")
        version = json.loads((source / ".codex-plugin/plugin.json").read_text(encoding="utf-8"))["version"]
        if result["version"] != version:
            raise ValueError("installed version differs from snapshot")
        count = verify_installed(source, installed)
        print(f"Verified {count} installed files from working-tree snapshot: {version}")
        entries = discover_skills(cli, checkout, env=env)
        expected = {"kanz-forge:" + p.parent.name for p in (source / "skills").glob("*/SKILL.md")}
        found = {s["name"] for s in entries if s["enabled"]
                 and Path(s["path"]).resolve().is_relative_to(installed)}
        if found != expected:
            raise ValueError(f"discovered Forge skills differ: {found} != {expected}")
        print(f"Fresh Codex discovery: {len(found)} enabled Forge skills")


if __name__ == "__main__":
    try:
        smoke_test()
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        raise SystemExit(f"install verification: {exc}")

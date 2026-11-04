"""Smoke-test a local install in a new empty CLI profile; never edit the live profile."""
from contextlib import contextmanager
import time
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
from skill_discovery import discover_skills

root = Path(__file__).resolve().parents[4]
source = root / ".agents/plugins/kanz-forge"
cli = shutil.which("codex.exe") or shutil.which("codex")
if not cli:
    raise SystemExit("Codex CLI unavailable; install smoke test was not run")
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
with disposable_profile() as profile:
    def call(*args):
        return json.loads(subprocess.check_output([cli,*args,"--json"],cwd=root,
                          env={**os.environ,"CODEX_HOME":profile},timeout=90))
    call("plugin","marketplace","add",".")
    result = call("plugin","add","kanz-forge@kanz-local")
    installed = Path(result["installedPath"]).resolve()
    assert installed.is_relative_to(Path(profile).resolve())
    assert result["version"] == json.loads((source/".codex-plugin/plugin.json").read_text())["version"]
    tracked = subprocess.check_output(["git","ls-files","-z","--",".agents/plugins/kanz-forge"],cwd=root).decode().split("\0")
    for path in filter(None,tracked):
        src = root/path
        dst = installed/src.relative_to(source)
        assert src.read_bytes().replace(b"\r\n",b"\n") == dst.read_bytes().replace(b"\r\n",b"\n"), path
    print(f"Verified {len(tracked)-1} installed files in disposable profile: {result['version']}")

    entries=discover_skills(cli,root,env={**os.environ,"CODEX_HOME":profile})
    expected={"kanz-forge:"+p.parent.name for p in (source/"skills").glob("*/SKILL.md")}
    found={s["name"] for s in entries if s["enabled"]
           and Path(s["path"]).resolve().is_relative_to(installed)}
    assert found==expected,(found,expected)
    print(f"Fresh Codex discovery: {len(found)} enabled Forge skills")

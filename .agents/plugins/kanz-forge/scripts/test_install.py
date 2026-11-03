"""Smoke-test a local install in a new empty CLI profile; never edit the live profile."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

root = Path(__file__).resolve().parents[4]
source = root / ".agents/plugins/kanz-forge"
cli = shutil.which("codex.exe") or shutil.which("codex")
if not cli:
    raise SystemExit("Codex CLI unavailable; install smoke test was not run")
with tempfile.TemporaryDirectory(prefix="forge-install-test-") as profile:
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

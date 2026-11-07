"""Read-only canonical exact-main workflow acceptance before any SSM command."""
import argparse
import types
import json
import os
from pathlib import Path
import re
import urllib.parse
from bounded_process import run

ROOT = Path(__file__).resolve().parents[2]
# Compile the tracked source directly: an ignored .pyc cannot authorize release.
release_source = ROOT / ".github/scripts/release_ci.py"
release_ci = types.ModuleType("release_ci")
release_ci.__file__ = str(release_source)
exec(compile(release_source.read_bytes(), str(release_source), "exec"), release_ci.__dict__)



class API:
    def get(self, suffix, **params):
        endpoint = "repos/eighred/kanz" + suffix
        if params:
            endpoint += "?" + urllib.parse.urlencode(params)
        environment = {key: os.environ[key] for key in (
            "PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "APPDATA", "LOCALAPPDATA", "TMP", "TEMP",
            "GH_TOKEN", "GITHUB_TOKEN", "GH_CONFIG_DIR") if key in os.environ}
        result = run(["gh", "api", "--hostname", "github.com", "--method", "GET", endpoint], cwd=ROOT,
                     env=environment, timeout=60, stdout_limit=8 << 20)
        return json.loads(result)


def verify(source):
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("invalid exact-main source")
    api = API()
    if api.get("/commits/main").get("sha") != source:
        raise ValueError("canonical main advanced")
    result = release_ci.verify_ci(api, source, str(ROOT))
    if api.get("/commits/main").get("sha") != source:
        raise ValueError("canonical main advanced during acceptance")
    print(json.dumps({"source": source, "canonicalWorkflows": len(result), "accepted": True}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", required=True)
    verify(parser.parse_args().source)

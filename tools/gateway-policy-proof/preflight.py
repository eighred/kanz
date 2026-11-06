"""Read-only canonical exact-main workflow acceptance before any SSM command."""
import argparse
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import urllib.parse

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("release_ci", ROOT / ".github/scripts/release_ci.py")
release_ci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release_ci)


class API:
    def get(self, suffix, **params):
        endpoint = "repos/eighred/kanz" + suffix
        if params:
            endpoint += "?" + urllib.parse.urlencode(params)
        result = subprocess.run(["gh", "api", "--hostname", "github.com", "--method", "GET", endpoint], cwd=ROOT,
                                capture_output=True, text=True, timeout=60)
        if result.returncode or len(result.stdout.encode()) > 8 << 20:
            raise ValueError("canonical GitHub acceptance lookup failed")
        return json.loads(result.stdout)


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

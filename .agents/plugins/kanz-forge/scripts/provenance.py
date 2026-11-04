"""Verify the pinned local security-audit imports; never fetch or adopt code."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import sys

PLUGIN = Path(__file__).resolve().parents[1]
OID = re.compile(r"[0-9a-f]{40}")
REPO = re.compile(r"[\w.-]+/[\w.-]+", re.ASCII)


def registry():
    rows = json.loads((PLUGIN / "upstreams.json").read_text(encoding="utf-8"))
    if not isinstance(rows, list) or not rows:
        raise ValueError("empty or invalid upstream registry")
    result = {}
    for row in rows:
        repo = row["repo"]
        if (not REPO.fullmatch(repo) or ".." in repo or repo in result
                or not OID.fullmatch(row["pin"]) or row["url"] != "https://github.com/" + repo):
            raise ValueError("invalid registry identity")
        if not row["imports"]:
            raise ValueError("registered import set is empty")
        for item in row["imports"]:
            path = (PLUGIN / item["local"]).resolve()
            if not path.is_relative_to(PLUGIN / "skills"):
                raise ValueError("import outside skills")
            data = path.read_bytes().replace(b"\r\n", b"\n")
            if hashlib.sha256(data).hexdigest() != item["sha256_lf"]:
                raise ValueError("local import drift: " + item["local"])
            if not item["adapted"]:
                blob = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
                if blob != item["blob"]:
                    raise ValueError("upstream blob mismatch: " + item["source"])
        result[repo] = row
    return result



def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()
    try:
        rows = registry()
        print(json.dumps({"verified": [{"repo": name, "pin": row["pin"],
                          "imports": len(row["imports"])} for name, row in rows.items()]}))
        return 0
    except (OSError, ValueError, KeyError, TypeError) as exc:
        print(f"provenance: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

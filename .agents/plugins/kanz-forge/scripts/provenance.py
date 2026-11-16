"""Verify the pinned local security-audit imports; never fetch or adopt code."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import sys

PLUGIN = Path(__file__).resolve().parents[1]
OID = re.compile(r"[0-9a-f]{40}")
REPO = re.compile(r"[\w.-]+/[\w.-]+", re.ASCII)


def relative_path(value):
    if (not isinstance(value, str) or not value or "\\" in value
            or PurePosixPath(value).is_absolute() or ".." in PurePosixPath(value).parts
            or str(PurePosixPath(value)) != value):
        raise ValueError("invalid import path")
    return value


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
        sources, destinations = {}, set()
        for item in row["imports"]:
            path = (PLUGIN / item["local"]).resolve()
            if not path.is_relative_to(PLUGIN / "skills"):
                raise ValueError("import outside skills")
            source = relative_path(item["source"])
            local = relative_path(item["local"])
            if source in sources or local in destinations:
                raise ValueError("duplicate imported source or destination")
            if (not isinstance(item["adapted"], bool) or not OID.fullmatch(item["blob"])
                    or not re.fullmatch(r"[0-9a-f]{64}", item["sha256_lf"])):
                raise ValueError("invalid import metadata")
            data = path.read_bytes().replace(b"\r\n", b"\n")
            if hashlib.sha256(data).hexdigest() != item["sha256_lf"]:
                raise ValueError("local import drift: " + item["local"])
            if not item["adapted"]:
                blob = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
                if blob != item["blob"]:
                    raise ValueError("upstream blob mismatch: " + item["source"])
            sources[source] = item
            destinations.add(local)
        licenses = row.get("license_sources")
        if not isinstance(licenses, list) or not licenses:
            raise ValueError("missing license provenance")
        for license in licenses:
            item = sources.get(relative_path(license["path"]))
            if item is None or item["adapted"] or item["blob"] != license["blob"]:
                raise ValueError("license must be retained as an unmodified verified import")
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

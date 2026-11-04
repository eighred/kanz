"""Optional pricing test receipts; historical source hints, never acceptance or authority.

The existing ~/.kanz-forge/receipts-v1.sqlite schema and worktree identity are
preserved. Scope excludes toolchain, external inputs and transitive dependencies;
unchanged source is not proof of unchanged behavior. Chat does not require this.
"""
import argparse
from contextlib import closing
import datetime
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys

SCOPES = ("kanz/internal/risk/pricing", "kanz/internal/risk/benchmarks",
          "kanz/internal/risk/termsource", "kanz/go.mod", "kanz/go.sum")


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], timeout=45)


def identity(root):
    common = Path(git(root, "rev-parse", "--path-format=absolute", "--git-common-dir").decode().strip())
    return hashlib.sha256(os.fsencode(common.resolve())).hexdigest()


def fingerprint(root):
    paths = git(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", *SCOPES)
    h = hashlib.sha256()
    for raw in sorted(set(filter(None, paths.split(b"\0")))):
        path = root / os.fsdecode(raw)
        if path.is_symlink() or not path.resolve().is_relative_to(root.resolve()):
            raise ValueError("refusing non-local evidence input")
        h.update(raw + b"\0")
        h.update(hashlib.sha256(path.read_bytes()).digest() if path.is_file() else b"deleted")
    return h.hexdigest()


def database(root):
    db = (Path.home()/".kanz-forge"/"receipts-v1.sqlite").resolve()
    if db.is_relative_to(root.resolve()):
        raise ValueError("cache cannot be inside repository")
    return db


def remember(root, collector):
    before = fingerprint(root)
    revision = git(root,"rev-parse","HEAD").decode().strip()
    result = collector(root)
    expected = {"github.com/eighred/kanz/internal/risk/pricing","github.com/eighred/kanz/internal/risk/benchmarks"}
    if (set(result["packages_passed"]) != expected or result["failures"] or result["skips"]
            or fingerprint(root) != before or git(root,"rev-parse","HEAD").decode().strip()!=revision):
        raise ValueError("incomplete verification or inputs changed; receipt not stored")
    db = database(root)
    db.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    with closing(sqlite3.connect(db, timeout=5)) as con, con:
        con.execute("CREATE TABLE IF NOT EXISTS receipts (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, sha TEXT NOT NULL, digest TEXT NOT NULL, observed TEXT NOT NULL)")
        con.execute("CREATE INDEX IF NOT EXISTS receipt_repo ON receipts(repo,id)")
        key=con.execute("INSERT INTO receipts(repo,sha,digest,observed) VALUES(?,?,?,?)",
                        (identity(root),revision,before,datetime.datetime.now(datetime.timezone.utc).isoformat())).lastrowid
    return {"receipt":key,"revision":revision,"observed":result,"scope":SCOPES}


def recall(root):
    db=database(root)
    if not db.exists(): return {"receipts":[],"status":"empty"}
    current=fingerprint(root)
    with closing(sqlite3.connect(db.as_uri()+"?mode=ro",uri=True,timeout=5)) as con:
        rows=con.execute("SELECT id,sha,digest,observed FROM receipts WHERE repo=? ORDER BY id DESC LIMIT 10",(identity(root),)).fetchall()
    return {"receipts":[{"id":i,"sha":sha,"observed":at,"status":"source_unchanged_not_retested" if digest==current else "stale"} for i,sha,digest,at in rows],
            "scope":SCOPES,"authorization":False}



def pricing(root):
    groups = ["./internal/risk/pricing", "./internal/risk/benchmarks"]
    expected = {"github.com/eighred/kanz/" + group[2:] for group in groups}
    proc = subprocess.run(["go", "test", "-json", "-mod=readonly", "-p", "1", "-count=1", *groups],
                          cwd=root / "kanz", stdout=subprocess.PIPE, text=True, timeout=900)
    packages, failed, skipped = [], [], []
    for line in proc.stdout.splitlines():
        event = json.loads(line)
        label = event.get("Package", "") + ":" + event.get("Test", "")
        if event["Action"] == "skip":
            skipped.append(label)
        if event["Action"] == "fail":
            failed.append(label)
        if event["Action"] == "pass" and not event.get("Test"):
            packages.append(event["Package"])
    summary = {"packages_passed": packages, "failures": failed, "skips": skipped}
    if proc.returncode or failed or set(packages) != expected or len(packages) != len(expected):
        raise ValueError("incomplete or failed pricing suite: " + json.dumps(summary))
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("read", "record"),
                        help="read existing history, or run pricing/benchmark tests and record only complete unskipped results")
    args = parser.parse_args()
    try:
        root = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True, timeout=45).strip())
        if not (root / "kanz/go.mod").is_file():
            raise ValueError("run from a Kanz checkout")
        result = recall(root) if args.action == "read" else remember(root, pricing)
        print(json.dumps(result))
        return 0
    except (OSError, ValueError, sqlite3.Error, subprocess.SubprocessError) as exc:
        print(f"receipts: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

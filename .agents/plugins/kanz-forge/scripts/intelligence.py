"""Scoped evidence receipts and advisory planning; never an execution authority."""
from contextlib import closing
import datetime
import hashlib
import json
import os
from pathlib import Path, PureWindowsPath
import re
import sqlite3
import subprocess

VERSION = 1
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
    result = collector("/financial","pricing")
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


def route(paths):
    actions={}
    for name in paths:
        path=PureWindowsPath(name)
        if path.anchor or ".." in path.parts or not path.parts: raise ValueError("expected relative repository path")
        parts=path.as_posix().lower().split("/")
        def add(cmd): actions.setdefault(cmd,[]).append(name)
        if any(x in parts for x in ("pricing","risk","portfolio","tca","backtest","performance")): add("/financial")
        if any(x in parts for x in ("oms","identity","auth","execution","compliance","infra","security-audit")): add("/security")
        if parts[-1] in ("go.mod","go.sum","package-lock.json","requirements.txt","upstreams.json"): add("/deps")
        if parts[-1].endswith((".proto",".sql")): add("/audit")
    return {"suggestions":actions,"execution":"none","next":"choose applicable verification"}


def prioritize(issues):
    candidates=[]; blocked=[]
    for issue in issues:
        labels={x["name"] for x in issue["labels"]}
        levels=labels & {"P0","P1","P2"}
        reasons=sorted(labels & {"blocked-external","decision-needed"})
        if len(levels)!=1 or "open-work" not in labels: reasons.append("readiness not established")
        if issue["number"] in {1316,1125,1360}: reasons.append("protected capital boundary")
        item={"number":issue["number"],"title":issue["title"],"labels":sorted(labels)}
        if reasons: blocked.append(dict(item,reasons=reasons));continue
        candidates.append((next(iter(levels)),not bool(labels & {"execution","security","compliance"}),issue["number"],item))
    return {"shortlist":[t[-1] for t in sorted(candidates)[:5]],"blocked":blocked,
            "decision":"Investigate code, dependencies, duplicates and existing approvals before selection. No task dispatched."}


def run(action,target,root,collector):
    if action in {"/remember","/memory","/recall"}:
        if target not in ("","pricing"): raise ValueError("supported receipt scope: pricing")
        return remember(root,collector) if action=="/remember" else recall(root)
    if action=="/route":
        if target: return route([target])
        paths=git(root,"diff","--name-only","-z","HEAD")+git(root,"ls-files","-z","--others","--exclude-standard")
        return route([os.fsdecode(p) for p in paths.split(b"\0") if p])
    if target: raise ValueError("unexpected target")
    if action=="/prioritize":
        rows=json.loads(subprocess.check_output(["gh","issue","list","--repo","eighred/kanz","--state","open","--limit","1000","--json","number,title,labels"],timeout=60))
        if len(rows)>=1000: raise ValueError("issue snapshot possibly truncated")
        return prioritize(rows)
    if action!="/doctor": raise ValueError("unknown planning operation")
    config=root/".agents/plugins/kanz-forge/.codex-plugin/plugin.json"
    installed=json.loads(subprocess.check_output(["codex","plugin","list","--marketplace","kanz-local","--json"],timeout=45))
    return {"checkout":str(root),"sha":git(root,"rev-parse","HEAD").decode().strip(),
            "dirty":bool(git(root,"status","--porcelain")),"checkout_version":json.loads(config.read_text())["version"],
            "installed":[{"version":p["version"],"enabled":p["enabled"],"source":p["source"]["path"]} for p in installed["installed"] if p["name"]=="kanz-forge"],
            "memory_exists":database(root).is_file(),"scope":"observed state; no repair performed"}

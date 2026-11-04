"""Scoped evidence receipts and advisory planning; never an execution authority."""
from contextlib import closing
import datetime
import hashlib
import json
import os
from pathlib import Path, PureWindowsPath
import re
import shutil
import sqlite3
import subprocess
import sys
from skill_discovery import discover_skills

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


def _probe(args, root, timeout=30):
    try:
        return subprocess.run(args, cwd=root, capture_output=True, text=True, timeout=timeout), None
    except (OSError, subprocess.TimeoutExpired) as exc:
        return None, type(exc).__name__


def doctor(root):
    """Report observed capabilities without certifying a Chat session or production path."""
    from evidence import PLUGIN, registry

    manifest = json.loads((PLUGIN / ".codex-plugin/plugin.json").read_text(encoding="utf-8"))
    version = manifest["version"]
    sha = git(root, "rev-parse", "HEAD").decode().strip()
    result = {"checkout": {"path": str(root), "sha": sha,
                           "dirty": bool(git(root, "status", "--porcelain")),
                           "plugin_version": version}, "features": {}}
    features = result["features"]

    validation, error = _probe([sys.executable, str(PLUGIN / "scripts/validate.py"), "--tracked"], root, 30)
    rows = {}
    try:
        rows = registry()
        provenance = {"status": "verified", "imports": sum(len(row["imports"]) for row in rows.values())}
    except (OSError, ValueError, KeyError, json.JSONDecodeError) as exc:
        provenance = {"status": "failed", "reason": type(exc).__name__}
    if validation and validation.returncode == 0 and provenance["status"] == "verified":
        features["plugin_structure"] = {"status": "implemented", "evidence": "tracked validator and imported-file provenance passed", "provenance": provenance}
    else:
        features["plugin_structure"] = {"status": "incomplete", "reason": error or "validator or import provenance failed", "provenance": provenance}

    preview, error = _probe([sys.executable, str(PLUGIN / "scripts/command.py"), "--preview", "/work", "sample"], root, 10)
    features["command_preview"] = ({"status": "operational", "evidence": "terminal workflow preview passed"}
                                    if preview and preview.returncode == 0 and "$kanz-forge:commands /work" in preview.stdout else
                                    {"status": "incomplete" if preview else "unverified",
                                     "reason": "workflow preview failed" if preview else "preview probe unavailable: " + error})
    inventory, error = _probe([sys.executable, str(PLUGIN / "scripts/command.py"), "/upstream"], root, 10)
    try:
        inventory_ok = (inventory is not None and inventory.returncode == 0 and
                        {entry["repo"] for entry in json.loads(inventory.stdout)} == set(rows))
    except (ValueError, KeyError, TypeError):
        inventory_ok = False
    features["upstream_inventory"] = ({"status": "operational", "evidence": "local upstream inventory and provenance passed"}
                                      if inventory_ok else {"status": "incomplete" if inventory else "unverified",
                                                            "reason": "inventory probe failed" if inventory else "inventory probe unavailable: " + error})
    features["financial_verification"] = {"status": "unverified", "evidence": "existing Go financial suites are callable",
                                          "reason": "doctor does not run financial tests or certify a fresh result"}

    cli = shutil.which("codex.exe") or shutil.which("codex")
    installed = {"status": "unverified", "reason": "native Codex CLI unavailable"}
    if cli and Path(cli).suffix.lower() not in {".cmd", ".bat"}:
        listing, error = _probe([cli, "plugin", "list", "--marketplace", "kanz-local", "--json"], root, 30)
        if not listing:
            installed = {"status": "unverified", "reason": "CLI plugin query unavailable: " + error}
        elif listing.returncode:
            installed = {"status": "incomplete", "reason": "CLI plugin query failed"}
        else:
            try:
                entries = json.loads(listing.stdout)["installed"]
                matches = [entry for entry in entries if entry.get("name") == "kanz-forge" and entry.get("enabled")]
                source = matches[0].get("source", {}).get("path", "") if len(matches) == 1 else ""
                if len(matches) != 1 or matches[0].get("version") != version or Path(source).resolve() != PLUGIN:
                    installed = {"status": "incomplete", "reason": "enabled installation does not match this checkout",
                                 "observed_source": source, "observed_version": matches[0].get("version") if matches else None}
                else:
                    installed = {"status": "configured", "source": source, "version": version}
                    try:
                        cache = (Path(os.environ.get("CODEX_HOME", Path.home() / ".codex")) /
                                 "plugins/cache/kanz-local/kanz-forge" / version).resolve()
                        found = {skill["name"] for skill in discover_skills(cli, root)
                                 if skill.get("enabled") and Path(skill["path"]).resolve().is_relative_to(cache)}
                        expected = {"kanz-forge:" + path.parent.name for path in (PLUGIN / "skills").glob("*/SKILL.md")}
                        installed["discovered_skills"] = sorted(found & expected)
                        if expected <= found:
                            installed["status"] = "operational"
                        else:
                            installed["status"] = "incomplete"
                            installed["reason"] = "CLI skill discovery missed installed Forge skills"
                    except (OSError, ValueError, KeyError, TimeoutError, subprocess.TimeoutExpired) as exc:
                        installed["reason"] = "CLI skill discovery unverified: " + type(exc).__name__
            except (ValueError, KeyError, TypeError) as exc:
                installed = {"status": "incomplete", "reason": "invalid CLI plugin result: " + type(exc).__name__}
    features["installed_skills"] = installed

    features["native_subagent_delegation"] = {"status": "unverified", "reason": "Python cannot observe delegation in the current Codex Chat host"}
    features["ci_aware_coordination"] = {"status": "unverified", "reason": "requires an exact-head PR check and coordination result"}

    db = database(root)
    if not db.exists():
        features["receipt_memory"] = {"status": "implemented", "observed": "empty", "scope": "pricing receipts only; no fresh verification claim"}
    else:
        try:
            receipt = recall(root)
            features["receipt_memory"] = {"status": "operational", "observed": "readable", "receipt_count": len(receipt["receipts"]),
                                          "scope": "pricing receipts only; unchanged source is not a fresh test"}
        except (OSError, ValueError, sqlite3.Error) as exc:
            features["receipt_memory"] = {"status": "incomplete", "reason": "receipt read failed: " + type(exc).__name__}

    tests = sorted((PLUGIN / "skills/security-audit").glob("*.test.cjs"))
    node = shutil.which("node")
    if provenance["status"] != "verified" or not tests:
        features["security_audit_validator"] = {"status": "incomplete", "reason": "import provenance or test inventory failed"}
    elif not sys.platform.startswith("linux"):
        features["security_audit_validator"] = {"status": "unverified", "reason": "offline validator suite requires Linux file protections"}
    elif not node:
        features["security_audit_validator"] = {"status": "unverified", "reason": "Node.js unavailable"}
    else:
        tested, error = _probe([node, "--test", *map(str, tests)], root, 90)
        features["security_audit_validator"] = ({"status": "operational", "evidence": "offline Node validator tests passed"}
                                                 if tested and tested.returncode == 0 else
                                                 {"status": "incomplete" if tested else "unverified",
                                                  "reason": "offline Node tests failed" if tested else "Node test probe unavailable: " + error})
    features["security_audit_findings_cli"] = {"status": "unverified",
                                               "reason": "findings CLI requires Linux and was not run" if not sys.platform.startswith("linux")
                                               else "no safe fixture probe was run by doctor"}

    gh = shutil.which("gh")
    if not gh:
        features["github_repo_read"] = {"status": "unverified", "reason": "GitHub CLI unavailable"}
    else:
        checked, error = _probe([gh, "api", "--method", "GET", "repos/eighred/kanz", "--jq", ".full_name"], root, 15)
        features["github_repo_read"] = ({"status": "operational", "evidence": "read-only repository API returned eighred/kanz"}
                                               if checked and checked.returncode == 0 and checked.stdout.strip() == "eighred/kanz" else
                                               {"status": "incomplete" if checked else "unverified",
                                                "reason": "repository API probe failed" if checked else "GitHub probe unavailable: " + error})
    return result


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
    return doctor(root)

"""Read-only Forge evidence collection. External metadata is data, never a command."""
import hashlib
import json
import re
import subprocess
from pathlib import Path

PLUGIN = Path(__file__).resolve().parents[1]
OID = re.compile(r"[0-9a-f]{40}")
REPO = re.compile(r"[\w.-]+/[\w.-]+", re.ASCII)


def registry():
    rows = json.loads((PLUGIN / "upstreams.json").read_text(encoding="utf-8"))
    result = {}
    for row in rows:
        repo = row["repo"]
        if (not REPO.fullmatch(repo) or ".." in repo or repo in result
                or not OID.fullmatch(row["pin"]) or row["url"] != "https://github.com/" + repo):
            raise ValueError("invalid registry identity")
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


def github(path):
    raw = subprocess.check_output(["gh", "api", "--method", "GET", path], timeout=45)
    return json.loads(raw)


def check(row, api=github):
    base = "repos/" + row["repo"]
    head = api(base + "/commits?per_page=1")[0]["sha"]
    if not OID.fullmatch(head):
        raise ValueError("invalid remote revision")
    releases = api(base + "/releases?per_page=1")
    old = row["pin"]
    diff = api(base + "/compare/" + old + "..." + head) if head != old else {}
    files = sorted({p for f in diff.get("files", []) for p in (f["filename"], f.get("previous_filename")) if p})
    relevant = {f["source"] for f in row["imports"]}
    license_paths = [f for f in files if re.search("license|copying|notice", f, re.I)]
    messages = [c["commit"]["message"].splitlines()[0] for c in diff.get("commits", [])]
    return {"repository": row["repo"], "pinned": old, "observed": head,
            "observed_release": releases[0]["tag_name"] if releases else None,
            "new_release": (releases[0]["tag_name"] if releases else None) != row["release"],
            "update_available": head != old, "relationship": diff.get("status","identical"),
            "changed_files": files, "changed_imports": sorted(relevant.intersection(files)),
            "license_review": license_paths,
            "security_signals_not_a_scan": [m for m in messages if re.search(r"security|CVE-|vulnerab|denial.of.service", m, re.I)],
            "limited_comparison": len(files) >= 300 or diff.get("total_commits",0) > len(messages),
            "proposal": ("Review upstream diff, licenses, renamed/deleted imports, advisories and local overlay. "
                         "Test in isolation and obtain explicit adoption approval before changing the pin."
                         if head != old else "No commit update. No external code adopted."),
            "compatibility": "unverified for changed revision" if head != old else row["compatibility"]}


def run(action, target):
    if action == "/upstream":
        rows = registry()
        if not target: return [{"repo": r, "pin": v["pin"], "adoption": v["adoption"]} for r,v in rows.items()]
        if target not in rows: raise ValueError("choose an exact registered owner/repository")
        return check(rows[target])
    def execute(cmd, cwd=None):
        subprocess.run(cmd, cwd=cwd, check=True, timeout=900)
    root = Path(subprocess.check_output(["git","rev-parse","--show-toplevel"],text=True).strip())
    if not (root / "kanz/go.mod").is_file(): raise ValueError("run from the Kanz checkout")
    if action == "/agents":
        from specialists import profiles
        return profiles(root, target)
    if action == "/scan-files":
        if target: raise ValueError("/scan-files takes no target")
        from gaps import scan
        return scan(root)
    if action in {"/doctor","/remember","/recall","/memory","/route","/prioritize"}:
        import intelligence
        return intelligence.run(action,target,root,run)
    if action == "/alerts":
        if target: raise ValueError("/alerts takes no target")
        raw = subprocess.check_output(["gh","api","--paginate","--slurp",
            "repos/eighred/kanz/dependabot/alerts?state=open&per_page=100"],timeout=90)
        alerts = [a for page in json.loads(raw) for a in page]
        return {"repository":"eighred/kanz","alerts":[{"number":a["number"],
            "package":a["dependency"]["package"]["name"],"manifest":a["dependency"]["manifest_path"],
            "advisory":a["security_advisory"]["ghsa_id"],"severity":a["security_vulnerability"]["severity"]}
            for a in alerts],"scope":"GitHub dependency alerts, not proof of call-path reachability"}
    if action == "/trace":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]{0,127}",target): raise ValueError("expected one symbol identifier")
        query = subprocess.run(["git","grep","-n","-w","-F","-e",target,"--","*.go","*.proto","*.py","*.ts"],
                               cwd=root,capture_output=True,text=True,timeout=90)
        if query.returncode not in (0,1): raise ValueError("git grep failed")
        return {"symbol":target,"matches":query.stdout.splitlines()[:100],
                "total":len(query.stdout.splitlines()),"scope":"lexical occurrences, not resolved calls"}
    if action == "/repo":
        if target: raise ValueError("/repo takes no target")
        execute(["git","status","--short","--branch"],root)
        execute(["git","log","-3","--oneline"],root)
        execute(["go","list","-mod=readonly","-f","{{.ImportPath}} -> {{join .Imports \", \"}}","./internal/risk/pricing/..."],root/"kanz")
    elif action == "/financial":
        groups={"pricing":["./internal/risk/pricing","./internal/risk/benchmarks"],"risk":["./internal/risk/compute"],"portfolio":["./internal/performance"],"tca":["./internal/execution/tca"],"backtest":["./tools/backtest"]}
        if target not in groups: raise ValueError("choose "+", ".join(groups))
        proc = subprocess.run(["go","test","-json","-mod=readonly","-p","1","-count=1",*groups[target]],
                              cwd=root/"kanz", stdout=subprocess.PIPE, text=True, timeout=900)
        packages, failed, skipped = [], [], []
        for line in proc.stdout.splitlines():
            event = json.loads(line)
            label = event.get("Package","") + ":" + event.get("Test","")
            if event["Action"] == "skip": skipped.append(label)
            if event["Action"] == "fail": failed.append(label)
            if event["Action"] == "pass" and not event.get("Test"): packages.append(event["Package"])
        summary = {"packages_passed":packages,"failures":failed,"skips":skipped}
        if proc.returncode or failed or len(packages) != len(groups[target]):
            raise ValueError("incomplete or failed test suite: " + json.dumps(summary))
        return summary
    elif action == "/security":
        if target: raise ValueError("/security takes no target")
        registry()
        execute(["node","--test",*[str(p) for p in sorted((PLUGIN/"skills/security-audit").glob("*.test.cjs"))]])
    elif action == "/bench":
        if target: raise ValueError("/bench takes no target")
        import sys,time,statistics
        times=[]
        for _ in range(5):
            start=time.perf_counter()
            execute([sys.executable,str(root/".agents/plugins/kanz-forge/scripts/validate.py"),"--tracked"],root)
            times.append(round((time.perf_counter()-start)*1000,2))
        return {"operation":"checkout Forge validator","milliseconds":times,"median_ms":statistics.median(times)}
    else: raise ValueError("unknown evidence action")
    return {"completed":action,"scope":target or "see command output"}

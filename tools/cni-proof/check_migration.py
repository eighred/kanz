"""Exact-checkout validation of every owned K3s/Cilium gateway proof phase."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

import migration
import proof

ROOT = Path(__file__).resolve().parents[2]


def gateway_module():
    path = ROOT / "tools/gateway-policy-proof/proof.py"
    spec = importlib.util.spec_from_file_location("gateway_migration_evidence", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def validate(report, source, tree, run_id, attempt, policies, render_hash):
    gateway = gateway_module()
    if (report.get("source") != source or report.get("tree") != tree or report.get("runID") != run_id
            or report.get("runAttempt") != attempt or report.get("nodeImage") != migration.NODE
            or report.get("verdict") != "PASS" or report.get("cleanup") != "verified_absent"
            or set(report.get("phases", {})) != {"flannel", "cilium", "rollback"}):
        raise ValueError("migration source or complete lifecycle differs")
    expected = [{"name": p["metadata"]["name"], "namespace": p["metadata"]["namespace"], "sha256": gateway.digest(p)} for p in policies]
    for phase, backend in (("flannel", "flannel"), ("cilium", "cilium"), ("rollback", "flannel")):
        item = report["phases"][phase]
        if item.get("state") != "preserved" or item.get("dns") != "PASS" or item.get("newPVC") != "PASS":
            raise ValueError("migration existing state controls differ")
        proof.validate_report(item.get("policy", {}), source)
        evidence = item.get("gateway", {})
        if (evidence.get("sourceSHA") != source or evidence.get("sourceTree") != tree
                or evidence.get("runID") != run_id or evidence.get("runAttempt") != attempt
                or evidence.get("phase") != phase or evidence.get("backend") != backend
                or evidence.get("renderSHA256") != render_hash or evidence.get("policies") != expected):
            raise ValueError("migration gateway source, phase or actual render differs")
        gateway.validate_union(evidence)


def check(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 1 << 20:
        raise ValueError("invalid migration evidence file")
    def run(args):
        return subprocess.check_output(args, text=True, cwd=ROOT, timeout=30).strip()
    source, tree = run(["git", "rev-parse", "HEAD"]), run(["git", "rev-parse", "HEAD^{tree}"])
    policies, _, render_hash = gateway_module().render(run)
    report = json.loads(path.read_text(encoding="utf-8"))
    validate(report, source, tree, os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"], policies, render_hash)


if __name__ == "__main__":
    check(Path(sys.argv[1]))
    print("All three owned backend phases retain canonical CNI and rendered gateway controls")

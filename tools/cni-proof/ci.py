"""Run the operator's exact probe engine on disposable enforcing kind/Cilium."""
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import uuid

import proof

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("feed_identity_setup", ROOT / "tools/feed-identity/proof.py")
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


def check_disabled_control(result, source):
    expected = ["baseline_" + client + "_" + target
                for client in proof.PHASES["baseline"] for target in ("pod", "service")]
    cases = result.get("cases", [])
    if (result.get("source") != source or result.get("image") != proof.IMAGE or
            result.get("cleanup") != "verified_absent" or result.get("verdict") != "FAIL" or
            len(cases) != 5 or [case["case"] for case in cases[:4]] != expected or
            any(case != {"case": name, "verdict": "PASS", "observation": "echo"}
                for name, case in zip(expected, cases[:4])) or
            cases[4] != {"case": "default_deny_selected_pod", "verdict": "FAIL", "observation": "echo"}):
        raise proof.ProofError("disabled_policy_control_did_not_discriminate")


def loaded_mode(k, expected):
    config = k("-n", "kube-system", "exec", "daemonset/cilium", "-c", "cilium-agent", "--", "cilium-dbg", "config")
    if re.findall(r"(?m)^PolicyEnforcement\s*:\s*(\S+)\s*$", config) != [expected]:
        raise proof.ProofError("unexpected_loaded_Cilium_policy_mode")


def main():
    name = "cni-proof-" + uuid.uuid4().hex[:12]
    artifact = Path(os.environ["RUNNER_TEMP"]) / "cni-proof.json"
    evidence = {"verdict": "FAIL", "cleanup": "unknown"}
    cleaned = False
    with tempfile.TemporaryDirectory(prefix="kanz-cni-ci-") as directory:
        scratch = Path(directory)
        kubeconfig = str(scratch / "kubeconfig")
        try:
            source = setup.command(["git", "rev-parse", "HEAD"])
            setup.create_enforcing_cluster(name, scratch)
            k = proof.Kubectl(["kubectl", "--kubeconfig", kubeconfig, "--context", "kind-" + name])
            loaded_mode(k, "default")
            evidence = proof.run_proof(k, source)
            proof.validate_report(evidence, source)
            # Deliberately disable enforcement ONLY in this newly created CI
            # cluster. The operator engine/wrapper exposes no such override.
            setup.command(["helm", "upgrade", "cilium", str(Path(os.environ["RUNNER_TEMP"]) / "cilium.tgz"),
                           "--kubeconfig", kubeconfig, "--kube-context", "kind-" + name,
                           "--namespace", "kube-system", "--reuse-values", "--set", "policyEnforcementMode=never",
                           "--wait", "--timeout", "5m"], timeout=360)
            k("-n", "kube-system", "rollout", "restart", "daemonset/cilium")
            k("-n", "kube-system", "rollout", "status", "daemonset/cilium", "--timeout=180s", timeout=190)
            loaded_mode(k, "never")
            negative = proof.run_proof(k, source)
            check_disabled_control(negative, source)
            evidence["negativeControl"] = negative
        except (OSError, ValueError, RuntimeError, proof.ProofError, subprocess.TimeoutExpired):
            evidence["verdict"] = "FAIL"
        finally:
            try:
                setup.command(["kind", "delete", "cluster", "--name", name, "--kubeconfig", kubeconfig], timeout=180)
                cleaned = name not in setup.command(["kind", "get", "clusters"]).splitlines()
            except (OSError, ValueError, RuntimeError, proof.ProofError, subprocess.TimeoutExpired):
                cleaned = False
            evidence["ciClusterCleanup"] = "verified_absent" if cleaned else "unknown"
            if not cleaned:
                evidence["verdict"] = "FAIL"
            artifact.write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")
            print(json.dumps(evidence, separators=(",", ":")))
    return 0 if evidence["verdict"] == "PASS" and len(evidence.get("cases", [])) == 16 and "negativeControl" in evidence else 1


if __name__ == "__main__":
    raise SystemExit(main())

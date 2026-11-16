"""Classify the observed failing boundary, never infer an unseen provider cause."""
import re


def classify(log, evidence, artifact=None, prerequisites=None):
    if not isinstance(evidence, str) or not evidence.strip():
        raise ValueError("classification needs an evidence locator")
    if prerequisites:
        failed = [item["id"] for item in prerequisites if item["state"] != "DELIVERABLE"]
        if failed:
            return {"kind": "dependent", "cause_id": None, "requires": failed, "evidence": evidence}
    hosts = sorted(set(re.findall(r"Temporary failure resolving ['\"]([a-zA-Z0-9.-]+)['\"]", log)))
    if hosts:
        return {"kind": "infrastructure", "cause_id": "dns:" + ",".join(hosts),
                "observed": "archive hostname resolution failed", "provider_root_cause": "unestablished", "evidence": evidence}
    if artifact and artifact.get("passed") is False and artifact.get("knownAdvisoryControlPassed") is True:
        native = artifact.get("opensslAdvisory", {})
        advisories = native.get("advisories", [])
        if (artifact.get("failureStage") == "native-openssl-catalog" and native.get("version")
                and advisories and all(re.fullmatch(r"CVE-\d{4}-\d+", item) for item in advisories)):
            return {"kind": "security", "cause_id": "openssl:" + native["version"],
                    "advisories": sorted(advisories), "evidence": evidence}
    identifiers = sorted(set(re.findall(r"Vulnerability #\d+: (GO-\d{4}-\d+)", log)))
    if identifiers and "Found in:" in log and "Fixed in:" in log:
        return {"kind": "security", "cause_id": ",".join(identifiers), "evidence": evidence}
    if "Installation process exited with code: 100" in log:
        return {"kind": "dependency", "cause_id": None, "observed": "package installation failed; cause unclassified", "evidence": evidence}
    return {"kind": "unknown", "cause_id": None, "evidence": evidence,
            "next": "inspect failing step and source/run-bound artifact; do not modify application code from a generic exit"}

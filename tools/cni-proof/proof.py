#!/usr/bin/env python3
"""Synthetic namespace-only CNI proof; no domain-service or caller target inputs."""
import argparse
import ipaddress
import json
import re
import signal
import subprocess
import sys
import time
import uuid

IMAGE = "python:3.13.18-alpine3.24@sha256:7712cfd23e0501f5cb58faa5e6e0d515c9939bdbce866921799b31e696a1d13c"
PREFIX = "kanz-cni-proof-"
PHASES = {
    "baseline": {"selected": "allowed", "unselected": "allowed"},
    "default_deny": {"selected": "blocked", "unselected": "blocked"},
    "selected_allow": {"selected": "allowed", "unselected": "blocked"},
    "restored": {"selected": "allowed", "unselected": "allowed"},
}
SERVER = """import socket,sys,time
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(('0.0.0.0',8080)); s.listen(8); s.settimeout(2)
end=time.monotonic()+300
while time.monotonic()<end:
 try:
  c,_=s.accept()
 except TimeoutError:
  continue
 with c:
  c.settimeout(2)
  try:
   value=c.recv(64)
   if value==sys.argv[1].encode(): c.sendall(value)
  except OSError:
   pass
"""
PROBE = """import errno,ipaddress,json,socket,sys
ipaddress.ip_address(sys.argv[1])
result={'outcome':'unknown','reason':'protocol'}
try:
 s=socket.create_connection((sys.argv[1],8080),timeout=2)
except TimeoutError:
 result={'outcome':'blocked','reason':'connect_timeout'}
except OSError as e:
 if e.errno in (errno.ECONNREFUSED,errno.EHOSTUNREACH):
  result={'outcome':'blocked','reason':'connect_refused'}
 else:
  result={'outcome':'unknown','reason':'connect_error'}
else:
 with s:
  try:
   s.sendall(sys.argv[2].encode())
   if s.recv(64)==sys.argv[2].encode(): result={'outcome':'allowed','reason':'echo'}
  except OSError:
   pass
print(json.dumps(result,separators=(',',':')))
"""


class ProofError(Exception):
    """Only fixed, non-secret failure categories leave the engine."""


class Kubectl:
    def __init__(self, prefix):
        self.prefix = list(prefix)

    def __call__(self, *args, value=None, timeout=25):
        try:
            result = subprocess.run(
                self.prefix + ["--request-timeout=20s", *args],
                input=None if value is None else json.dumps(value),
                capture_output=True, text=True, timeout=timeout, check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            raise ProofError("kubectl_unavailable_or_timeout") from None
        if result.returncode or len(result.stdout) > 1 << 20:
            # Fixed diagnostic only: raw stderr can contain operational data.
            # A timeout remains a failed proof, never an observed policy denial.
            if result.returncode and any(marker in result.stderr.lower() for marker in (
                    "context deadline exceeded", "timed out waiting for the condition", "client.timeout exceeded")):
                raise ProofError("kubectl_reported_timeout")
            if result.returncode and "error from server (notfound)" in result.stderr.lower():
                raise ProofError("kubectl_reported_not_found")
            raise ProofError("kubectl_failed_or_oversized")
        return result.stdout.strip()


def namespace_name():
    return PREFIX + uuid.uuid4().hex


def validate_namespace(namespace):
    if not re.fullmatch(PREFIX + r"[0-9a-f]{32}", namespace):
        raise ProofError("invalid_fixture_namespace")


def objects(namespace, nonce):
    validate_namespace(namespace)
    items = []
    for name in ("server", "selected", "unselected"):
        command = (["python", "-I", "-B", "-c", SERVER, nonce] if name == "server"
                   else ["python", "-I", "-B", "-c", "import time;time.sleep(300)"])
        container = {
            "name": "fixture", "image": IMAGE, "imagePullPolicy": "IfNotPresent",
            "command": command,
            "securityContext": {"allowPrivilegeEscalation": False,
                                "readOnlyRootFilesystem": True,
                                "capabilities": {"drop": ["ALL"]}},
            "resources": {"requests": {"cpu": "10m", "memory": "24Mi"},
                          "limits": {"cpu": "100m", "memory": "64Mi"}},
        }
        if name == "server":
            container["readinessProbe"] = {"tcpSocket": {"port": 8080},
                                           "periodSeconds": 1, "timeoutSeconds": 1}
        items.append({"apiVersion": "v1", "kind": "Pod",
                      "metadata": {"name": name, "namespace": namespace,
                                   "labels": {"kanz.io/cni-fixture": name}},
                      "spec": {"automountServiceAccountToken": False,
                               "enableServiceLinks": False, "restartPolicy": "Never",
                               "activeDeadlineSeconds": 300, "terminationGracePeriodSeconds": 1,
                               "dnsPolicy": "None", "dnsConfig": {"nameservers": ["127.0.0.1"]},
                               "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                                                   "runAsGroup": 65532,
                                                   "seccompProfile": {"type": "RuntimeDefault"}},
                               "containers": [container]}})
    items.append({"apiVersion": "v1", "kind": "Service",
                  "metadata": {"name": "server", "namespace": namespace},
                  "spec": {"selector": {"kanz.io/cni-fixture": "server"},
                           "ports": [{"port": 8080, "targetPort": 8080, "protocol": "TCP"}]}})
    # Permanent egress confinement; test policies only change fixture ingress.
    items.append(policy(namespace, "fixture-egress", {
        "podSelector": {}, "policyTypes": ["Egress"],
        "egress": [{"to": [{"podSelector": {"matchLabels": {"kanz.io/cni-fixture": "server"}}}],
                    "ports": [{"protocol": "TCP", "port": 8080}]}],
    }))
    return {"apiVersion": "v1", "kind": "List", "items": items}


def policy(namespace, name, spec):
    validate_namespace(namespace)
    return {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
            "metadata": {"name": name, "namespace": namespace}, "spec": spec}


def decode_probe(raw):
    try:
        value = json.loads(raw)
    except (TypeError, ValueError):
        raise ProofError("invalid_probe_output") from None
    if value not in ({"outcome": "allowed", "reason": "echo"},
                     {"outcome": "blocked", "reason": "connect_timeout"},
                     {"outcome": "blocked", "reason": "connect_refused"}):
        raise ProofError("unknown_probe_result")
    return value


def validate_report(report, source):
    """One evidence contract, used by both CI and the canonical host wrapper."""
    if (set(report) != {"format", "source", "image", "namespace", "images", "cases", "cleanup", "verdict"} or
            report["format"] != "kanz-cni-proof-v1" or report["source"] != source or
            report["image"] != IMAGE or report["cleanup"] != "verified_absent" or report["verdict"] != "PASS"):
        raise ProofError("invalid_report_binding")
    validate_namespace(report["namespace"])
    if set(report["images"]) != {"server", "selected", "unselected"}:
        raise ProofError("incomplete_loaded_images")
    for image_id in report["images"].values():
        validate_image_id(image_id)
    expected = {phase + "_" + client + "_" + target: outcome
                for phase, clients in PHASES.items() for client, outcome in clients.items()
                for target in ("pod", "service")}
    seen = set()
    if not isinstance(report["cases"], list) or len(report["cases"]) != len(expected):
        raise ProofError("incomplete_cases")
    for case in report["cases"]:
        if set(case) != {"case", "verdict", "observation"}:
            raise ProofError("invalid_case")
        name = case["case"]
        if name not in expected or name in seen or case["verdict"] != "PASS":
            raise ProofError("missing_or_duplicate_case")
        reasons = {"echo"} if expected[name] == "allowed" else {"connect_timeout", "connect_refused"}
        if case["observation"] not in reasons:
            raise ProofError("wrong_case_observation")
        seen.add(name)


def validate_image_id(value):
    digest = IMAGE.split("@", 1)[1]
    if (not isinstance(value, str) or not re.fullmatch(r"[a-zA-Z0-9./:@_-]{1,200}", value) or
            (value != digest and not value.endswith("@" + digest))):
        raise ProofError("unexpected_loaded_image")


def inspect_pod(pod):
    containers = pod["spec"].get("containers", [])
    statuses = pod["status"].get("containerStatuses", [])
    if (len(containers) != 1 or containers[0].get("image") != IMAGE or
            len(statuses) != 1 or not statuses[0].get("ready") or statuses[0].get("restartCount") != 0):
        raise ProofError("unexpected_pod_image_or_state")
    image_id = statuses[0].get("imageID")
    validate_image_id(image_id)
    return pod["metadata"]["uid"], image_id


def run_proof(k, source):
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ProofError("invalid_source")
    namespace, nonce = namespace_name(), uuid.uuid4().hex
    validate_namespace(namespace)
    evidence = {"format": "kanz-cni-proof-v1", "source": source,
                "image": IMAGE, "namespace": namespace, "images": {}, "cases": [],
                "cleanup": "unknown", "verdict": "FAIL"}
    owned_uid = None
    active_case = "setup"
    failed_observation = "unknown"
    started = time.monotonic()
    try:
        # CREATE refuses an existing namespace. Never adopt or overwrite one.
        created = json.loads(k("create", "-f", "-", "-o", "json", value={
            "apiVersion": "v1", "kind": "Namespace",
            "metadata": {"name": namespace, "labels": {
                "kanz.io/cni-proof": nonce,
                "pod-security.kubernetes.io/enforce": "restricted",
                "pod-security.kubernetes.io/enforce-version": "v1.36"}}}))
        owned_uid = created["metadata"]["uid"]
        if not owned_uid:
            raise ProofError("missing_namespace_uid")
        k("-n", namespace, "create", "-f", "-", value=objects(namespace, nonce))
        k("-n", namespace, "wait", "--for=condition=Ready", "pod", "--all", "--timeout=90s", timeout=100)
        pod_uids = {}
        for name in ("server", "selected", "unselected"):
            pod = json.loads(k("-n", namespace, "get", "pod", name, "-o", "json"))
            pod_uids[name], evidence["images"][name] = inspect_pod(pod)
        server = json.loads(k("-n", namespace, "get", "pod", "server", "-o", "json"))
        server_uid = server["metadata"]["uid"]
        pod_ip = server["status"]["podIP"]
        service = json.loads(k("-n", namespace, "get", "service", "server", "-o", "json"))
        addresses = {"pod": pod_ip, "service": service["spec"]["clusterIP"]}
        for address in addresses.values():
            ipaddress.ip_address(address)

        def healthy():
            for name in pod_uids:
                pod = json.loads(k("-n", namespace, "get", "pod", name, "-o", "json"))
                identity = inspect_pod(pod)
                if identity != (pod_uids[name], evidence["images"][name]):
                    raise ProofError("fixture_pod_identity_changed")
            current = json.loads(k("-n", namespace, "get", "pod", "server", "-o", "json"))
            statuses = current["status"].get("containerStatuses", [])
            if (current["metadata"]["uid"] != server_uid or current["status"].get("podIP") != pod_ip or
                    len(statuses) != 1 or not statuses[0].get("ready") or statuses[0].get("restartCount") != 0):
                raise ProofError("server_changed_or_unhealthy")
            raw = k("-n", namespace, "exec", "server", "--", "python", "-I", "-B", "-c", PROBE, "127.0.0.1", nonce)
            if decode_probe(raw)["outcome"] != "allowed":
                raise ProofError("server_self_check_failed")

        def phase(name, expectations):
            nonlocal active_case, failed_observation
            active_case = name + "_server_health"
            healthy()
            for client, expected in expectations.items():
                for target, address in addresses.items():
                    active_case = name + "_" + client + "_" + target
                    # Policy propagation may lag. Only valid socket observations
                    # are retried; exec/transport/protocol errors are never denial.
                    deadline = min(started + 240, time.monotonic() + 20)
                    consecutive, result = 0, None
                    while time.monotonic() < deadline:
                        raw = k("-n", namespace, "exec", client, "--", "python", "-I", "-B", "-c", PROBE, address, nonce)
                        result = decode_probe(raw)
                        consecutive = consecutive + 1 if result["outcome"] == expected else 0
                        if consecutive == 3:
                            break
                        time.sleep(0.3)
                    if consecutive != 3:
                        failed_observation = result["reason"] if result else "unknown"
                        raise ProofError("policy_observation_not_stable")
                    evidence["cases"].append({"case": name + "_" + client + "_" + target,
                                              "verdict": "PASS", "observation": result["reason"]})
            healthy()

        phase("baseline", PHASES["baseline"])
        k("-n", namespace, "create", "-f", "-", value=policy(namespace, "deny-ingress", {
            "podSelector": {}, "policyTypes": ["Ingress"], "ingress": []}))
        phase("default_deny", PHASES["default_deny"])
        k("-n", namespace, "create", "-f", "-", value=policy(namespace, "selected-ingress", {
            "podSelector": {"matchLabels": {"kanz.io/cni-fixture": "server"}},
            "policyTypes": ["Ingress"], "ingress": [{
                "from": [{"podSelector": {"matchLabels": {"kanz.io/cni-fixture": "selected"}}}],
                "ports": [{"protocol": "TCP", "port": 8080}]}]}))
        phase("selected_allow", PHASES["selected_allow"])
        k("-n", namespace, "delete", "networkpolicy", "deny-ingress", "selected-ingress", "--wait=true")
        phase("restored", PHASES["restored"])
        evidence["verdict"] = "PASS"
    except (ProofError, KeyError, ValueError, TypeError):
        evidence["cases"].append({"case": active_case, "verdict": "FAIL", "observation": failed_observation})
        evidence["verdict"] = "FAIL"
    finally:
        try:
            raw = k("get", "namespace", namespace, "--ignore-not-found", "-o", "json")
            if not raw:
                evidence["cleanup"] = "verified_absent"
            else:
                current = json.loads(raw)
                metadata = current["metadata"]
                if (metadata.get("labels", {}).get("kanz.io/cni-proof") != nonce or
                        (owned_uid and metadata["uid"] != owned_uid)):
                    raise ProofError("namespace_ownership_changed")
                # UID precondition closes the read/delete replacement race.
                k("delete", "--raw", "/api/v1/namespaces/" + namespace, "-f", "-", value={
                    "apiVersion": "v1", "kind": "DeleteOptions",
                    "preconditions": {"uid": metadata["uid"]}, "propagationPolicy": "Foreground"})
                k("wait", "--for=delete", "namespace/" + namespace, "--timeout=60s", timeout=65)
                remaining = k("get", "namespace", namespace, "--ignore-not-found", "-o", "name")
                if remaining:
                    raise ProofError("cleanup_incomplete")
                evidence["cleanup"] = "verified_absent"
        except (ProofError, KeyError, ValueError, TypeError):
            evidence["cleanup"] = "unknown"
        if evidence["cleanup"] != "verified_absent":
            evidence["verdict"] = "FAIL"
    return evidence


def interrupted(_signum, _frame):
    raise ProofError("interrupted")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--tokyo", action="store_true")
    mode.add_argument("--verify-report", action="store_true")
    args = parser.parse_args()
    if args.verify_report:
        try:
            raw = sys.stdin.read(65537)
            if len(raw) > 65536:
                raise ProofError("oversized_report")
            validate_report(json.loads(raw), args.source)
        except (ProofError, ValueError, TypeError, KeyError):
            raise SystemExit("CNI proof evidence rejected") from None
        raise SystemExit(0)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    kubectl = Kubectl(["/usr/local/bin/k3s", "kubectl"])
    # Observed via read-only SSM on 2026-11-06. Reinstallation requires a
    # separately reviewed identity update, not an operator override.
    cluster = json.loads(kubectl("get", "namespace", "kube-system", "-o", "json"))
    if cluster["metadata"]["uid"] != "86ea9839-5806-461d-b2d1-afd1ff5d87ff":
        raise SystemExit("CNI proof refused: unexpected Tokyo cluster identity")
    result = run_proof(kubectl, args.source)
    print(json.dumps(result, separators=(",", ":")))
    sys.exit(0 if result["verdict"] == "PASS" else 1)

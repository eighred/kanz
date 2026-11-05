"""Disposable K3s CNI transition qualification. Never accepts a live target."""
import copy
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import time
import uuid

import proof

NODE = "rancher/k3s:v1.36.4-k3s1@sha256:8b3560e0a99b2f22832b2c963550952ed69830320d4ee712693c52026ada7b89"
LABEL = "io.kanz.cni-migration"
CNI_DIR = "/var/lib/rancher/k3s/agent/etc/cni/net.d"
BASE_CONFIG = {"cluster-init": True, "disable": ["traefik", "servicelb", "metrics-server"],
               "secrets-encryption": True, "write-kubeconfig-mode": "0600"}


def cpu_limit():
    return "--cpus=" + str(min(4, os.cpu_count() or 1))


def command(args, data=None, timeout=120):
    # Raw process output can contain a K3s bootstrap token. Never print it.
    result = subprocess.run(args, input=data, capture_output=True, text=True, timeout=timeout)
    if result.returncode or len(result.stdout) > 1 << 20:
        if "cpus" in result.stderr.lower() and "range" in result.stderr.lower():
            raise proof.ProofError("fixture_CPU_limit_exceeds_host")
        raise proof.ProofError("fixture_command_failed")
    return result.stdout.strip()


def validate_name(name):
    if not re.fullmatch(r"kanz-cni-migration-[0-9a-f]{24}", name):
        raise proof.ProofError("invalid_disposable_name")


class Fixture:
    def __init__(self, name):
        validate_name(name)
        self.name = name
        self.volume = name + "-data"
        self.mode = "flannel"
        self.stage = "initialize"
        self.k = proof.Kubectl(["docker", "exec", "-i", name, "kubectl"])

    def inside(self, *args, data=None, timeout=120):
        return command(["docker", "exec", "-i", self.name, *args], data, timeout)

    def ownership(self):
        obj = json.loads(command(["docker", "inspect", self.name]))[0]
        expected_id = command(["docker", "image", "inspect", NODE, "--format", "{{.Id}}"])
        if (obj["Config"]["Labels"].get(LABEL) != self.name or obj["Image"] != expected_id or
                obj["Config"]["Image"] != NODE or
                any(m["Type"] == "bind" for m in obj["Mounts"])):
            raise proof.ProofError("disposable_ownership_mismatch")
        return obj["Id"]

    def datapath(self):
        active = self.inside("sh", "-ceu", "find " + CNI_DIR + " -maxdepth 1 -name '*.conflist' | sort | head -1")
        if active not in (CNI_DIR + "/05-cilium.conflist", CNI_DIR + "/10-flannel.conflist"):
            raise proof.ProofError("unexpected_active_CNI_file")
        conf = json.loads(self.inside("cat", active))
        types = [plugin.get("type") for plugin in conf.get("plugins", [])]
        if self.mode == "cilium":
            if "cilium-cni" not in types or "flannel" in types:
                raise proof.ProofError("Cilium_not_active_CNI")
            self.k("-n", "kube-system", "exec", "daemonset/cilium", "-c", "cilium-agent", "--", "cilium-dbg", "status", "--brief")
            config = self.k("-n", "kube-system", "exec", "daemonset/cilium", "-c", "cilium-agent", "--", "cilium-dbg", "config")
            if re.findall(r"(?m)^PolicyEnforcement\s*:\s*(\S+)\s*$", config) != ["default"]:
                raise proof.ProofError("Cilium_policy_not_enabled")
        else:
            if "flannel" not in types or "cilium-cni" in types:
                raise proof.ProofError("Flannel_not_active_CNI")
            if self.k("-n", "kube-system", "get", "daemonset", "cilium", "--ignore-not-found", "-o", "name"):
                raise proof.ProofError("Cilium_remains_after_rollback")
            self.inside("sh", "-ceu", "iptables-save > /tmp/policy-check; grep -q KUBE-ROUTER /tmp/policy-check")
        self.inside("iptables", "-t", "nat", "-S", "KUBE-SERVICES")

    def configure(self, cilium):
        self.ownership()
        config = copy.deepcopy(BASE_CONFIG)
        if cilium:
            config.update({"flannel-backend": "none", "disable-network-policy": True})
        self.inside("sh", "-ceu", "mkdir -p /etc/rancher/k3s; cat > /etc/rancher/k3s/config.yaml",
                    data=json.dumps(config))

    def ready(self):
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            try:
                nodes = json.loads(self.k("get", "nodes", "-o", "json"))["items"]
                if len(nodes) == 1 and any(c["type"] == "Ready" and c["status"] == "True"
                                          for c in nodes[0]["status"].get("conditions", [])):
                    return nodes[0]
            except (proof.ProofError, ValueError):
                pass
            time.sleep(2)
        raise proof.ProofError("node_not_ready")

    def api_ready(self):
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            try:
                if self.k("get", "--raw=/readyz") == "ok":
                    return
            except proof.ProofError:
                pass
            time.sleep(2)
        raise proof.ProofError("api_not_ready")

    def recycle_system(self):
        # Existing pod network namespaces do not become Cilium endpoints merely
        # because CNI config changed. Exercise the actual DNS/storage owners.
        for deployment in ("coredns", "local-path-provisioner"):
            self.k("-n", "kube-system", "scale", "deployment/" + deployment, "--replicas=1")
            self.k("-n", "kube-system", "rollout", "restart", "deployment/" + deployment)
            self.k("-n", "kube-system", "rollout", "status", "deployment/" + deployment,
                   "--timeout=180s", timeout=190)

    def initial_system_ready(self):
        # Node Ready is not an addon-controller creation barrier in K3s.
        for deployment in ("coredns", "local-path-provisioner"):
            self.stage = "system_presence_" + deployment
            deadline = time.monotonic() + 180
            while time.monotonic() < deadline:
                if self.k("-n", "kube-system", "get", "deployment", deployment, "--ignore-not-found", "-o", "name"):
                    break
                time.sleep(2)
            else:
                raise proof.ProofError("packaged_system_deployment_absent")
            self.stage = "system_rollout_" + deployment
            self.k("-n", "kube-system", "rollout", "status", "deployment/" + deployment, "--timeout=180s", timeout=190)

    def quiesce_system(self):
        for deployment in ("coredns", "local-path-provisioner"):
            obj = json.loads(self.k("-n", "kube-system", "get", "deployment", deployment, "-o", "json"))
            if obj["spec"].get("replicas") != 1:
                raise proof.ProofError("unexpected_fixture_system_replicas")
            self.k("-n", "kube-system", "scale", "deployment/" + deployment, "--replicas=0")
        self.k("-n", "kube-system", "wait", "--for=delete", "pod", "-l", "k8s-app=kube-dns", "--timeout=60s", timeout=70)
        self.k("-n", "kube-system", "wait", "--for=delete", "pod", "-l", "app=local-path-provisioner", "--timeout=60s", timeout=70)

    def storage(self, claim, marker, create):
        if create:
            self.stage = "storage_apply_claim"
            self.k("apply", "-f", "-", value={"apiVersion": "v1", "kind": "PersistentVolumeClaim",
                   "metadata": {"name": claim, "namespace": self.name},
                   "spec": {"accessModes": ["ReadWriteOnce"], "storageClassName": "local-path",
                            "resources": {"requests": {"storage": "16Mi"}}}})
        script = ("from pathlib import Path;import socket,time;"
                  "socket.getaddrinfo('kubernetes.default.svc.cluster.local',443);"
                  + ("Path('/state/marker').write_text('" + marker + "');" if create else "")
                  + "assert Path('/state/marker').read_text()=='" + marker + "';time.sleep(120)")
        pod = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "storage-check", "namespace": self.name},
               "spec": {"automountServiceAccountToken": False, "restartPolicy": "Never", "activeDeadlineSeconds": 180,
                        "securityContext": {"runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532,
                                            "runAsNonRoot": True, "seccompProfile": {"type": "RuntimeDefault"}},
                        "containers": [{"name": "check", "image": proof.IMAGE, "command": ["python3", "-c", script],
                                        "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                                            "capabilities": {"drop": ["ALL"]}},
                                        "resources": {"requests": {"cpu": "10m", "memory": "24Mi"},
                                                      "limits": {"cpu": "100m", "memory": "64Mi"}},
                                        "volumeMounts": [{"name": "state", "mountPath": "/state"}]}],
                        "volumes": [{"name": "state", "persistentVolumeClaim": {"claimName": claim}}]}}
        self.stage = "storage_apply_pod"
        self.k("apply", "-f", "-", value=pod)
        self.stage = "storage_wait_pod"
        self.k("-n", self.name, "wait", "--for=condition=Ready", "pod/storage-check", "--timeout=120s", timeout=130)
        self.stage = "storage_image"
        loaded = json.loads(self.k("-n", self.name, "get", "pod/storage-check", "-o", "json"))
        image_id = loaded["status"]["containerStatuses"][0].get("imageID", "")
        if not image_id.endswith("@" + proof.IMAGE.split("@")[1]):
            raise proof.ProofError("storage_image_digest_mismatch")
        if self.mode == "cilium":
            owned = False
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                raw = self.k("-n", self.name, "get", "ciliumendpoint", "storage-check", "--ignore-not-found", "-o", "json")
                if raw and json.loads(raw).get("status", {}).get("id", 0) > 0:
                    owned = True
                    break
                time.sleep(1)
            if not owned:
                raise proof.ProofError("new_pod_not_owned_by_Cilium")
        # Ready alone precedes the script's write/assert; explicit exec observes
        # both persisted bytes and functional cluster DNS after startup.
        self.stage = "storage_DNS_and_marker"
        observed = self.k("-n", self.name, "exec", "storage-check", "--", "python3", "-c",
                          "from pathlib import Path;import socket,time;socket.getaddrinfo('kubernetes.default.svc.cluster.local',443);"
                          "p=Path('/state/marker');end=time.monotonic()+10\nwhile not p.exists() and time.monotonic()<end: time.sleep(.1)\nprint(p.read_text())")
        if observed != marker:
            raise proof.ProofError("persistent_marker_mismatch")
        self.stage = "storage_identity"
        pvc = json.loads(self.k("-n", self.name, "get", "pvc", claim, "-o", "json"))
        pv = json.loads(self.k("get", "pv", pvc["spec"]["volumeName"], "-o", "json"))
        self.stage = "storage_delete_probe"
        self.k("-n", self.name, "delete", "pod/storage-check", "--wait=true", "--timeout=60s", timeout=70)
        return {"pvc": pvc["metadata"]["uid"], "pv": pv["metadata"]["uid"], "imageID": image_id}

    def storage_diagnostic(self):
        # Only finite status categories from our own synthetic Pod; never logs,
        # termination messages, Pod specification, kubeconfig or node token.
        raw = self.k("-n", self.name, "get", "pod", "storage-check", "--ignore-not-found", "-o", "json")
        if not raw:
            return {"pod": "absent"}
        status = json.loads(raw).get("status", {})
        phases = {"Pending", "Running", "Succeeded", "Failed", "Unknown"}
        reasons = {"ContainerCreating", "ErrImagePull", "ImagePullBackOff", "CrashLoopBackOff", "Completed", "Error", "OOMKilled", "RunContainerError", "CreateContainerError"}
        result = {"pod": status.get("phase") if status.get("phase") in phases else "Unknown", "containers": []}
        for entry in status.get("containerStatuses", [])[:1]:
            state = entry.get("state", {})
            detail = state.get("waiting", state.get("terminated", {}))
            result["containers"].append({"reason": detail.get("reason") if detail.get("reason") in reasons else "Other",
                                          "exitCode": detail.get("exitCode") if isinstance(detail.get("exitCode"), int) else None})
        return result

    def switch_to_cilium(self):
        self.quiesce_system()
        self.inside("cp", CNI_DIR + "/10-flannel.conflist", "/tmp/original-flannel.conflist")
        self.configure(True)
        command(["docker", "restart", "--timeout", "30", self.name], timeout=90)
        self.api_ready()
        # K3s deliberately retains kube-router rules when disabled. Remove only
        # those rules inside this owned fixture, before Cilium owns enforcement.
        self.inside("sh", "-ceu", "set -o pipefail; iptables-save > /tmp/before-policy-rules; test -s /tmp/before-policy-rules; grep -v KUBE-ROUTER /tmp/before-policy-rules > /tmp/after-policy-rules; test -s /tmp/after-policy-rules; iptables-restore < /tmp/after-policy-rules")
        self.inside("sh", "-ceu", "for link in flannel.1 cni0; do if ip link show \"$link\" >/dev/null 2>&1; then ip link delete \"$link\"; fi; done")
        self.inside("helm", "install", "cilium", "/tmp/cilium.tgz", "--kubeconfig", "/etc/rancher/k3s/k3s.yaml",
                    "--namespace", "kube-system", "--set", "kubeProxyReplacement=false",
                    "--set", "ipam.mode=kubernetes", "--set", "operator.replicas=1",
                    "--set", "cni.binPath=/var/lib/rancher/k3s/data/cni",
                    "--set", "cni.confPath=" + CNI_DIR,
                    "--wait", "--timeout", "5m", timeout=330)
        self.ready()
        self.recycle_system()
        self.mode = "cilium"

    def rollback(self):
        self.ownership()
        self.quiesce_system()
        # Retain the installed version's cleanup binary within the fixture only.
        self.inside("sh", "-ceu", "kubectl -n kube-system exec daemonset/cilium -c cilium-agent -- cat /usr/bin/cilium-dbg > /tmp/cilium-dbg; chmod 0700 /tmp/cilium-dbg")
        self.inside("helm", "uninstall", "cilium", "--kubeconfig", "/etc/rancher/k3s/k3s.yaml",
                    "--namespace", "kube-system", "--wait", "--timeout", "3m", timeout=200)
        self.inside("/tmp/cilium-dbg", "post-uninstall-cleanup", "--all-state", "--force")
        self.inside("sh", "-ceu", "rm -f " + CNI_DIR + "/05-cilium.conflist; cp /tmp/original-flannel.conflist " + CNI_DIR + "/10-flannel.conflist")
        self.configure(False)
        command(["docker", "restart", "--timeout", "30", self.name], timeout=90)
        self.ready()
        self.recycle_system()
        self.mode = "flannel"


def main():
    name = "kanz-cni-migration-" + uuid.uuid4().hex[:24]
    f = Fixture(name)
    source = command(["git", "rev-parse", "HEAD"])
    evidence = {"source": source, "nodeImage": NODE, "verdict": "FAIL", "phases": {}, "cleanup": "unknown"}
    phase = "create"
    try:
        command(["docker", "volume", "create", "--label", LABEL + "=" + name, f.volume])
        phase = "docker_run"
        command(["docker", "run", "-d", "--name", name, "--hostname", name, "--label", LABEL + "=" + name,
                 "--privileged", cpu_limit(), "--memory=5g", "--pids-limit=4096", "--log-driver=none",
                 "--tmpfs", "/run", "--tmpfs", "/var/run", "--mount", "type=volume,src=" + f.volume + ",dst=/var/lib/rancher",
                 NODE, "server", "--cluster-init", "--secrets-encryption", "--disable=traefik", "--disable=servicelb", "--disable=metrics-server"], timeout=240)
        phase = "node_identity"
        container_id = f.ownership()
        evidence["nodeImageID"] = command(["docker", "image", "inspect", NODE, "--format", "{{.Id}}"])
        phase = "node_version"
        version = f.inside("k3s", "--version").splitlines()[0]
        if not version.startswith("k3s version v1.36.4+k3s1 "):
            raise proof.ProofError("unexpected_K3s_version")
        evidence["nodeVersion"] = version
        phase = "copy_verified_tools"
        for src, dst in ((shutil.which("helm"), "/bin/helm"), (str(Path(os.environ["RUNNER_TEMP"]) / "cilium.tgz"), "/tmp/cilium.tgz")):
            command(["docker", "cp", src, name + ":" + dst])
        phase = "initial_node_ready"
        node = f.ready()
        node_uid = node["metadata"]["uid"]
        phase = "initial_system_ready"
        f.initial_system_ready()
        f.k("create", "namespace", name)
        nonce = uuid.uuid4().hex
        f.k("apply", "-f", "-", value={"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "etcd-marker", "namespace": name}, "data": {"marker": nonce}})
        phase = "initial_storage"
        original = f.storage("retained", nonce, True)
        config_uid = json.loads(f.k("-n", name, "get", "configmap", "etcd-marker", "-o", "json"))["metadata"]["uid"]
        evidence["preservedIdentities"] = {"node": node_uid, "etcdObject": config_uid,
                                          "pvc": original["pvc"], "pv": original["pv"]}
        for phase in ("flannel", "cilium", "rollback"):
            if phase == "cilium":
                f.switch_to_cilium()
            elif phase == "rollback":
                f.rollback()
            if f.ownership() != container_id or f.ready()["metadata"]["uid"] != node_uid:
                raise proof.ProofError("node_identity_changed")
            f.inside("test", "-d", "/var/lib/rancher/k3s/server/db/etcd")
            cm = json.loads(f.k("-n", name, "get", "configmap", "etcd-marker", "-o", "json"))
            if cm["metadata"]["uid"] != config_uid or cm["data"] != {"marker": nonce}:
                raise proof.ProofError("etcd_state_changed")
            if f.storage("retained", nonce, False) != original:
                raise proof.ProofError("persistent_volume_identity_changed")
            f.storage("fresh-" + phase, nonce, True)
            f.datapath()
            report = proof.run_proof(f.k, source)
            proof.validate_report(report, source)
            evidence["phases"][phase] = {"state": "preserved", "dns": "PASS", "newPVC": "PASS", "storageImageID": original["imageID"], "policy": report}
        evidence["verdict"] = "PASS"
    except (OSError, ValueError, RuntimeError, proof.ProofError, subprocess.TimeoutExpired) as error:
        evidence["failedPhase"] = phase
        evidence["failedStage"] = f.stage
        evidence["reason"] = str(error) if isinstance(error, proof.ProofError) else "fixture_transport_or_decode_failure"
        try:
            evidence["storageDiagnostic"] = f.storage_diagnostic()
        except (OSError, ValueError, proof.ProofError, subprocess.TimeoutExpired):
            evidence["storageDiagnostic"] = {"pod": "unknown"}
    finally:
        try:
            # Names originate only in this process; ownership is checked before
            # destructive cleanup, including partially completed fixture runs.
            if name in command(["docker", "ps", "-a", "--format", "{{.Names}}"]).splitlines():
                f.ownership()
                command(["docker", "rm", "--force", "--volumes", name])
            if f.volume in command(["docker", "volume", "ls", "--format", "{{.Name}}"]).splitlines():
                vol = json.loads(command(["docker", "volume", "inspect", f.volume]))[0]
                if vol.get("Labels", {}).get(LABEL) != name:
                    raise proof.ProofError("volume_ownership_mismatch")
                command(["docker", "volume", "rm", f.volume])
            if name in command(["docker", "ps", "-a", "--format", "{{.Names}}"]).splitlines() or f.volume in command(["docker", "volume", "ls", "--format", "{{.Name}}"]).splitlines():
                raise proof.ProofError("fixture_cleanup_incomplete")
            evidence["cleanup"] = "verified_absent"
        except (OSError, ValueError, RuntimeError, proof.ProofError, subprocess.TimeoutExpired):
            evidence["verdict"] = "FAIL"
        Path(os.environ["RUNNER_TEMP"], "k3s-cni-migration.json").write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(evidence, separators=(",", ":")))
    return 0 if evidence["verdict"] == "PASS" and len(evidence["phases"]) == 3 and evidence["cleanup"] == "verified_absent" else 1


if __name__ == "__main__":
    raise SystemExit(main())

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
# K3s v1.36.4+k3s1 sets its bundled CNI paths only while Flannel is enabled
# (pkg/executor/embed/embed.go). With backend=none, containerd uses these defaults.
CILIUM_CNI_DIR = "/etc/cni/net.d"
CILIUM_CNI_BIN = "/opt/cni/bin"
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
        if any(marker in result.stderr.lower() for marker in ("context deadline exceeded", "timed out waiting")):
            raise proof.ProofError("fixture_reported_timeout")
        raise proof.ProofError("fixture_command_failed")
    return result.stdout.strip()


def validate_name(name):
    if not re.fullmatch(r"kanz-cni-migration-[0-9a-f]{24}", name):
        raise proof.ProofError("invalid_disposable_name")


def runtime_diagnostic(message):
    # Kubernetes runtime messages can include paths or credentials. Export only
    # a fixed category; never copy a message, substring, or arbitrary mount path.
    if not isinstance(message, str) or len(message) > 4096:
        return "unknown"
    value = message.lower()
    if "not a shared or slave mount" in value or "not a shared mount" in value:
        for path, category in (("/sys/fs/bpf", "bpffs_not_shared"),
                               ("/run/cilium/cgroupv2", "cgroup_not_shared"),
                               ("/var/run/netns", "netns_not_shared"),
                               ("/run/cilium/netns", "netns_not_shared")):
            if path in value:
                return category
        return "mount_not_shared"
    for marker, category in (("apparmor", "apparmor_refused"),
                             ("seccomp", "seccomp_refused"),
                             ("read-only file system", "read_only_filesystem"),
                             ("permission denied", "permission_denied"),
                             ("operation not permitted", "operation_not_permitted")):
        if marker in value:
            return category
    return "unknown"


def mount_inventory(raw):
    result = {}
    for line in raw.splitlines():
        left, right = line.split(" - ", 1)
        fields = left.split()
        if len(fields) < 6 or not right.split() or fields[0] in result:
            raise proof.ProofError("invalid_mount_inventory")
        tags = frozenset(flag for flag in fields[6:] if flag.startswith(("shared:", "master:")))
        result[fields[0]] = (fields[4], right.split()[0], tags)
    return result


def private_mount_boundary(before, after=None, new_id=None, *, target="/sys/fs/bpf", filesystem="bpf"):
    if (target, filesystem) not in {("/sys/fs/bpf", "bpf"), ("/run/cilium/cgroupv2", "cgroup2"),
                                     ("/var/run/netns", "tmpfs"), ("/run/netns", "tmpfs")}:
        raise proof.ProofError("unapproved_fixture_mount")
    if not {"/", "/sys"}.issubset({row[0] for row in before.values()}):
        raise proof.ProofError("mount_ancestors_unknown")
    for path, _, tags in before.values():
        if (path == target or target.startswith(path.rstrip("/") + "/")) and tags:
            raise proof.ProofError("mount_ancestor_not_private")
        if path == target or path.startswith(target + "/"):
            raise proof.ProofError("preexisting_fixture_mount_refused")
    if filesystem == "tmpfs":
        parent = target.rsplit("/", 1)[0]
        if len([row for row in before.values() if row == (parent, "tmpfs", frozenset())]) != 1:
            raise proof.ProofError("netns_parent_not_private_tmpfs")
    if after is None:
        return None
    matches = [(identity, row) for identity, row in after.items() if row[0] == target]
    if len(matches) != 1 or matches[0][0] in before or matches[0][1][1] != filesystem:
        raise proof.ProofError("new_fixture_mount_not_identified")
    identity, (_, _, tags) = matches[0]
    if new_id is None:
        if tags:
            raise proof.ProofError("new_fixture_mount_not_private")
    elif identity != new_id or not any(tag.startswith("shared:") for tag in tags) or any(tag.startswith("master:") for tag in tags):
        raise proof.ProofError("new_fixture_mount_shared_state_not_verified")
    # Only the newly created fixture mount may change propagation.
    for other_id, (path, _, other_tags) in after.items():
        if other_id == identity:
            continue
        old = before.get(other_id)
        if (old is None and other_tags) or (old is not None and other_tags != old[2]):
            raise proof.ProofError("unrelated_mount_propagation_changed")
        if target.startswith(path.rstrip("/") + "/") and other_tags:
            raise proof.ProofError("mount_ancestor_not_private")
    for old_id, row in before.items():
        if target.startswith(row[0].rstrip("/") + "/") and after.get(old_id) != row:
            raise proof.ProofError("mount_ancestor_changed")
    return identity


class Fixture:
    def __init__(self, name):
        validate_name(name)
        self.name = name
        self.volume = name + "-data"
        self.mode = "flannel"
        self.stage = "initialize"
        self.rule_diagnostic = {"status": "not_started"}
        self.k = proof.Kubectl(["docker", "exec", "-i", name, "kubectl"])

    def inside(self, *args, data=None, timeout=120):
        return command(["docker", "exec", "-i", self.name, *args], data, timeout)

    def ownership(self):
        obj = json.loads(command(["docker", "inspect", self.name]))[0]
        expected_id = command(["docker", "image", "inspect", NODE, "--format", "{{.Id}}"])
        if (obj["Config"]["Labels"].get(LABEL) != self.name or obj["Image"] != expected_id or
                obj["Config"]["Image"] != NODE or
                obj.get("HostConfig", {}).get("PidMode", "") not in ("", "private") or
                obj.get("HostConfig", {}).get("CgroupnsMode") != "private" or
                obj.get("HostConfig", {}).get("NetworkMode") == "host" or
                any(m["Type"] == "bind" for m in obj["Mounts"])):
            raise proof.ProofError("disposable_ownership_mismatch")
        return obj["Id"]

    def datapath(self):
        directory = CILIUM_CNI_DIR if self.mode == "cilium" else CNI_DIR
        active = self.inside("sh", "-ceu", "find " + directory + " -maxdepth 1 -name '*.conflist' | sort | head -1")
        expected = directory + ("/05-cilium.conflist" if self.mode == "cilium" else "/10-flannel.conflist")
        if active != expected:
            raise proof.ProofError("unexpected_active_CNI_file")
        conf = json.loads(self.inside("cat", active))
        types = [plugin.get("type") for plugin in conf.get("plugins", [])]
        if self.mode == "cilium":
            if "cilium-cni" not in types or "flannel" in types:
                raise proof.ProofError("Cilium_not_active_CNI")
            self.inside("test", "-x", CILIUM_CNI_BIN + "/cilium-cni")
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

    def service_rules_ready(self):
        # API readiness does not mean kube-proxy has populated the new network
        # namespace after Docker restart. Never turn an empty save into a pass.
        self.stage = "cilium_wait_service_rules"
        self.inside("iptables", "--version", timeout=15)
        self.inside("iptables-save", "--version", timeout=15)
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            try:
                self.inside("iptables", "-t", "nat", "-S", "KUBE-SERVICES", timeout=15)
                if self.inside("iptables-save", timeout=15).strip():
                    return
            except proof.ProofError:
                pass
            time.sleep(2)
        raise proof.ProofError("service_rules_not_ready")

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

    def cilium_diagnostic(self):
        pods = json.loads(self.k("-n", "kube-system", "get", "pods", "-l", "k8s-app=cilium", "-o", "json"))["items"]
        allowed = {"ContainerCreating", "PodInitializing", "ErrImagePull", "ImagePullBackOff", "CrashLoopBackOff", "Completed", "Error", "OOMKilled", "RunContainerError", "CreateContainerError"}
        names = {"config", "mount-cgroup", "apply-sysctl-overwrites", "mount-bpf-fs", "clean-cilium-state", "install-cni-binaries", "cilium-agent"}
        result = []
        for pod in pods[:2]:
            status = pod.get("status", {})
            states = []
            for item in (status.get("initContainerStatuses", []) + status.get("containerStatuses", []))[:16]:
                state = item.get("state", {})
                detail = state.get("waiting", state.get("terminated", {}))
                states.append({"name": item.get("name") if item.get("name") in names else "Other",
                               "reason": detail.get("reason") if detail.get("reason") in allowed else "Other",
                               "diagnostic": runtime_diagnostic(detail.get("message")),
                               "running": "running" in state, "ready": item.get("ready") is True,
                               "exitCode": detail.get("exitCode") if isinstance(detail.get("exitCode"), int) else None})
            result.append(states)
        return result

    def remove_old_policy_rules(self):
        # Capture success and content separately. A failed save is never treated
        # as an empty ruleset, and an unchanged ruleset is never restored.
        self.stage = "cilium_save_old_rules"
        rules = self.inside("iptables-save")
        lines = rules.splitlines()
        old = sum("KUBE-ROUTER" in line for line in lines)
        self.rule_diagnostic = {"status": "captured", "lines": len(lines), "oldPolicyLines": old}
        if not lines:
            raise proof.ProofError("saved_rules_empty")
        if old == 0:
            self.rule_diagnostic["status"] = "no_old_rules"
            return
        filtered = "\n".join(line for line in lines if "KUBE-ROUTER" not in line) + "\n"
        if not filtered.strip():
            raise proof.ProofError("filtered_rules_lack_tables")
        self.stage = "cilium_validate_filtered_rules"
        self.inside("iptables-restore", "--test", data=filtered)
        self.stage = "cilium_restore_filtered_rules"
        self.inside("iptables-restore", data=filtered)
        self.stage = "cilium_verify_old_rules_absent"
        if "KUBE-ROUTER" in self.inside("iptables-save"):
            raise proof.ProofError("old_policy_rules_remain")
        self.rule_diagnostic["status"] = "removed_verified"

    def prepare_private_mount(self, target, filesystem):
        # Nested Pods need propagation from these new fixture-only mounts.
        # Never change propagation on an inherited filesystem or its ancestors.
        self.stage = "cilium_private_" + filesystem + "_preflight"
        self.ownership()
        before = mount_inventory(self.inside("cat", "/proc/self/mountinfo"))
        private_mount_boundary(before, target=target, filesystem=filesystem)
        if filesystem == "cgroup2":
            self.inside("test", "!", "-L", "/run/cilium")
            self.inside("test", "!", "-L", target)
            self.inside("mkdir", "-p", target)
        self.stage = "cilium_private_" + filesystem + "_mount"
        self.inside("mount", "-t", filesystem, "none", target)
        mounted = mount_inventory(self.inside("cat", "/proc/self/mountinfo"))
        identity = private_mount_boundary(before, mounted, target=target, filesystem=filesystem)
        self.inside("mount", "--make-shared", target)
        self.stage = "cilium_private_" + filesystem + "_verify"
        shared = mount_inventory(self.inside("cat", "/proc/self/mountinfo"))
        private_mount_boundary(before, shared, identity, target=target, filesystem=filesystem)
        self.ownership()

    def qualify_stale_policy_cleanup(self):
        # Docker restart creates a fresh network namespace. Explicit synthetic
        # RETURN-only rules qualify the nonempty cleanup branch; they are not
        # evidence of real residual rules after a host service restart.
        old = "KUBE-ROUTER-KANZ-FIXTURE"
        keep = "KANZ-CNI-KEEP"
        self.stage = "cilium_synthetic_rules_preflight"
        self.ownership()
        before = self.inside("iptables-save")
        if not before.strip() or old in before or keep in before:
            raise proof.ProofError("synthetic_rule_fixture_not_fresh")
        self.stage = "cilium_synthetic_rules_create"
        for chain in (old, keep):
            self.inside("iptables", "-w", "5", "-N", chain)
            self.inside("iptables", "-w", "5", "-A", chain, "-j", "RETURN")
            self.inside("iptables", "-w", "5", "-A", "INPUT", "-j", chain)
        sentinel = self.inside("iptables", "-S", keep)
        if sentinel.splitlines() != ["-N " + keep, "-A " + keep + " -j RETURN"]:
            raise proof.ProofError("synthetic_sentinel_not_identified")
        self.ownership()
        self.remove_old_policy_rules()
        self.stage = "cilium_synthetic_rules_verify"
        if self.rule_diagnostic.get("status") != "removed_verified" or self.rule_diagnostic.get("oldPolicyLines", 0) < 3:
            raise proof.ProofError("nonempty_policy_cleanup_not_exercised")
        if old in self.inside("iptables-save") or self.inside("iptables", "-S", keep) != sentinel:
            raise proof.ProofError("synthetic_rule_cleanup_mismatch")
        self.inside("iptables", "-C", "INPUT", "-j", keep)
        self.inside("iptables", "-t", "nat", "-S", "KUBE-SERVICES")
        # Remove only our preflight-absent sentinel after proving preservation.
        self.inside("iptables", "-w", "5", "-D", "INPUT", "-j", keep)
        self.inside("iptables", "-w", "5", "-D", keep, "-j", "RETURN")
        self.inside("iptables", "-w", "5", "-X", keep)
        if keep in self.inside("iptables-save"):
            raise proof.ProofError("synthetic_sentinel_cleanup_incomplete")
        self.ownership()
        self.rule_diagnostic["syntheticStaleRules"] = "removed_with_unrelated_rules_preserved"

    def prepare_private_netns(self):
        # The chart consumes the node's /var/run/netns. Bind only an empty
        # directory on this fixture's private tmpfs; never hide live handles.
        self.stage = "cilium_private_netns_preflight"
        self.ownership()
        parent = self.inside("readlink", "-f", "/var/run")
        if parent not in ("/run", "/var/run"):
            raise proof.ProofError("netns_parent_unexpected")
        target = parent + "/netns"
        before = mount_inventory(self.inside("cat", "/proc/self/mountinfo"))
        private_mount_boundary(before, target=target, filesystem="tmpfs")
        # Fixed positional path, no interpolation. Refuse symlinks, non-directory
        # entries and existing namespace handles before creating a self-bind.
        self.inside("sh", "-ceu", 'test ! -L "$1"; if test -e "$1"; then test -d "$1"; else mkdir "$1"; fi; entries=$(find "$1" -mindepth 1 -maxdepth 1 -print -quit); test -z "$entries"', "netns-preflight", target)
        self.stage = "cilium_private_netns_mount"
        self.inside("mount", "--bind", target, target)
        mounted = mount_inventory(self.inside("cat", "/proc/self/mountinfo"))
        identity = private_mount_boundary(before, mounted, target=target, filesystem="tmpfs")
        self.inside("mount", "--make-shared", target)
        self.stage = "cilium_private_netns_verify"
        shared = mount_inventory(self.inside("cat", "/proc/self/mountinfo"))
        private_mount_boundary(before, shared, identity, target=target, filesystem="tmpfs")
        self.ownership()

    def switch_to_cilium(self):
        self.stage = "cilium_quiesce_system"
        self.quiesce_system()
        self.stage = "cilium_capture_flannel_config"
        self.inside("cp", CNI_DIR + "/10-flannel.conflist", "/tmp/original-flannel.conflist")
        self.configure(True)
        self.stage = "cilium_restart_K3s"
        command(["docker", "restart", "--timeout", "30", self.name], timeout=90)
        self.stage = "cilium_API_ready"
        self.api_ready()
        self.service_rules_ready()
        # K3s deliberately retains kube-router rules when disabled. Remove only
        # those rules inside this owned fixture, before Cilium owns enforcement.
        self.qualify_stale_policy_cleanup()
        self.stage = "cilium_remove_old_links"
        self.inside("sh", "-ceu", "for link in flannel.1 cni0; do if ip link show \"$link\" >/dev/null 2>&1; then ip link delete \"$link\"; fi; done")
        self.prepare_private_mount("/sys/fs/bpf", "bpf")
        self.prepare_private_mount("/run/cilium/cgroupv2", "cgroup2")
        self.prepare_private_netns()
        self.stage = "cilium_install"
        self.inside("helm", "install", "cilium", "/tmp/cilium.tgz", "--kubeconfig", "/etc/rancher/k3s/k3s.yaml",
                    "--namespace", "kube-system", "--set", "kubeProxyReplacement=false",
                    "--set", "ipam.mode=kubernetes", "--set", "operator.replicas=1",
                    "--set", "cni.binPath=" + CILIUM_CNI_BIN,
                    "--set", "cni.confPath=" + CILIUM_CNI_DIR,
                    "--wait", "--timeout", "5m", timeout=330)
        self.stage = "cilium_node_ready"
        self.ready()
        self.stage = "cilium_restore_system"
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
        self.inside("sh", "-ceu", "rm -f " + CILIUM_CNI_DIR + "/05-cilium.conflist; cp /tmp/original-flannel.conflist " + CNI_DIR + "/10-flannel.conflist")
        self.configure(False)
        command(["docker", "restart", "--timeout", "30", self.name], timeout=90)
        self.ready()
        self.recycle_system()
        self.mode = "flannel"


def rendered_gateway_proof(f, source, phase):
    # The executor remains bound to this owned K3s container, never a host context.
    import importlib.util
    import yaml
    path = Path(__file__).resolve().parents[1] / 'gateway-policy-proof/proof.py'
    spec = importlib.util.spec_from_file_location('rendered_gateway_proof', path)
    gateway = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(gateway)
    policies, labels, rendered = gateway.render(command)
    report = {'sourceSHA': source, 'sourceTree': command(['git', 'rev-parse', 'HEAD^{tree}']),
              'runID': os.environ['GITHUB_RUN_ID'], 'runAttempt': os.environ['GITHUB_RUN_ATTEMPT'],
              'backend': f.mode, 'phase': phase, 'renderSHA256': rendered, 'observations': {},
              'policies': [{'name': p['metadata']['name'], 'namespace': p['metadata']['namespace'],
                            'sha256': gateway.digest(p)} for p in policies]}
    def k(*args, data=None, timeout=90):
        values = list(yaml.safe_load_all(data)) if data is not None else []
        value = ({'apiVersion': 'v1', 'kind': 'List', 'items': values} if len(values) > 1
                 else values[0] if values else None)
        return f.k(*args, value=value, timeout=min(timeout, 90))
    gateway.qualify_union(k, policies, labels, report)
    gateway.validate_union(report)
    return report


def main():
    name = "kanz-cni-migration-" + uuid.uuid4().hex[:24]
    f = Fixture(name)
    source = command(["git", "rev-parse", "HEAD"])
    evidence = {"source": source, "tree": command(["git", "rev-parse", "HEAD^{tree}"]),
                "runID": os.environ["GITHUB_RUN_ID"], "runAttempt": os.environ["GITHUB_RUN_ATTEMPT"], "nodeImage": NODE, "verdict": "FAIL", "phases": {}, "cleanup": "unknown"}
    phase = "create"
    try:
        command(["docker", "volume", "create", "--label", LABEL + "=" + name, f.volume])
        phase = "docker_run"
        command(["docker", "run", "-d", "--name", name, "--hostname", name, "--label", LABEL + "=" + name,
                 "--privileged", "--cgroupns=private", cpu_limit(), "--memory=5g", "--pids-limit=4096", "--log-driver=none",
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
                if (f.rule_diagnostic.get("status") != "removed_verified" or
                        f.rule_diagnostic.get("oldPolicyLines", 0) < 3 or
                        f.rule_diagnostic.get("syntheticStaleRules") != "removed_with_unrelated_rules_preserved"):
                    raise proof.ProofError("synthetic_policy_qualification_missing")
                evidence["oldPolicyCleanup"] = f.rule_diagnostic
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
            evidence["phases"][phase] = {"state": "preserved", "dns": "PASS", "newPVC": "PASS", "storageImageID": original["imageID"], "policy": report,
                                          "gateway": rendered_gateway_proof(f, source, phase)}
        evidence["verdict"] = "PASS"
    except (OSError, ValueError, RuntimeError, proof.ProofError, subprocess.TimeoutExpired) as error:
        evidence["failedPhase"] = phase
        evidence["failedStage"] = f.stage
        evidence["oldPolicyCleanup"] = f.rule_diagnostic
        evidence["reason"] = str(error) if isinstance(error, proof.ProofError) else "fixture_transport_or_decode_failure"
        try:
            evidence["storageDiagnostic"] = f.storage_diagnostic()
        except (OSError, ValueError, proof.ProofError, subprocess.TimeoutExpired):
            evidence["storageDiagnostic"] = {"pod": "unknown"}
        if phase == "cilium":
            try:
                evidence["ciliumDiagnostic"] = f.cilium_diagnostic()
            except (OSError, ValueError, proof.ProofError, subprocess.TimeoutExpired):
                evidence["ciliumDiagnostic"] = "unknown"
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

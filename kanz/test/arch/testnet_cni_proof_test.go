package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This guard binds the operator mutation boundary to the exact reviewed probe
// and its executable CI proof; the Python tests exercise failure classification.
var cniProofRequirements = map[string][]string{
	"tools/Test-TestnetCNI.ps1": {
		"tools/cni-proof/proof.py tools/gateway-policy-proof/network_observation.py *> $null", "$verifierBytes.Length + $networkBytes.Length",
		"$networkBytes.Length) -gt 24576", "[Text.UTF8Encoding]::new($false, $true)", "'set -C'",
		"rmdir", `base64 -d > "$work/gateway-policy-proof/network_observation.py"`, "$parameters.Length -gt 30000",
		"fetch origin main --quiet", "$headCommit -ne $releaseCommit",
		"git -C $repoRoot ls-files --error-unmatch tools/Test-TestnetCNI.ps1 tools/cni-proof/proof.py",
		"git -C $repoRoot diff --quiet $releaseCommit -- tools/Test-TestnetCNI.ps1 tools/cni-proof/proof.py",
		"$Region = 'ap-northeast-1'", "$InstanceId = 'i-0ee1235614219f089'", "$AccountId = '012619468098'",
		"$identity.Account -ne $AccountId", "$instance.Id -ne $InstanceId", "$instance.State -ne 'running'",
		"executionTimeout = @('540')", "timeout --signal=TERM --kill-after=70s 400s python3 -I -B",
		"$report.source -ne $releaseCommit", "$report.image -ne $expectedImage",
		"$report.cleanup -ne 'verified_absent'", "@($report.cases).Count -ne 16",
		"--verify-report --source $releaseCommit",
	},
	"tools/cni-proof/proof.py": {
		`network_observation.py`, `compile(_network_source`, `CONNECT_SOURCE`,
		`PREFIX = "kanz-cni-proof-"`, `r"[0-9a-f]{32}"`, `"86ea9839-5806-461d-b2d1-afd1ff5d87ff"`,
		`"automountServiceAccountToken": False`, `"readOnlyRootFilesystem": True`,
		`"allowPrivilegeEscalation": False`, `"capabilities": {"drop": ["ALL"]}`,
		`"runAsNonRoot": True`, `"seccompProfile": {"type": "RuntimeDefault"}`,
		`"activeDeadlineSeconds": 300`, `"limits": {"cpu": "100m", "memory": "64Mi"}`,
		`phase("baseline",`, `phase("default_deny",`, `phase("selected_allow",`, `phase("restored",`,
		`raise ProofError("unknown_probe_result")`, `raise ProofError("kubectl_failed_or_oversized")`,
		`metadata.get("labels", {}).get("kanz.io/cni-proof") != nonce`,
		`(owned_uid and metadata["uid"] != owned_uid)`, `if evidence["cleanup"] != "verified_absent":`,
		`"preconditions": {"uid": metadata["uid"]}`, `containers[0].get("image") != IMAGE`,
		`name not in expected or name in seen`, `case["observation"] not in reasons`,
		`sha256:7712cfd23e0501f5cb58faa5e6e0d515c9939bdbce866921799b31e696a1d13c`,
	},
	"tools/cni-proof/ci.py": {
		`setup.create_enforcing_cluster(name, scratch)`, `evidence = proof.run_proof(k, source)`, `negative = proof.run_proof(k, source)`,
		`len(evidence.get("cases", [])) == 16`, `"policyEnforcementMode=never"`, `loaded_mode(k, "never")`, `check_disabled_control(negative, source)`, `name not in setup.command(["kind", "get", "clusters"])`,
	},
	".github/workflows/testnet-cni-proof.yml": {
		"disposable-cni:", "contents: read", "persist-credentials: false", "timeout-minutes: 20",
		"python3 -B -m unittest discover -s tools/cni-proof", "bash tools/feed-identity/install.sh",
		"python3 -B tools/cni-proof/ci.py", "if-no-files-found: error",
	},
	"tools/cni-proof/storage_signal.py": {
		`progress["imageStopSignal"] = image_stop_signal()`,
		`values[0]["Config"].get("StopSignal", "") not in ("", "SIGTERM", "15")`,
		`if sys.platform != "linux":`, "            case(repaired, progress)", `for repaired in (False, True):`,
		`migration.STORAGE_SHUTDOWN if repaired else ""`, `assert os.getpid()==1`,
		`"--network=none", "--read-only", "--user=65532:65532", "--cap-drop=ALL"`,
		`"--memory=64m", "--cpus=0.1", "--pids-limit=8"`,
		`"docker", "kill", "--signal=TERM", identity`, `"docker", "wait", identity`,
		`state["ExitCode"] != 0 or state["OOMKilled"]`,
		`obj["Id"] != identity`, `get(LABEL) != nonce`, `obj.get("Name") != "/" + name`,
		`obj = owned(name, nonce, identity)`, `"signal container cleanup incomplete"`,
	},
	"tools/cni-proof/migration.py": {
		`STORAGE_SHUTDOWN = "import signal,sys;signal.signal(signal.SIGTERM,lambda *_:sys.exit(0));"`,
		`script = (STORAGE_SHUTDOWN + "from pathlib import Path;import socket,time;"`,
		`"delete", "pod/storage-check", "--wait=true", "--timeout=60s", timeout=70`,
		`"gateway": rendered_gateway_proof(f, source, phase)`, `gateway.qualify_union`, `gateway.validate_union`,
		`r"kanz-cni-migration-[0-9a-f]{24}"`, `any(m["Type"] == "bind"`,
		`str(min(4, os.cpu_count() or 1))`, `cpu_limit(), "--memory=5g", "--pids-limit=4096", "--log-driver=none"`,
		`"--cluster-init", "--secrets-encryption"`, `"kubeProxyReplacement=false"`,
		`def recycle_system(self):`, `def quiesce_system(self):`, `"node_identity_changed"`,
		`"etcd_state_changed"`, `"persistent_volume_identity_changed"`, `f.storage("fresh-" + phase, nonce, True)`,
		`for phase in ("flannel", "cilium", "rollback")`, `proof.validate_report(report, source)`,
		`"cleanup"] == "verified_absent"`, `vol.get("Labels", {}).get(LABEL) != name`,
		`rules = self.inside("iptables-save")`, `self.inside("iptables-restore", "--test", data=filtered)`, `self.inside("iptables-restore", data=filtered)`,
		`"new_pod_not_owned_by_Cilium"`, `"storage_image_digest_mismatch"`,
		`"unexpected_K3s_version"`, `"Cilium_remains_after_rollback"`, `f.datapath()`,
		`private_mount_boundary(before, target=target, filesystem=filesystem)`, `private_mount_boundary(before, mounted, target=target, filesystem=filesystem)`, `private_mount_boundary(before, shared, identity, target=target, filesystem=filesystem)`,
		`"unrelated_mount_propagation_changed"`, `"preexisting_fixture_mount_refused"`, `"unapproved_fixture_mount"`,
		`self.prepare_private_mount("/sys/fs/bpf", "bpf")`, `self.prepare_private_mount("/run/cilium/cgroupv2", "cgroup2")`,
		`"--cgroupns=private"`, `obj.get("HostConfig", {}).get("CgroupnsMode") != "private"`,
		`self.service_rules_ready()`, `"service_rules_not_ready"`,
	},
	".github/workflows/k3s-cni-migration.yml": {
		`python3 -B tools/cni-proof/check_migration.py`, `python3 -B tools/cni-proof/storage_signal.py`,
		"disposable-transition:", "persist-credentials: false", "timeout-minutes: 30",
		"python3 -B tools/cni-proof/migration.py", "bash tools/feed-identity/install.sh",
		"if-no-files-found: error",
	},
	"tools/cni-proof/check_migration.py": {
		`{"flannel", "cilium", "rollback"}`, `gateway.validate_union`, `proof.validate_report`,
		`evidence.get("runAttempt") != attempt`, `report.get("tree") != tree`, `evidence.get("backend") != backend`, `evidence.get("renderSHA256") != render_hash`,
	},
	"tools/gateway-policy-proof/rollout_engine.py": {
		`self.k("get", "pods", "-A", "-l", probe.OWNER + "=" + self.nonce, "-o", "json")`,
		`len(items) != len(self.owned)`, `if key in pods:`, `self.observe(entry, obj)`,
		`if set(pods) != expected:`, `obj = pods[(entry["namespace"], entry["name"])]`,
		`TCP_BATCH as TCP`, `or "unknown" in values:`, `for _ in range(6)`,
		`consecutive = consecutive + 1 if accepted else 0`, `if consecutive >= 3`,
		`final_end = min(self.started + 420, time.monotonic() + 180)`,
		`active_end = min(final_end, time.monotonic() + 10)`,
		`"format": "kanz-gateway-network-v1"`, `"isolated": isolated`, `if type(isolated) is not bool:`, `"network_verified" if passed else "network_refused"`,
	},
	"tools/gateway-policy-proof/network_fixture.py": {
		`OWNER = "kanz.io/gateway-proof"`, `def selector_independent(policies, labels):`,
		`if key == OWNER:`, `elif value == OWNER:`, `if any(OWNER in value for value in labels.values()):`,
		`query("get", "pods", "-A", "-l", OWNER + "=" + nonce, "-o", "json")`,
		`len(expected) != 8`, `len(items) != 8`, `key not in expected or key in result`,
		`meta["labels"].get(OWNER) != nonce`, `if set(result) != set(expected):`,
		`if snapshot(probes.k, expected, image, nonce) != identities:`,
	},
	"tools/gateway-policy-proof/proof.py": {
		`network.selector_independent(policies, labels)`, `{**labels[target], network.OWNER: nonce}`,
		`IMAGE, 'closed', nonce)`, `IMAGE, 'no_policy', nonce)`, `IMAGE, 'enforced', nonce)`,
	},
	"tools/gateway-policy-proof/network_observation.py": {
		`errno.ECONNREFUSED`, `errno.EHOSTUNREACH`, `ipaddress.ip_address(address)`,
		`return None,'unknown'`, `type(port) is not int`,
	},
	"tools/gateway-policy-proof/transaction_fixture.py": {
		`durable(value)`, `raw != tx.encoded(value) + b"\n"`, `validate_network_receipt(report["networkReceipt"])`,
		`durable.close()`,
	},
	".github/workflows/gateway-policy-proof.yml": {
		`name: Measure all actual operator wrappers across 96 variants`,
		`name: Measure actual isolated CNI wrapper across 32 variants`,
		`python3 -B -m unittest test_network_framing.NetworkFramingTests.test_actual_wrappers_all_96_source_variants`,
		`python3 -B -m unittest test_transfer.CNITransferTests.test_actual_powershell_32_cni_variants`,
	},
	".github/scripts/release_ci.py": {`"testnet-cni-proof.yml": {"disposable-cni"}`, `"k3s-cni-migration.yml": {"disposable-transition"}`},
}

func checkCNIProofBoundary(files map[string]string) error {
	for path, markers := range cniProofRequirements {
		for _, marker := range markers {
			if !strings.Contains(files[path], marker) {
				return fmt.Errorf("%s lacks required CNI proof boundary %q", path, marker)
			}
		}
	}
	operator := files["tools/Test-TestnetCNI.ps1"]
	ssm := strings.Index(operator, "$aws ssm send-command")
	for _, prior := range []string{"git -C $repoRoot diff --quiet", "$identity.Account -ne $AccountId", "$instance.Id -ne $InstanceId"} {
		if at := strings.Index(operator, prior); at < 0 || ssm < at {
			return fmt.Errorf("CNI proof must verify %s before SSM", prior)
		}
	}
	engine := files["tools/cni-proof/proof.py"]
	for _, forbidden := range []string{`add_argument("--namespace"`, `add_argument("--target"`, `"hostNetwork": True`, `"privileged": True`} {
		if strings.Contains(engine, forbidden) {
			return fmt.Errorf("CNI fixture must not expose %s", forbidden)
		}
	}
	return nil
}

func TestTestnetCNIProofIsScopedAndVerified(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	files := map[string]string{}
	for path := range cniProofRequirements {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(data)
	}
	if err := checkCNIProofBoundary(files); err != nil {
		t.Fatal(err)
	}
	for path, markers := range cniProofRequirements {
		for _, marker := range markers {
			t.Run(path+"/"+marker, func(t *testing.T) {
				original := files[path]
				files[path] = strings.Replace(original, marker, "REMOVED_BOUNDARY", 1)
				defer func() { files[path] = original }()
				if err := checkCNIProofBoundary(files); err == nil {
					t.Fatal("removed boundary was accepted")
				}
			})
		}
	}
}

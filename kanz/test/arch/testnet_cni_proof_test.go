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
	"tools/cni-proof/migration.py": {
		`r"kanz-cni-migration-[0-9a-f]{24}"`, `any(m["Type"] == "bind"`,
		`"--cpus=4", "--memory=5g", "--pids-limit=4096", "--log-driver=none"`,
		`"--cluster-init", "--secrets-encryption"`, `"kubeProxyReplacement=false"`,
		`def recycle_system(self):`, `def quiesce_system(self):`, `"node_identity_changed"`,
		`"etcd_state_changed"`, `"persistent_volume_identity_changed"`, `f.storage("fresh-" + phase, nonce, True)`,
		`for phase in ("flannel", "cilium", "rollback")`, `proof.validate_report(report, source)`,
		`"cleanup"] == "verified_absent"`, `vol.get("Labels", {}).get(LABEL) != name`,
		`set -o pipefail; iptables-save`, `"new_pod_not_owned_by_Cilium"`, `"storage_image_digest_mismatch"`,
		`"unexpected_K3s_version"`, `"Cilium_remains_after_rollback"`, `f.datapath()`,
	},
	".github/workflows/k3s-cni-migration.yml": {
		"disposable-transition:", "persist-credentials: false", "timeout-minutes: 30",
		"python3 -B tools/cni-proof/migration.py", "bash tools/feed-identity/install.sh",
		"if-no-files-found: error",
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

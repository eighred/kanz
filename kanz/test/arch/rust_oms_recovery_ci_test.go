package arch

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkRecoveryWorkflow(raw []byte) error {
	var workflow struct {
		Jobs map[string]struct {
			If    string `yaml:"if"`
			Steps []struct {
				Name     string
				If       string
				Run      string
				Continue bool `yaml:"continue-on-error"`
				Env      map[string]string
				With     map[string]string
			}
		}
	}
	if e := yaml.Unmarshal(raw, &workflow); e != nil {
		return e
	}
	job, ok := workflow.Jobs["recovery-coordinator"]
	if !ok || job.If != "" {
		return fmt.Errorf("recovery job absent or conditional")
	}
	required := map[string][]string{
		"Compile the actual recovery owner and fixture":        {"cargo test --locked -p kanz-oms-recovery --all-targets", "cargo clippy --locked -p kanz-oms-recovery --all-targets -- -D warnings", "cargo build --locked -p kanz-oms-recovery --example recovery-coordinator-fixture"},
		"Start separate owned recovery dependencies":           {"postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9", "--name oms-recovery-pg", "--name oms-recovery-default-pg", "kanz.fixture.run=$GITHUB_RUN_ID", "-c max_locks_per_transaction=1024 -c max_connections=100 -c max_prepared_transactions=0", "127.0.0.1:55432:5432", "127.0.0.1:55433:5432", "NOSUPERUSER NOBYPASSRLS", "kanz/infra/nats/bootstrap-job.yaml"},
		"Qualify actual recovery and complete large histories": {"set -euo pipefail", "go test -race -p 1 ./services/oms/tools/recovery-conformance -skip '^TestRustRecoveryComplete8000History$' -timeout 20m", "-run '^TestRustRecoveryComplete8000History/go$' -timeout 20m", "-run '^TestRustRecoveryComplete8000History/rust$' -timeout 20m", "check_oms_recovery_evidence.py", "--create --source \"$GITHUB_SHA\" --tree \"$(git rev-parse HEAD^{tree})\" --run \"$GITHUB_RUN_ID\" --attempt \"$GITHUB_RUN_ATTEMPT\""},
		"Remove only owned recovery dependencies":              {"for container in oms-recovery-pg oms-recovery-default-pg oms-recovery-nats", "docker inspect --format", "kanz.fixture.run", "docker rm -f \"$container\""},
	}
	seen := map[string]bool{}
	for _, step := range job.Steps {
		markers, requiredStep := required[step.Name]
		if !requiredStep {
			continue
		}
		if seen[step.Name] || step.Continue {
			return fmt.Errorf("duplicate or ignored recovery step")
		}
		seen[step.Name] = true
		if step.Name == "Remove only owned recovery dependencies" {
			if step.If != "always()" {
				return fmt.Errorf("cleanup not unconditional")
			}
		} else if step.If != "" {
			return fmt.Errorf("conditional recovery proof")
		}
		for _, marker := range markers {
			if !strings.Contains(step.Run, marker) {
				return fmt.Errorf("recovery step %s missing %s", step.Name, marker)
			}
		}
		if step.Name == "Qualify actual recovery and complete large histories" {
			if !strings.Contains(step.Env["TEST_POSTGRES_URL"], "@127.0.0.1:55432/") || !strings.Contains(step.Env["TEST_POSTGRES_DEFAULT_LOCK_URL"], "@127.0.0.1:55433/") || step.Env["KANZ_OMS_RECOVERY_VERSION"] != "${{ github.sha }}" {
				return fmt.Errorf("recovery dependency/source binding")
			}
		}
	}
	if len(seen) != len(required) {
		return fmt.Errorf("missing recovery step")
	}
	return nil
}
func TestRustRecoveryWorkflowCannotSkipActualOwner(t *testing.T) {
	path := filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/rust-oms-fold.yml")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = checkRecoveryWorkflow(raw); e != nil {
		t.Fatal(e)
	}
	for _, marker := range []string{"  recovery-coordinator:", "--name oms-recovery-default-pg", "max_locks_per_transaction=1024", "-run '^TestRustRecoveryComplete8000History/go$'", "-run '^TestRustRecoveryComplete8000History/rust$'", "--attempt \"$GITHUB_RUN_ATTEMPT\"", "NOSUPERUSER NOBYPASSRLS", "kanz.fixture.run=$GITHUB_RUN_ID"} {
		t.Run(marker, func(t *testing.T) {
			changed := strings.ReplaceAll(string(raw), marker, "removed")
			if changed == string(raw) {
				t.Fatal("mutation absent")
			}
			if checkRecoveryWorkflow([]byte(changed)) == nil {
				t.Fatal("required recovery proof mutation accepted")
			}
		})
	}
}

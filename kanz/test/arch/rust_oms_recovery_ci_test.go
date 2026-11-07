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
		"Compile the actual recovery owner and fixture":        {"cargo test --locked -p kanz-oms-recovery --all-targets", "cargo test --locked -p kanz-oms-store --features qualification-diagnostics diagnostics::tests", "cargo clippy --locked -p kanz-oms-recovery --all-targets -- -D warnings", "cargo build --release --locked -p kanz-oms-recovery --example recovery-coordinator-fixture", "cargo clippy --locked -p kanz-oms-recovery --all-targets --features qualification-diagnostics -- -D warnings", "cargo build --release --locked -p kanz-oms-recovery --example recovery-coordinator-fixture --features qualification-diagnostics"},
		"Start separate owned recovery dependencies":           {`docker exec -e PGPASSWORD=fixture -e PGCONNECT_TIMEOUT=2 "$container" psql -X -w -h 127.0.0.1 -U fixture -d fixture -v ON_ERROR_STOP=1`, `PGCONNECT_TIMEOUT=2`, `PGOPTIONS=-c statement_timeout=2000`, `psql -X -w -h 127.0.0.1 -U fixture -d fixture -v ON_ERROR_STOP=1 -Atqc 'SELECT 1'`, `2>/dev/null)" = 1; then ready=true; break; fi`, "postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9", "--name oms-recovery-pg", "--name oms-recovery-default-pg", "--name oms-recovery-constrained-pg", "-c max_locks_per_transaction=10 -c max_connections=100 -c max_prepared_transactions=0", "127.0.0.1:55434:5432", "kanz.fixture.run=$GITHUB_RUN_ID", "kanz.fixture.attempt=$GITHUB_RUN_ATTEMPT", "-c max_locks_per_transaction=1024 -c max_connections=100 -c max_prepared_transactions=0", "127.0.0.1:55432:5432", "127.0.0.1:55433:5432", "NOSUPERUSER NOBYPASSRLS", "kanz/infra/nats/bootstrap-job.yaml"},
		"Qualify actual recovery and complete large histories": {"set -euo pipefail", "qualification_status=0", `sha256sum "$KANZ_OMS_RECOVERY_BIN"`, `exit "$qualification_status"`, "go test -race -p 1 ./services/oms/tools/recovery-conformance -skip '^TestRustRecoveryComplete8000History$' -timeout 20m", "-run '^TestRustRecoveryComplete8000History/go$' -timeout 20m", "-run '^TestRustRecoveryComplete8000History/rust$' -timeout 20m", "check_oms_recovery_evidence.py", "--create --source \"$GITHUB_SHA\" --tree \"$(git rev-parse HEAD^{tree})\" --run \"$GITHUB_RUN_ID\" --attempt \"$GITHUB_RUN_ATTEMPT\""},
		"Remove only owned recovery dependencies":              {"for container in oms-recovery-pg oms-recovery-default-pg oms-recovery-constrained-pg oms-recovery-nats", "docker inspect --format", "kanz.fixture.run", "kanz.fixture.attempt", `test -z "$remaining"`, "docker rm -f \"$container\""},
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
			if step.Name == "Compile the actual recovery owner and fixture" {
				exact := false
				for _, line := range strings.Split(step.Run, "\n") {
					if strings.TrimSpace(line) == marker {
						exact = true
					}
				}
				if !exact {
					return fmt.Errorf("missing exact compile command %s", marker)
				}
			}
			if !strings.Contains(step.Run, marker) {
				return fmt.Errorf("recovery step %s missing %s", step.Name, marker)
			}
		}
		if step.Name == "Qualify actual recovery and complete large histories" {
			invocations := 0
			for _, line := range strings.Split(step.Run, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "go test ") {
					invocations++
					if !strings.HasSuffix(strings.TrimSpace(line), "|| qualification_status=1") {
						return fmt.Errorf("recovery invocation exit ignored")
					}
				}
			}
			if invocations != 3 || strings.Count(step.Run, "|| qualification_status=1") != 4 {
				return fmt.Errorf("recovery invocation or checker exit missing")
			}
			if step.Env["KANZ_OMS_RECOVERY_BIN"] != "${{ github.workspace }}/kanz-rs/target/release/examples/recovery-coordinator-fixture" || !strings.Contains(step.Env["TEST_POSTGRES_CONSTRAINED_LOCK_URL"], "@127.0.0.1:55434/") || !strings.Contains(step.Env["TEST_POSTGRES_URL"], "@127.0.0.1:55432/") || !strings.Contains(step.Env["TEST_POSTGRES_DEFAULT_LOCK_URL"], "@127.0.0.1:55433/") || step.Env["KANZ_OMS_RECOVERY_VERSION"] != "${{ github.sha }}" {
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
	raw = []byte(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if e = checkRecoveryWorkflow(raw); e != nil {
		t.Fatal(e)
	}
	for _, marker := range []string{"          cargo clippy --locked -p kanz-oms-recovery --all-targets -- -D warnings\n", "          cargo build --release --locked -p kanz-oms-recovery --example recovery-coordinator-fixture\n", "--features qualification-diagnostics -- -D warnings", `docker exec -e PGPASSWORD=fixture -e PGCONNECT_TIMEOUT=2 "$container" psql -X -w -h 127.0.0.1 -U fixture -d fixture -v ON_ERROR_STOP=1`, "PGCONNECT_TIMEOUT=2", "PGOPTIONS=-c statement_timeout=2000", "-Atqc 'SELECT 1'", "-h 127.0.0.1 -U fixture -d fixture", "cargo build --release --locked", "target/release/examples/recovery-coordinator-fixture", "--name oms-recovery-constrained-pg", "max_locks_per_transaction=10 -c", `sha256sum "$KANZ_OMS_RECOVERY_BIN"`, `exit "$qualification_status"`, "|| qualification_status=1", "check_oms_recovery_evidence.py", "  recovery-coordinator:", "--name oms-recovery-default-pg", "max_locks_per_transaction=1024", "-run '^TestRustRecoveryComplete8000History/go$'", "-run '^TestRustRecoveryComplete8000History/rust$'", "--attempt \"$GITHUB_RUN_ATTEMPT\"", "NOSUPERUSER NOBYPASSRLS", "kanz.fixture.run=$GITHUB_RUN_ID", "kanz.fixture.attempt=$GITHUB_RUN_ATTEMPT", `test -z "$remaining"`} {
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

func checkRecoveryRuntimeProfile(cargo, store, recovery, fixture string) error {
	if !strings.Contains(cargo, "[profile.release]\noverflow-checks = true") || strings.Contains(cargo, "panic =") || strings.Contains(cargo, "[profile.release.") {
		return fmt.Errorf("unreviewed recovery release arithmetic or panic profile")
	}
	for source, markers := range map[string][]string{
		store:    {`database_error(error.code().map(|code| code.code()))`, `Some("53200") => Error::DatabaseMemory`, `_ => Error::Database`},
		recovery: {`E::DatabaseMemory => Self::DatabaseMemory`},
		fixture:  {`option_env!("KANZ_BUILD_VERSION")`, `version.len() != 40`, `cfg!(debug_assertions)`, `"build_profile"`},
	} {
		for _, marker := range markers {
			if !strings.Contains(source, marker) {
				return fmt.Errorf("runtime classification/profile source missing")
			}
		}
	}
	return nil
}

func TestRustRecoveryRuntimeProfileAndCapacityCause(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	read := func(path string) string {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(raw), "\r\n", "\n")
	}
	cargo := read("Cargo.toml")
	store := read("crates/oms-store/src/lib.rs")
	recovery := read("crates/oms-recovery/src/lib.rs")
	fixture := read("crates/oms-recovery/examples/recovery-coordinator-fixture.rs")
	if err := checkRecoveryRuntimeProfile(cargo, store, recovery, fixture); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"overflow-checks = true", `Some("53200")`, `E::DatabaseMemory => Self::DatabaseMemory`, `cfg!(debug_assertions)`, `version.len() != 40`} {
		t.Run(marker, func(t *testing.T) {
			change := func(value string) string { return strings.ReplaceAll(value, marker, "removed") }
			if checkRecoveryRuntimeProfile(change(cargo), change(store), change(recovery), change(fixture)) == nil {
				t.Fatal("runtime guard mutation accepted")
			}
		})
	}
}

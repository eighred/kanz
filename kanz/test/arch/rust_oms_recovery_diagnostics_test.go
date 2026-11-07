package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func diagnosticBoundary(files map[string]string) bool {
	required := map[string][]string{
		"kanz-rs/crates/oms-store/Cargo.toml":                                  {"default = []", "qualification-diagnostics = []"},
		"kanz-rs/crates/oms-recovery/Cargo.toml":                               {"default = []", "qualification-diagnostics = [\"kanz-oms-store/qualification-diagnostics\"]"},
		"kanz-rs/crates/oms-store/src/diagnostics.rs":                          {"pub const STAGES: usize = 14;", "pub counts: [u64; STAGES]", "pub nanoseconds: [u64; STAGES]", "saturating_add(1)", "saturating_add(elapsed)", "#[cfg(feature = \"qualification-diagnostics\")]", "CURRENT", "Snapshot::default()"},
		"kanz/services/oms/tools/recovery-conformance/worker_test.go":          {"const stageCount = 14", "profile=completed-commands-only excludes_unobserved_current=true nested_nonadditive=true", "qualification diagnostics feature required", "w.profile.add(result.Diagnostics)", "TestWorkerProfileBoundedFailureOnly", `case "Observation", "Ack", "Parked", "FeeCorrection":`},
		"kanz-rs/crates/oms-recovery/examples/recovery-coordinator-fixture.rs": {"cfg!(feature=\"qualification-diagnostics\")", "diagnostics::capture", "Stage::Total"},
	}
	for path, markers := range required {
		for _, marker := range markers {
			if !strings.Contains(files[path], marker) {
				return false
			}
		}
	}
	d := files["kanz-rs/crates/oms-store/src/diagnostics.rs"]
	for _, forbidden := range []string{"println!", "eprintln!", "Vec<", "String", "std::fs", "std::env"} {
		if strings.Contains(d, forbidden) {
			return false
		}
	}
	w := files["kanz/services/oms/tools/recovery-conformance/worker_test.go"]
	for _, line := range strings.Split(w, "\n") {
		if strings.Contains(line, "diagnostic(request,") && !strings.Contains(line, "Fatalf(") && !strings.Contains(line, "diagnostic :=") && !strings.HasPrefix(line, "func ") {
			return false
		}
	}
	return true
}

func TestRustRecoveryDiagnosticBoundary(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	paths := []string{"kanz-rs/crates/oms-store/Cargo.toml", "kanz-rs/crates/oms-recovery/Cargo.toml", "kanz-rs/crates/oms-store/src/diagnostics.rs", "kanz/services/oms/tools/recovery-conformance/worker_test.go", "kanz-rs/crates/oms-recovery/examples/recovery-coordinator-fixture.rs"}
	files := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(raw)
	}
	if !diagnosticBoundary(files) {
		t.Fatal("diagnostic boundary changed")
	}
	for _, mutation := range []struct{ path, old, replacement string }{
		{paths[0], "default = []", "default = [\"qualification-diagnostics\"]"},
		{paths[2], "pub const STAGES: usize = 14;", "pub const STAGES: usize = 15;"},
		{paths[2], "saturating_add(elapsed)", "wrapping_add(elapsed)"},
		{paths[3], "excludes_unobserved_current=true", "current=true"},
		{paths[3], "nested_nonadditive=true", "additive=true"},
		{paths[3], `case "Observation", "Ack", "Parked", "FeeCorrection":`, `case "Observation", "Ack", "Parked":`},
		{paths[3], "w.t.Fatalf(\"Rust fixture request failed: %s\", w.diagnostic(request,", "w.t.Logf(\"Rust fixture request failed: %s\", w.diagnostic(request,"},
	} {
		original := files[mutation.path]
		changed := strings.ReplaceAll(original, mutation.old, mutation.replacement)
		if changed == original {
			t.Fatal("mutation did not apply")
		}
		files[mutation.path] = changed
		if diagnosticBoundary(files) {
			t.Fatal("diagnostic mutation accepted")
		}
		files[mutation.path] = original
	}
}

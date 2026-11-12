package arch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const rustTermPath = "crates/term_structure"

var rustTermSources = []string{"src/lib.rs", "src/calibration.rs", "tests/numerical.rs", "tests/go_conformance.rs", "tests/calibration.rs", "tests/support/mod.rs"}
var rustTermArtifacts = []string{"reference/reference.py", "tests/reference.json", "reference/mixed_reference.py", "tests/mixed_reference.json"}

const rustTermCargo = `[package]
name = "kanz-term-structure"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
libm = "=0.2.16"

[dev-dependencies]
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"`

func checkRustTermStructure(root string) error {
	dir := filepath.Join(root, rustTermPath)
	manifest, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return err
	}
	if rustMigrationCompact(string(manifest)) != rustMigrationCompact(rustTermCargo) {
		return fmt.Errorf("unreviewed term-structure recipe")
	}
	allowed := map[string]bool{}
	found := map[string]bool{}
	for _, name := range rustTermSources {
		allowed[name] = true
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 || d.Name() == ".cargo" {
			return fmt.Errorf("term structure source/config indirection")
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(rel, ".proto") || (strings.HasSuffix(rel, ".toml") && rel != "Cargo.toml") {
			return fmt.Errorf("parallel term-structure configuration/contract")
		}
		if strings.HasSuffix(rel, ".rs") {
			if !allowed[rel] {
				return fmt.Errorf("unreviewed term-structure source %s", rel)
			}
			found[rel] = true
		}
		return nil
	}); err != nil {
		return err
	}
	for name := range allowed {
		if !found[name] {
			return fmt.Errorf("missing term-structure source %s", name)
		}
	}
	for _, name := range rustTermArtifacts {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular numerical reference")
		}
	}
	var code, mainCode string
	for _, name := range rustTermSources {
		if !strings.HasPrefix(name, "src/") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		clean, err := rustMigrationWithoutComments(string(raw))
		if err != nil {
			return err
		}
		code += "\n" + clean
		if name == "src/lib.rs" {
			mainCode = clean
		}
	}
	if regexp.MustCompile(`\b(?:unsafe|extern|File|Command|TcpStream|tokio|reqwest|tonic|nats)\b|std::(?:net|fs|process|io|env|thread)|(?:print|println|eprint|eprintln|dbg)\s*!|fn\s+main\s*\(|include(?:_str|_bytes)?\s*!|(?:struct|enum|type)\s+(?:Money|Decimal|Order|Position|BondTerms)\b`).MatchString(code) {
		return fmt.Errorf("term structure acquired effects or canonical-record ownership")
	}
	compact := rustMigrationCompact(mainCode)
	for _, marker := range []string{`#![forbid(unsafe_code)]`, `pubconstMAX_PILLARS:usize=256;`, `pubconstMAX_COUPONS:usize=4096;`, `returnErr(Error::CapacityExceeded)`, `returnErr(Error::InvalidInput)`, `returnErr(Error::InvalidQuery)`, `returnErr(Error::NumericalFailure)`, `libm::log1p`, `libm::expm1`, `fnzero_with_bump(`} {
		if !strings.Contains(compact, marker) {
			return fmt.Errorf("term-structure invariant missing: %s", marker)
		}
	}
	compact = rustMigrationCompact(code)
	for _, marker := range []string{`constMAX_EVALUATIONS:usize=256;`, `pubfncalibrate(`, `pubfnquote_rate(`, `ifbudget==0`, `128.0*f64::EPSILON*want.abs()`, `residual.abs()<=64.0*f64::EPSILON*q.value.abs()`, `hi-lo<=64.0*f64::EPSILON*lo.abs().max(hi.abs())`} {
		if !strings.Contains(compact, marker) {
			return fmt.Errorf("mixed calibration invariant missing: %s", marker)
		}
	}
	return nil
}

func TestRustTermStructureBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if err := checkRustTermStructure(root); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, file, from, to string }{
		{"transport", "Cargo.toml", `libm = "=0.2.16"`, "libm = \"=0.2.16\"\ntokio = \"1\""},
		{"record", "src/lib.rs", "", "\npub struct Money { pub amount: f64 }"},
		{"runtime", "src/main.rs", "", "fn main() {}"},
		{"filesystem", "src/lib.rs", "", "\npub fn read() { std::fs::read(\"file\").unwrap(); }"},
		{"capacity", "src/lib.rs", "return Err(Error::CapacityExceeded)", "return Err(Error::InvalidInput)"},
		{"finite-refusal", "src/lib.rs", "return Err(Error::NumericalFailure)", "return Ok(0.0)"},
		{"calibration-effects", "src/calibration.rs", "", "\npub fn read() { std::fs::read(\"file\").unwrap(); }"},
		{"calibration-residual", "src/calibration.rs", "64.0 * f64::EPSILON * q.value.abs()", "1.0"},
		{"calibration-budget", "src/calibration.rs", "MAX_EVALUATIONS: usize = 256", "MAX_EVALUATIONS: usize = 1000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			for _, name := range append(append([]string{"Cargo.toml"}, rustTermSources...), rustTermArtifacts...) {
				raw, err := os.ReadFile(filepath.Join(root, rustTermPath, name))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dst, rustTermPath, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dst, rustTermPath, tc.file)
			raw, _ := os.ReadFile(path)
			changed := string(raw) + tc.to
			if tc.from != "" {
				changed = strings.ReplaceAll(string(raw), tc.from, tc.to)
				if changed == string(raw) {
					t.Fatal("mutation missed")
				}
			}
			if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			if checkRustTermStructure(dst) == nil {
				t.Fatal("forbidden mutation accepted")
			}
		})
	}
}

func TestRustBondShadowSharesTermStructure(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs", rustBondShadowPath, "src/lib.rs"))
	if err != nil {
		t.Fatal(err)
	}
	check := func(s string) bool {
		return strings.Contains(s, "kanz_term_structure::Curve::new_zero") && strings.Contains(s, ".zero_with_bump(") && !regexp.MustCompile(`fn\s+zero\s*\(`).MatchString(s)
	}
	if !check(string(raw)) {
		t.Fatal("bond shadow must use the one shared curve kernel")
	}
	for _, s := range []string{strings.ReplaceAll(string(raw), ".zero_with_bump(", ".alternate("), string(raw) + "\nfn zero() {}"} {
		if check(s) {
			t.Fatal("duplicate/bypassed interpolation accepted")
		}
	}
}

func TestRustTermStructureRequiresFreshIndependentGate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/rust-bond-shadow.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	start := strings.Index(s, "  term-structure:")
	if start < 0 {
		t.Fatal("term structure job missing")
	}
	s = s[start:]
	markers := []string{`go run ./services/risk-engine/tools/curve_reference -source "$GITHUB_SHA"`, `python3 -B crates/term_structure/reference/reference.py --check`, `cargo test --locked -p kanz-term-structure --test numerical`, `missing_or_malformed_capture_refuses -- --exact`, `fresh_go_oracle_is_required -- --ignored --exact --nocapture`, `KANZ_CURVE_SOURCE: ${{ github.sha }}`, `KANZ_CURVE_CORPUS:`, `cargo clippy --locked -p kanz-term-structure --all-targets -- -D warnings`, `term-structure-evidence`}
	markers = append(markers, `go run ./services/risk-engine/tools/curve_reference -mixed -source "$GITHUB_SHA"`, `python3 -B crates/term_structure/reference/mixed_reference.py --check`, `cargo test --locked -p kanz-term-structure --lib`, `cargo test --locked -p kanz-term-structure --test calibration`, `fresh_mixed_go_oracle_is_required -- --ignored --exact --nocapture`, `KANZ_MIXED_SOURCE: ${{ github.sha }}`, `KANZ_MIXED_CORPUS:`)
	check := func(s string) bool {
		for _, m := range markers {
			if !strings.Contains(s, m) {
				return false
			}
		}
		return true
	}
	if !check(s) {
		t.Fatal("independent/fresh curve evidence optional or absent")
	}
	for _, m := range markers {
		if check(strings.ReplaceAll(s, m, "removed")) {
			t.Fatal("missing evidence command accepted")
		}
	}
}

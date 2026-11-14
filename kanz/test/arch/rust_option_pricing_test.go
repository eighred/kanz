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

// #1534 adds one pure analytic library, not a process or financial writer.
const rustOptionPricingPath = "crates/option_pricing"

var rustOptionPricingSources = []string{"src/lib.rs", "tests/numerical.rs", "tests/go_conformance.rs"}

const rustOptionPricingCargo = `[package]
name = "kanz-option-pricing"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
libm = "=0.2.16"

[dev-dependencies]
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"`

func checkRustOptionPricing(root string) error {
	dir := filepath.Join(root, rustOptionPricingPath)
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return err
	}
	if rustMigrationCompact(string(raw)) != rustMigrationCompact(rustOptionPricingCargo) {
		return fmt.Errorf("unreviewed option analytic recipe")
	}
	allowed := map[string]bool{}
	for _, name := range rustOptionPricingSources {
		allowed[name] = true
	}
	found := map[string]bool{}
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
			return fmt.Errorf("option source/config indirection")
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(rel, ".proto") || (strings.HasSuffix(rel, ".toml") && rel != "Cargo.toml") {
			return fmt.Errorf("parallel option contract or configuration")
		}
		if strings.HasSuffix(rel, ".rs") {
			if !allowed[rel] {
				return fmt.Errorf("unreviewed option runtime/source %s", rel)
			}
			found[rel] = true
		}
		return nil
	}); err != nil {
		return err
	}
	for name := range allowed {
		if !found[name] {
			return fmt.Errorf("missing option source %s", name)
		}
	}
	lib, err := os.ReadFile(filepath.Join(dir, "src/lib.rs"))
	if err != nil {
		return err
	}
	code, err := rustMigrationWithoutComments(string(lib))
	if err != nil {
		return err
	}
	if regexp.MustCompile(`\b(?:unsafe|extern|File|Command|TcpStream|tokio|reqwest|tonic|nats)\b|std::(?:net|fs|process|io|env)|(?:print|println|eprint|eprintln|dbg)\s*!|fn\s+main\s*\(|include(?:_str|_bytes)?\s*!|struct\s+(?:Decimal|Money|OrderState|Fill)\b`).MatchString(code) {
		return fmt.Errorf("option library acquired effects or canonical record ownership")
	}
	compact := rustMigrationCompact(code)
	for _, marker := range []string{`#![forbid(unsafe_code)]`, `i.exercise!=Exercise::European`, `returnErr(Error::UnsupportedModel)`, `returnErr(Error::NonFiniteInput)`, `returnErr(Error::NonFiniteOutput)`, `.all(f64::is_finite)`, `libm::erfc`, `pubfnevaluate(i:Input)->Result<Output,Error>`} {
		if !strings.Contains(compact, marker) {
			return fmt.Errorf("option bounds/ownership missing %s", marker)
		}
	}
	return nil
}

func TestRustOptionPricingBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if err := checkRustOptionPricing(root); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, file, from, to string }{
		{"transport", "Cargo.toml", `libm = "=0.2.16"`, "libm = \"=0.2.16\"\ntokio = \"1\""},
		{"record", "src/lib.rs", "", "\npub struct Money { pub amount: f64 }\n"},
		{"runtime", "src/main.rs", "", "fn main() {}"},
		{"filesystem", "src/lib.rs", "", "\npub fn read() { std::fs::read(\"file\").unwrap(); }"},
		{"stdout", "src/lib.rs", "", "\npub fn print_price() { println!(\"value\"); }"},
		{"unsupported", "src/lib.rs", "i.exercise != Exercise::European", "false"},
		{"finite-output", "src/lib.rs", "return Err(Error::NonFiniteOutput)", "return Ok(result)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			for _, file := range append([]string{"Cargo.toml"}, rustOptionPricingSources...) {
				body, err := os.ReadFile(filepath.Join(root, rustOptionPricingPath, file))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dst, rustOptionPricingPath, file)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dst, rustOptionPricingPath, tc.file)
			body, _ := os.ReadFile(path)
			changed := string(body) + tc.to
			if tc.from != "" {
				changed = strings.ReplaceAll(string(body), tc.from, tc.to)
				if changed == string(body) {
					t.Fatal("mutation missed")
				}
			}
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := checkRustOptionPricing(dst); err == nil {
				t.Fatal("forbidden option mutation accepted")
			}
		})
	}
}

func TestRustOptionPricingRequiresFreshGoGate(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/rust-bond-shadow.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	start := strings.Index(s, "  option-pricing:")
	if start < 0 {
		t.Fatal("independent option qualification job missing")
	}
	s = s[start:]
	markers := []string{"go run ./services/risk-engine/tools/option_pricing_reference -source \"$GITHUB_SHA\"", "cargo test --locked -p kanz-option-pricing --test numerical", "missing_or_malformed_capture_refuses -- --exact", "fresh_go_oracle_is_required -- --ignored --exact --nocapture", "KANZ_OPTION_PRICING_CORPUS:", "KANZ_OPTION_PRICING_SOURCE: ${{ github.sha }}", "cargo clippy --locked -p kanz-option-pricing --all-targets -- -D warnings", "option-pricing-evidence"}
	check := func(s string) bool {
		for _, marker := range markers {
			if !strings.Contains(s, marker) {
				return false
			}
		}
		return true
	}
	if !check(s) {
		t.Fatal("fresh Go option oracle or independent numerical evidence is optional/missing")
	}
	for _, marker := range markers {
		if check(strings.ReplaceAll(s, marker, "removed")) {
			t.Fatal("missing mandatory option evidence accepted")
		}
	}
}

package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const rustLedgerPath = "crates/ledger_replay"
const rustLedgerCargo = `[package]
name = "kanz-ledger-replay"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
kanz-order-core = { path = "../order-core" }
num-rational = "=0.4.2"
num-traits = "=0.2.19"

[dev-dependencies]
num-bigint = "=0.4.6"
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"
`

var rustLedgerSources = []string{"src/lib.rs", "tests/go_conformance.rs"}

// Conservative source/recipe guard, not Rust AST, data-flow or dependency proof.
func checkRustLedgerReplay(root string) error {
	dir := filepath.Join(root, rustLedgerPath)
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return err
	}
	if rustMigrationCompact(string(raw)) != rustMigrationCompact(rustLedgerCargo) {
		return fmt.Errorf("unreviewed ledger candidate dependencies")
	}
	allowed := map[string]bool{}
	found := map[string]bool{}
	for _, p := range rustLedgerSources {
		allowed[p] = true
	}
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("ledger symlink")
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if rel == "Cargo.toml" || rel == "README.md" {
			return nil
		}
		if !allowed[rel] {
			return fmt.Errorf("unreviewed ledger source/config %s", rel)
		}
		found[rel] = true
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		code, e := rustMigrationWithoutComments(string(raw))
		if e != nil {
			return e
		}
		if strings.HasPrefix(rel, "src/") && regexp.MustCompile(`\b(?:f32|f64|unsafe|TcpStream|Command|File)\b|std::(?:net|fs|process|env|thread|io)|\bfn\s+main\s*\(|\b(?:struct|type)\s+(?:Decimal|Money|Lot)\b|include\w*\s*!`).MatchString(strings.ReplaceAll(code, "forbid(unsafe_code)", "")) {
			return fmt.Errorf("ledger candidate acquired IO or parallel financial arithmetic")
		}
		return nil
	})
	if err != nil {
		return err
	}
	for p := range allowed {
		if !found[p] {
			return fmt.Errorf("missing ledger source %s", p)
		}
	}
	return nil
}

func TestRustLedgerReplayBoundaryRejectsMutations(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for _, mutation := range []struct{ path, body string }{
		{"src/lib.rs", "\nfn read(){std::fs::read(\"journal\");}"},
		{"src/lib.rs", "\nfn amount()->f64{1.0}"},
		{"src/lib.rs", "\npub struct Lot {}"},
		{"src/lib.rs", "\nfn config(){std::env::var(\"DSN\");}"},
		{"src/owner.rs", "fn main(){}"},
		{"build.rs", "fn main(){}"},
		{"Cargo.toml", "\nredis=\"*\""},
	} {
		t.Run(mutation.path+mutation.body, func(t *testing.T) {
			dst := t.TempDir()
			for _, rel := range append([]string{"Cargo.toml"}, rustLedgerSources...) {
				raw, e := os.ReadFile(filepath.Join(root, rustLedgerPath, rel))
				if e != nil {
					t.Fatal(e)
				}
				path := filepath.Join(dst, rustLedgerPath, rel)
				if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(path, raw, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if e := checkRustLedgerReplay(dst); e != nil {
				t.Fatalf("baseline: %v", e)
			}
			path := filepath.Join(dst, rustLedgerPath, mutation.path)
			f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				t.Fatal(e)
			}
			_, e = f.WriteString(mutation.body)
			closeErr := f.Close()
			if e != nil || closeErr != nil {
				t.Fatalf("mutation write: %v %v", e, closeErr)
			}
			if checkRustLedgerReplay(dst) == nil {
				t.Fatal("forbidden mutation accepted")
			}
		})
	}
}

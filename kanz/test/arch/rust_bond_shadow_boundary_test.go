package arch

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #1413 permits this bounded, local analytic shadow only. These lexical and
// exact-recipe checks are not a Rust AST or transitive dependency audit. Runtime
// numerical/oracle tests and the Go authority boundary remain separate proofs.
const rustBondShadowPath = "services/bond-risk-shadow"

var rustBondShadowSources = []string{"build.rs", "src/main.rs", "src/lib.rs", "src/decimal.rs"}

const rustBondShadowCargo = `[package]
name = "kanz-bond-risk-shadow"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
kanz-decimal-compat = { path = "../.." }
prost = "=0.14.4"
prost-types = "=0.14.4"
num-bigint = "=0.4.6"
num-rational = "=0.4.2"
num-traits = "=0.2.19"

[build-dependencies]
prost-build = "=0.14.1"
protoc-bin-vendored = "=3.3.0"`

const rustBondShadowBuild = `fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = "../../../kanz-schemas/proto";
    for file in [
        "quant/v1/bond_risk.proto",
        "reference/v1/fixed_income.proto",
        "reference/v1/conventions.proto",
        "domain/v1/calibration.proto",
    ] {
        println!("cargo:rerun-if-changed={root}/{file}");
    }
    println!("cargo:rerun-if-env-changed=KANZ_BUILD_VERSION");
    let mut config = prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.extern_path(".common.v1", "::kanz_decimal_compat::common::v1");
    config.compile_protos(&[format!("{root}/quant/v1/bond_risk.proto")], &[root])?;
    Ok(())
}`

const rustBondShadowMain = `#![forbid(unsafe_code)]
use kanz_bond_risk_shadow::{evaluate, proto::quant::v1::BondRiskRequest};
use prost::Message;
use std::io::{Read, Write};
const LIMIT: usize = 1 << 20;
fn run() -> Result<(), Box<dyn std::error::Error>> {
    let version = option_env!("KANZ_BUILD_VERSION")
        .filter(|v| !v.trim().is_empty())
        .ok_or("unstamped binary")?;
    let mut bytes = Vec::new();
    std::io::stdin()
        .take((LIMIT + 1) as u64)
        .read_to_end(&mut bytes)?;
    if bytes.is_empty() || bytes.len() > LIMIT {
        return Err("invalid input size".into());
    }
    let request = BondRiskRequest::decode(bytes.as_slice())?;
    let output = evaluate(&request, version).encode_to_vec();
    if output.len() > LIMIT {
        return Err("invalid output size".into());
    }
    std::io::stdout().lock().write_all(&output)?;
    Ok(())
}

fn main() {
    if run().is_err() {
        eprintln!("bond shadow worker refused request");
        std::process::exit(1);
    }
}`

func checkRustBondShadowBoundary(root string) error {
	root = filepath.Join(root, filepath.FromSlash(rustBondShadowPath))
	for file, expected := range map[string]string{"Cargo.toml": rustBondShadowCargo, "build.rs": rustBondShadowBuild, "src/main.rs": rustBondShadowMain} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			return err
		}
		code, err := rustMigrationWithoutComments(string(data))
		if err != nil {
			return err
		}
		want, err := rustMigrationWithoutComments(expected)
		if err != nil {
			return err
		}
		if rustMigrationCompact(code) != rustMigrationCompact(want) {
			return fmt.Errorf("unreviewed bond shadow %s", file)
		}
	}
	found := map[string]bool{}
	allowed := map[string]bool{}
	for _, path := range rustBondShadowSources {
		allowed[path] = true
	}
	forbidden := regexp.MustCompile(`\bunsafe\b|\b(?:net|fs|thread|Command|TcpStream|UdpSocket|TcpListener|OpenOptions|File)\b|\b(?:struct|enum|union|type)\s+(?:r#)?(?:Decimal|Money|BondTerms|BondRiskRequest|BondRiskResult|Order|Position)\b|SubmitOrder|CancelOrder|ExecuteRequest|async_nats|reqwest|tonic|sqlx|rusqlite|\.\s*(?:publish|connect|send_order|put|insert)\s*\(`)
	quoted := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	include := regexp.MustCompile(`\binclude(?:_str|_bytes)?\s*!`)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("bond shadow symlink %s", rel)
		}
		if d.IsDir() {
			if rel == "target" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(rel), ".proto") || (strings.EqualFold(d.Name(), "Cargo.toml") && rel != "Cargo.toml") || strings.EqualFold(d.Name(), "config") || strings.EqualFold(d.Name(), "config.toml") {
			return fmt.Errorf("unreviewed bond shadow contract/config %s", rel)
		}
		if !strings.EqualFold(filepath.Ext(rel), ".rs") {
			return nil
		}
		if !allowed[rel] {
			return fmt.Errorf("unreviewed bond shadow source %s", rel)
		}
		found[rel] = true
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		code, err := rustMigrationWithoutComments(string(body))
		if err != nil {
			return err
		}
		lexical := quoted.ReplaceAllString(code, `""`)
		if regexp.MustCompile(`\b(?:struct|enum|union)\s+[A-Za-z_]`).MatchString(lexical) || forbidden.MatchString(lexical) || regexp.MustCompile(`#\s*\[[^\]]*\bpath\b[^\]]*\]`).MatchString(code) {
			return fmt.Errorf("bond shadow authority/alternate model/source token in %s", rel)
		}
		if (rel == "src/lib.rs" || rel == "src/decimal.rs") && regexp.MustCompile(`\b(?:io|process|env)\s*::|\b(?:print|println|eprint|eprintln)!`).MatchString(lexical) {
			return fmt.Errorf("bond kernel must remain pure: %s", rel)
		}
		count := len(include.FindAllString(code, -1))
		if rel == "src/lib.rs" {
			if count != 3 {
				return fmt.Errorf("bond kernel generated contract count changed")
			}
			for _, token := range []string{`pub use kanz_decimal_compat::common;`, `include!(concat!(env!("OUT_DIR"), "/reference.v1.rs"));`, `include!(concat!(env!("OUT_DIR"), "/domain.v1.rs"));`, `include!(concat!(env!("OUT_DIR"), "/quant.v1.rs"));`, `r.payments.len() > 1201`, `r.curve_points.len() > 256`, `r.capture_digest.len() != 32`} {
				if !strings.Contains(rustMigrationCompact(code), rustMigrationCompact(token)) {
					return fmt.Errorf("bond analytic contract/bound removed")
				}
			}
		} else if count != 0 {
			return fmt.Errorf("unexpected bond source include %s", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for file := range allowed {
		if !found[file] {
			return fmt.Errorf("missing bond shadow source %s", file)
		}
	}
	return nil
}

func TestRustBondShadowBoundary(t *testing.T) {
	if err := checkRustBondShadowBoundary(filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")); err != nil {
		t.Fatal(err)
	}
}
func TestRustBondShadowBoundaryMutations(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for _, tc := range []struct{ name, path, old, replacement string }{
		{"dependency", "Cargo.toml", "prost =", "reqwest = \"1\"\nprost ="},
		{"sidecar", "src/authority.rs", "", "fn start() {}"},
		{"schema", "proto/money.proto", "", "message Money {}"},
		{"alternate-build", "build.rs", "quant/v1/bond_risk.proto", "market/v1/order_book.proto"},
		{"unchecked-output", "src/main.rs", "if output.len() > LIMIT", "if false"},
		{"unbounded-input", "src/main.rs", ".take((LIMIT + 1) as u64)", ""},
		{"unstamped", "src/main.rs", "option_env!(\"KANZ_BUILD_VERSION\")", "Some(\"unversioned\")"},
		{"network", "src/lib.rs", "", "\nfn connect() { std::net::TcpStream::connect(\"localhost\"); }"},
		{"storage", "src/lib.rs", "", "\nfn store() { std::fs::write(\"file\", b\"data\"); }"},
		{"order", "src/lib.rs", "", "\nfn action() { SubmitOrder(); }"},
		{"parallel-bond-model", "src/lib.rs", "", "\nstruct Bond { face: f64 }"},
		{"copied-money", "src/decimal.rs", "", "\nstruct Money { value: f64 }"},
		{"unbounded-payments", "src/lib.rs", "r.payments.len() > 1201", "false"},
		{"alternate-include", "src/decimal.rs", "", "\ninclude!(\"other.rs\");"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			for _, file := range append([]string{"Cargo.toml"}, rustBondShadowSources...) {
				body, err := os.ReadFile(filepath.Join(root, rustBondShadowPath, filepath.FromSlash(file)))
				if err != nil {
					t.Fatal(err)
				}
				dest := filepath.Join(dst, rustBondShadowPath, filepath.FromSlash(file))
				if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dest, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := checkRustBondShadowBoundary(dst); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			path := filepath.Join(dst, rustBondShadowPath, filepath.FromSlash(tc.path))
			body, _ := os.ReadFile(path)
			text := string(body)
			if tc.old == "" {
				text += tc.replacement
			} else {
				changed := strings.Replace(text, tc.old, tc.replacement, 1)
				if changed == text {
					t.Fatal("mutation missed")
				}
				text = changed
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkRustBondShadowBoundary(dst); err == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

// This is a bounded syntax/recipe guard over named owners, not arbitrary Go
// data-flow proof. Parsing drops comments so an inert example cannot satisfy it.
var bondCloneOwners = []struct{ path, function, call string }{
	{"internal/risk/termsource/provider.go", "toBondSpec", "CanonicalTerms: pricing.CloneBoundedBondTerms(b)"},
	{"services/risk-engine/internal/replay/providers.go", "BondTerms", "a.Value.CanonicalTerms = pricing.CloneBoundedBondTerms(a.Value.CanonicalTerms)"},
	{"internal/risk/compute/fi.go", "positionBondRisk", "canonical = pricing.CloneBoundedBondTerms(spec.CanonicalTerms)"},
	{"services/risk-engine/internal/bondshadow/worker.go", "Submit", "c.Terms = pricing.CloneBoundedBondTerms(c.Terms)"},
	{"internal/risk/pricing/bond_projection.go", "ProjectBondRisk", "terms = CloneBoundedBondTerms(terms)"},
}

func bondCloneNode(node ast.Node) string {
	var out bytes.Buffer
	if err := format.Node(&out, token.NewFileSet(), node); err != nil {
		return ""
	}
	return rustMigrationCompact(out.String())
}

func checkBondCloneBoundary(root string) error {
	const helper = "internal/risk/pricing/bond_terms.go"
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(helper)), nil, 0)
	if err != nil {
		return err
	}
	bound, body := false, ""
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "CloneBoundedBondTerms" {
			body = bondCloneNode(fn.Body)
		}
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			for _, spec := range gen.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok && len(value.Names) == 1 && value.Names[0].Name == "MaxBondTermsBytes" && len(value.Values) == 1 {
					bound = bondCloneNode(value.Values[0]) == "1<<20"
				}
			}
		}
	}
	want := `{ if terms == nil || proto.Size(terms) > MaxBondTermsBytes { return nil }; return proto.Clone(terms).(*referencepb.BondTerms) }`
	// Parse the expected body too: format.Node normalizes semicolon spelling.
	expected, err := parser.ParseFile(token.NewFileSet(), "expected.go", "package expected; func clone() "+want, 0)
	if err != nil {
		return err
	}
	if !bound || body != bondCloneNode(expected.Decls[0].(*ast.FuncDecl).Body) {
		return fmt.Errorf("bond metadata clone must check 1MiB size before cloning")
	}
	for _, owner := range bondCloneOwners {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(owner.path)), nil, 0)
		if err != nil {
			return err
		}
		matches := 0
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != owner.function {
				continue
			}
			matches++
			code := bondCloneNode(fn.Body)
			if strings.Count(code, rustMigrationCompact(owner.call)) != 1 || strings.Contains(code, "proto.Clone(") {
				return fmt.Errorf("%s:%s must use bounded canonical metadata clone", owner.path, owner.function)
			}
		}
		if matches != 1 {
			return fmt.Errorf("missing or ambiguous bond clone owner %s:%s", owner.path, owner.function)
		}
	}
	return nil
}

func TestBondCanonicalCloneBoundaries(t *testing.T) {
	if err := checkBondCloneBoundary(moduleRoot(t)); err != nil {
		t.Fatal(err)
	}
}

func TestBondCanonicalCloneBoundaryMutations(t *testing.T) {
	const helper = "internal/risk/pricing/bond_terms.go"
	cases := []struct{ name, path, old, replacement string }{
		{"size-check-removed", helper, "proto.Size(terms) > MaxBondTermsBytes", "false"},
		{"limit-expanded", helper, "1 << 20", "1 << 30"},
		{"clone-before-size", helper, "if terms == nil", "_ = proto.Clone(terms)\n if terms == nil"},
	}
	for _, owner := range bondCloneOwners {
		cases = append(cases, struct{ name, path, old, replacement string }{"bypass-" + owner.function, owner.path, "CloneBoundedBondTerms(", "UnboundedClone("})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			paths := []string{helper}
			for _, owner := range bondCloneOwners {
				paths = append(paths, owner.path)
			}
			for _, path := range paths {
				body, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(path)))
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dst, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := checkBondCloneBoundary(dst); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			path := filepath.Join(dst, filepath.FromSlash(tc.path))
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(body), tc.old, tc.replacement, 1)
			if changed == string(body) {
				t.Fatal("mutation missed")
			}
			if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkBondCloneBoundary(dst); err == nil {
				t.Fatal("unbounded metadata clone accepted")
			}
		})
	}
}

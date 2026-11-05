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

// #1413 starts with a compatibility library, not a second execution authority.
// This deliberately bounded source/layout/declaration guard is NOT a Rust AST,
// call graph, Cargo resolver, or proof about transitive dependency behavior.
// New modules, manifest syntax, dependency features and build inputs require
// explicit review while this transitional boundary remains in force. Rust's
// compiler and Go-oracle conformance tests establish different properties.
func TestRustMigrationRemainsCanonicalCompatibilityLibrary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if err := checkRustMigrationBoundary(root); err != nil {
		t.Fatal(err)
	}
}

var rustMigrationSources = map[string]bool{
	"build.rs": true, "src/lib.rs": true, "tests/go_conformance.rs": true,
}

// Restrict dependency declarations rather than searching for known bad crate
// names. Aliases, path/git overrides, target tables and workspace inheritance
// cannot silently bring transport/storage/runtime dependencies into this slice.
var rustMigrationCargo = map[string]map[string]string{
	"package": {
		"name": `"kanz-decimal-compat"`, "version": `"0.1.0"`, "edition": `"2021"`,
		"rust-version": `"1.90"`, "publish": `false`,
	},
	"dependencies":       {"prost": `"=0.14.1"`},
	"build-dependencies": {"prost-build": `"=0.14.1"`, "protoc-bin-vendored": `"=3.2.0"`},
	"dev-dependencies":   {"serde": `{ version = "=1.0.229", features = ["derive"] }`, "serde_json": `"=1.0.154"`},
	"profile.release":    {"overflow-checks": `true`},
}

// Pin the complete small build recipe: merely finding the canonical path could
// pass when it occurred in a comment or an unused variable beside another input.
const rustMigrationBuild = `fn main() -> Result<(), Box<dyn std::error::Error>> {
    let schema = "../kanz-schemas/proto/common/v1/decimal.proto";
    println!("cargo:rerun-if-changed={schema}");
    let mut config = prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.compile_protos(&[schema], &["../kanz-schemas/proto"])?;
    Ok(())
}`

func rustMigrationCompact(s string) string { return strings.Join(strings.Fields(s), "") }

func checkRustMigrationBoundary(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if err != nil {
		return fmt.Errorf("Rust compatibility manifest missing: %w", err)
	}
	seen := map[string]bool{}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if _, ok := rustMigrationCargo[section]; !ok || seen["["+section+"]"] {
				return fmt.Errorf("unreviewed or duplicate Cargo table %q", line)
			}
			seen["["+section+"]"] = true
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		want, allowed := rustMigrationCargo[section][key]
		entry := section + "." + key
		if !ok || !allowed || seen[entry] || rustMigrationCompact(value) != rustMigrationCompact(want) {
			return fmt.Errorf("unreviewed Cargo declaration %q", entry)
		}
		seen[entry] = true
	}
	for table, entries := range rustMigrationCargo {
		for key := range entries {
			if !seen[table+"."+key] {
				return fmt.Errorf("missing canonical Cargo declaration %s.%s", table, key)
			}
		}
	}
	build, err := os.ReadFile(filepath.Join(root, "build.rs"))
	if err != nil || rustMigrationCompact(string(build)) != rustMigrationCompact(rustMigrationBuild) {
		return fmt.Errorf("Rust build must generate only the canonical common/v1/decimal.proto via the reviewed build recipe")
	}

	// Conservative token checks also inspect comments and strings. They may ask
	// for review of harmless new prose; they do not expand macros or resolve Rust.
	// Range syntax (0..10) is not mistaken for an unsuffixed decimal literal.
	floats := regexp.MustCompile(`\bf(?:32|64)\b|\b[0-9][0-9_]*f(?:32|64)\b|\b[0-9][0-9_]*\.[0-9][0-9_]*|\b[0-9][0-9_]*(?:\.[0-9_]*)?[eE][+-]?[0-9]|\b[0-9][0-9_]*\.(?:[^.a-zA-Z0-9_]|$)`)
	copiedDecimal := regexp.MustCompile(`\b(?:struct|enum|union|type)\s+(?:r#)?Decimal\b`)
	declaration := regexp.MustCompile(`\b(?:struct|enum|union)\s+[A-Za-z_]`)
	runtime := regexp.MustCompile(`\b(?:net|fs|process|thread|os|io|unsafe)\b|\bfn\s+main\s*\(`)
	include := regexp.MustCompile(`\binclude(?:_str|_bytes)?\s*!`)
	const canonicalInclude = `include!(concat!(env!("OUT_DIR"), "/common.v1.rs"));`
	found := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Rust source tree symlink requires review: %s", rel)
		}
		if d.IsDir() {
			if rel == "target" || rel == ".git" {
				return filepath.SkipDir // Generated Cargo output is not authored source.
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(rel), ".proto") {
			return fmt.Errorf("parallel protobuf contract forbidden: %s", rel)
		}
		if strings.EqualFold(d.Name(), "Cargo.toml") && rel != "Cargo.toml" {
			return fmt.Errorf("additional Rust package requires migration review: %s", rel)
		}
		if strings.EqualFold(d.Name(), "config") || strings.EqualFold(d.Name(), "config.toml") {
			return fmt.Errorf("Cargo configuration/patch overrides require migration review: %s", rel)
		}
		if !strings.EqualFold(filepath.Ext(rel), ".rs") {
			return nil
		}
		if !rustMigrationSources[rel] {
			return fmt.Errorf("unreviewed Rust source or runtime entrypoint: %s", rel)
		}
		found[rel] = true
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if floats.Match(body) || copiedDecimal.Match(body) {
			return fmt.Errorf("Rust financial float token or copied Decimal declaration in %s", rel)
		}
		if rel == "src/lib.rs" {
			if runtime.Match(body) || declaration.Match(body) {
				return fmt.Errorf("runtime/storage token or handwritten type declaration in compatibility library")
			}
			if len(include.FindAll(body, -1)) != 1 || !strings.Contains(rustMigrationCompact(string(body)), rustMigrationCompact(canonicalInclude)) {
				return fmt.Errorf("compatibility library must include only generated common.v1.rs")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for path := range rustMigrationSources {
		if !found[path] {
			return fmt.Errorf("required Rust compatibility source missing: %s", path)
		}
	}
	return nil
}

// Mutations run against disposable copies, never the shared checkout. This
// proves the named guard cases, not complete semantic Rust analysis.
func TestRustMigrationBoundaryRejectsMutations(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for _, tc := range []struct{ name, path, appendText string }{
		{"runtime_dependency", "Cargo.toml", "\n[target.'cfg(unix)'.dependencies]\ntokio = \"1\"\n"},
		{"binary_entrypoint", "src/main.rs", "fn main() {}\n"},
		{"runtime_module", "src/runtime.rs", "pub fn start() {}\n"},
		{"parallel_contract", "proto/common.proto", "message Decimal {}\n"},
		{"copied_decimal", "src/lib.rs", "\npub struct Decimal { coefficient: i64, exponent: i32 }\n"},
		{"financial_float", "src/lib.rs", "\npub fn amount() -> f64 { 1.0 }\n"},
		{"unsuffixed_float", "src/lib.rs", "\npub fn amount() { let x = 1e3; }\n"},
		{"alternate_build_input", "build.rs", "\nfn extra() { prost_build::compile_protos(&[\"copied.proto\"], &[\".\"]).unwrap(); }\n"},
		{"storage_in_library", "src/lib.rs", "\npub fn read() { std::fs::read(\"file\").unwrap(); }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			for _, rel := range []string{"Cargo.toml", "build.rs", "src/lib.rs", "tests/go_conformance.rs"} {
				body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dst, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := checkRustMigrationBoundary(dst); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			path := filepath.Join(dst, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := f.WriteString(tc.appendText)
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("mutation: %v %v", writeErr, closeErr)
			}
			if err := checkRustMigrationBoundary(dst); err == nil {
				t.Fatal("guard accepted prohibited migration mutation")
			}
		})
	}
}

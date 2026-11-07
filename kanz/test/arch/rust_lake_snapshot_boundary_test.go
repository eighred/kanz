package arch

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #1413 permits this finite worker without granting financial authority.
// This closed source/declaration guard is not a Rust AST, macro expansion,
// transitive dependency audit or kernel proof. Actual Linux worker and seccomp
// subprocess tests remain mandatory. Go retains identity, files and publication.
const rustLakePath = "services/lake-snapshot"
const rustLakeCargo = `[package]
name = "kanz-lake-snapshot"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
arrow-array = "=59.3.2"
arrow-schema = "=59.3.2"
bytes = "=1.12.1"
datafusion = { version = "=54.1.0", default-features = false }
parquet = { version = "=59.3.2", default-features = false, features = ["arrow"] }
prost = "=0.14.1"
prost-types = "=0.14.1"
sha2 = "=0.10.9"
tokio = { version = "=1.54.1", default-features = false, features = ["rt", "time"] }

[target.'cfg(target_os = "linux")'.dependencies]
libc = "=0.2.191"

[build-dependencies]
prost-build = "=0.14.1"
protoc-bin-vendored = "=3.2.0"`

// Hash reviewed startup, canonical generation and process-test seams after the
// shared Rust comment masker and whitespace compaction. Changes require review;
// comments cannot confer an exemption or hide executable tokens.
var rustLakePinned = map[string]string{
	"build.rs":                  "4b23d1a679817025a1f215db5e5ee3ed89c93bddf5c4664774bee3602ff68a94",
	"examples/sandbox-probe.rs": "14cc4f882990c62aa6a374ad4b2c2332a4081bfdb846b84117ce10bfa205461c",
	"src/main.rs":               "e0a32604d98cf6f72eaf21fbcbe836a59a9a2e389816dfee7bb0c1e23a3f3c12",
	"src/proto.rs":              "002b6f86d61dedc8a1ac2b7d2cbe9ed64fdec7cd1078c03c4f8c27cf09983c87",
	"src/sandbox.rs":            "6b7a3ba9feea625754b58c4b3080a7137df12cb9ec58c64b72925ec50d4ead3e",
	"tests/sandbox_process.rs":  "d33d493b172347bf213516d18e8712edb698418aa273de389667fff45889ea18",
	"tests/worker.rs":           "0f8a7076552ab5915601277c119822e98a4cd258b5cd1e931de5fca9a71b04ef",
}
var rustLakeSources = []string{"build.rs", "src/lib.rs", "src/proto.rs", "src/main.rs", "src/query.rs", "src/snapshot.rs", "src/sandbox.rs", "examples/sandbox-probe.rs", "tests/sandbox_process.rs", "tests/worker.rs"}

func checkRustLakeSnapshot(root string) error {
	base := filepath.Join(root, rustLakePath)
	manifest, err := os.ReadFile(filepath.Join(base, "Cargo.toml"))
	if err != nil {
		return err
	}
	if rustMigrationCompact(string(manifest)) != rustMigrationCompact(rustLakeCargo) {
		return fmt.Errorf("unreviewed lake worker dependencies")
	}
	allowed := map[string]bool{}
	found := map[string]bool{}
	for _, p := range rustLakeSources {
		allowed[p] = true
	}
	forbidden := regexp.MustCompile(`\b(?:unsafe|f32|f64|fs|net|process|thread|env|os|libc|extern|spawn|spawn_blocking)\b|\b(?:fs|net|process|thread|env)\s*::|\b(?:File|OpenOptions|Path|PathBuf|TcpStream|UdpSocket|Command)\b|\.\s*(?:sql|register_[A-Za-z_]+|read_parquet|read_csv|read_json)\s*\(|\b(?:struct|enum|type|union)\s+(?:r#)?(?:Decimal|Money|Envelope|EventFrame|Order|Position)\b|SubmitOrder|CancelOrder|ExecuteRequest|venue\s*::|admission\s*::|include(?:_str|_bytes)?\s*!|#\s*\[\s*path`)
	floats := regexp.MustCompile(`\b[0-9][0-9_]*\.[0-9][0-9_]*|\b[0-9][0-9_]*[eE][+-]?[0-9]|\b[0-9][0-9_]*f(?:32|64)\b`)
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, e := filepath.Rel(base, path)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("lake source symlink: %s", rel)
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(rel), ".proto") || (strings.EqualFold(d.Name(), "Cargo.toml") && rel != "Cargo.toml") || strings.EqualFold(d.Name(), "config") || strings.EqualFold(d.Name(), "config.toml") {
			return fmt.Errorf("unreviewed lake source contract/config: %s", rel)
		}
		if !strings.EqualFold(filepath.Ext(rel), ".rs") {
			return nil
		}
		if !allowed[rel] {
			return fmt.Errorf("unreviewed lake source: %s", rel)
		}
		found[rel] = true
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		body, e := rustMigrationWithoutComments(string(raw))
		if e != nil {
			return e
		}
		if digest, ok := rustLakePinned[rel]; ok {
			if fmt.Sprintf("%x", sha256.Sum256([]byte(rustMigrationCompact(body)))) != digest {
				return fmt.Errorf("unreviewed lake fixed recipe: %s", rel)
			}
		} else if forbidden.MatchString(body) || floats.MatchString(body) {
			return fmt.Errorf("lake worker authority/API boundary: %s", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for p := range allowed {
		if !found[p] {
			return fmt.Errorf("missing lake source: %s", p)
		}
	}
	return nil
}

func TestRustLakeSnapshotBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if err := checkRustLakeSnapshot(root); err != nil {
		t.Fatal(err)
	}
}

func TestRustLakeSnapshotBoundaryRejectsWeakening(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for _, tc := range []struct{ name, file, old, new string }{
		{"request_before_filter", "src/main.rs", "if sandbox::install().is_err()", "let _ = run(&runtime); if sandbox::install().is_err()"},
		{"drop_filter", "src/main.rs", "sandbox::install().is_err()", "false"},
		{"new_dependency", "Cargo.toml", "bytes =", "reqwest = \"=0.13.5\"\nbytes ="},
		{"parallel_proto", "copied.proto", "", "message Envelope {}"},
		{"new_source", "src/unreviewed.rs", "", "pub fn extra() {}"},
		{"network", "src/query.rs", "", "\nuse std::net::TcpStream;"},
		{"sql", "src/query.rs", "", "\nfn extra(ctx: SessionContext) { ctx.sql(\"select 1\"); }"},
		{"file", "src/snapshot.rs", "", "\nuse std::fs::File;"},
		{"financial_float", "src/lib.rs", "", "\npub fn price() -> f64 { 1.2 }"},
		{"copied_model", "src/lib.rs", "", "\npub struct Decimal {}"},
		{"alternate_proto", "build.rs", "envelope/v1/event_frame.proto", "other/frame.proto"},
		{"sandbox_default", "src/sandbox.rs", "0x0005_0000 | libc::EPERM as u32", "0x7fff_0000"},
		{"test_network_escape", "tests/worker.rs", "", "\nuse std::net::TcpStream;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			base := filepath.Join(dst, rustLakePath)
			for _, p := range append([]string{"Cargo.toml"}, rustLakeSources...) {
				b, e := os.ReadFile(filepath.Join(root, rustLakePath, p))
				if e != nil {
					t.Fatal(e)
				}
				out := filepath.Join(base, p)
				if e = os.MkdirAll(filepath.Dir(out), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(out, b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if e := checkRustLakeSnapshot(dst); e != nil {
				t.Fatalf("baseline: %v", e)
			}
			path := filepath.Join(base, tc.file)
			b, e := os.ReadFile(path)
			if e != nil && !os.IsNotExist(e) {
				t.Fatal(e)
			}
			body := string(b)
			if tc.old != "" {
				if !strings.Contains(body, tc.old) {
					t.Fatal("mutation anchor missing")
				}
				body = strings.Replace(body, tc.old, tc.new, 1)
			} else {
				body += tc.new
			}
			if e = os.WriteFile(path, []byte(body), 0600); e != nil {
				t.Fatal(e)
			}
			if e = checkRustLakeSnapshot(dst); e == nil {
				t.Fatal("weakening accepted")
			}
		})
	}
}

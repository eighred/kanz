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

// #1413 allows two named public-data runtimes and their shared owner. These conservative lexical/file/
// direct-declaration checks are not Rust AST, macro expansion, data flow, Cargo
// resolution or TLS proof. Compiler, replay, broker and dependency checks remain.
const rustDepthBuild = `fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = "../../../kanz-schemas/proto";
    let schemas = [
        "market/v1/order_book.proto",
        "market/v1/market_data.proto",
        "envelope/v1/event_frame.proto",
    ];
    for schema in schemas {
        println!("cargo:rerun-if-changed={root}/{schema}");
    }
    println!("cargo:rerun-if-changed={root}/envelope/v1/envelope.proto");
    println!("cargo:rerun-if-changed={root}/envelope/v1/event_class.proto");
    let inputs: Vec<String> = schemas.iter().map(|s| format!("{root}/{s}")).collect();
    let mut config = prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.extern_path(".common.v1", "::kanz_decimal_compat::common::v1");
    config.compile_protos(&inputs, &[root])?;
    Ok(())
}`

type rustFeedSpec struct {
	manifest string
	sources  map[string]bool
}

var rustFeedPackages = map[string]rustFeedSpec{
	"crates/market-feed": {manifest: `[package]
name = "kanz-market-feed"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
kanz-decimal-compat = { path = "../.." }
prost = "=0.14.1"
prost-types = "=0.14.1"
num-bigint = "=0.4.6"
num-rational = "=0.4.2"
num-traits = "=0.2.19"
tokio = { version = "=1.54.1", features = ["macros", "rt-multi-thread", "signal", "time", "net", "io-util", "sync"] }
reqwest = { version = "=0.13.5", default-features = false, features = ["rustls-no-provider", "stream", "json", "query"] }
async-nats = { version = "=0.51.0", default-features = false, features = ["ring", "jetstream", "nuid"] }
rustls = { version = "=0.23.46", default-features = false, features = ["ring", "std", "tls12"] }
spiffe = { version = "=0.19.0", features = ["x509-source"] }
spiffe-rustls = "=0.11.0"
uuid = { version = "=1.29.0", features = ["v7"] }

[build-dependencies]
prost-build = "=0.14.1"
protoc-bin-vendored = "=3.2.0"

[dev-dependencies]
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"`, sources: map[string]bool{"build.rs": true, "src/lib.rs": true, "src/book.rs": true, "src/decimal.rs": true, "src/publisher.rs": true, "src/source.rs": true, "src/runtime.rs": true, "tests/go_okx_conformance.rs": true, "tests/support/go_corpus.rs": true}},
	"services/binance-depth": {manifest: `[package]
name = "kanz-binance-depth"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
kanz-market-feed = { path = "../../crates/market-feed" }
kanz-decimal-compat = { path = "../.." }
prost = "=0.14.1"
prost-types = "=0.14.1"
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"
tokio = { version = "=1.54.1", features = ["macros", "rt-multi-thread", "signal", "time", "net", "io-util", "sync"] }
futures-util = { version = "=0.3.34", features = ["sink"] }
tokio-tungstenite = { version = "=0.30.0", default-features = false, features = ["connect", "rustls-tls-webpki-roots"] }
reqwest = { version = "=0.13.5", default-features = false, features = ["rustls-no-provider", "stream", "json", "query"] }
rustls = { version = "=0.23.46", default-features = false, features = ["ring", "std", "tls12"] }`, sources: map[string]bool{"src/lib.rs": true, "src/main.rs": true, "src/source.rs": true, "tests/go_conformance.rs": true}},
	"services/okx-depth": {manifest: `[package]
name = "kanz-okx-depth"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
kanz-decimal-compat = { path = "../.." }
kanz-market-feed = { path = "../../crates/market-feed" }
prost-types = "=0.14.1"
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"
tokio = { version = "=1.54.1", features = ["macros", "rt-multi-thread", "time", "net", "sync"] }
futures-util = { version = "=0.3.34", features = ["sink"] }
tokio-tungstenite = { version = "=0.30.0", default-features = false, features = ["connect", "rustls-tls-webpki-roots"] }
url = "=2.5.8"
rustls = { version = "=0.23.46", default-features = false, features = ["ring", "std", "tls12"] }`, sources: map[string]bool{"src/lib.rs": true, "src/main.rs": true, "src/source.rs": true}},
}

func rustBoundaryFixtureFiles() []string {
	out := []string{"Cargo.toml", "build.rs", "src/lib.rs", "tests/go_conformance.rs"}
	for name, spec := range rustFeedPackages {
		out = append(out, name+"/Cargo.toml")
		for file := range spec.sources {
			out = append(out, name+"/"+file)
		}
	}
	out = append(out, rustBondShadowPath+"/Cargo.toml")
	for _, file := range rustBondShadowSources {
		out = append(out, rustBondShadowPath+"/"+file)
	}
	out = append(out, rustOMSPath+"/Cargo.toml")
	for _, file := range rustOMSSources {
		out = append(out, rustOMSPath+"/"+file)
	}
	return out
}
func checkRustDepthBoundary(root string) error {
	for name, spec := range rustFeedPackages {
		if err := checkRustFeedPackage(filepath.Join(root, filepath.FromSlash(name)), spec, name == "crates/market-feed"); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}
func checkRustFeedPackage(root string, spec rustFeedSpec, shared bool) error {
	raw, err := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if err != nil {
		return err
	}
	if rustMigrationCompact(string(raw)) != rustMigrationCompact(spec.manifest) {
		return fmt.Errorf("unreviewed feed manifest/dependencies")
	}
	if shared {
		raw, err = os.ReadFile(filepath.Join(root, "build.rs"))
		if err != nil {
			return err
		}
		if rustMigrationCompact(string(raw)) != rustMigrationCompact(rustDepthBuild) {
			return fmt.Errorf("unreviewed canonical build recipe")
		}
	}
	sources := spec.sources
	found := map[string]bool{}
	quoted := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	forbidden := regexp.MustCompile(`\bunsafe\b|\bf(?:32|64)\b|\b(?:struct|enum|union|type)\s+(?:r#)?(?:Decimal|Money|OrderBookSnapshot|OrderBookDelta|PriceLevel|Envelope|EventFrame)\b`)
	floats := regexp.MustCompile(`\b[0-9][0-9_]*\.[0-9][0-9_]*|\b[0-9][0-9_]*[eE][+-]?[0-9]|\b[0-9][0-9_]*f(?:32|64)\b|\b[0-9][0-9_]*\.(?:[^.a-zA-Z0-9_]|$)`)
	trading := regexp.MustCompile(`SubmitOrder|CancelOrder|ExecuteRequest|venue::|order::|X-MBX-APIKEY|OK-ACCESS-KEY|OK-ACCESS-SIGN|BINANCE_API_KEY|API_SECRET|hmac::|"(?:order|command|venue)\.`)
	publishing := regexp.MustCompile(`\.\s*(?:publish|publish_with_headers|try_publish|send_publish)\s*\(`)
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
			return fmt.Errorf("depth symlink %s", rel)
		}
		if d.IsDir() {
			if rel == "target" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(rel), ".proto") {
			return fmt.Errorf("parallel depth protobuf %s", rel)
		}
		if strings.EqualFold(d.Name(), "Cargo.toml") && rel != "Cargo.toml" {
			return fmt.Errorf("nested depth package %s", rel)
		}
		if strings.EqualFold(d.Name(), "config") || strings.EqualFold(d.Name(), "config.toml") {
			return fmt.Errorf("Cargo override %s", rel)
		}
		if !strings.EqualFold(filepath.Ext(rel), ".rs") {
			return nil
		}
		if !sources[rel] {
			return fmt.Errorf("unreviewed depth source %s", rel)
		}
		found[rel] = true
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		code, err := rustMigrationWithoutComments(string(raw))
		if err != nil {
			return err
		}
		lexical := quoted.ReplaceAllString(code, `""`)
		paths := regexp.MustCompile(`#\s*\[[^\]]*\bpath\b[^\]]*\]`).FindAllString(code, -1)
		expectedPath := ""
		if shared && rel == "tests/go_okx_conformance.rs" {
			expectedPath = `#[path = "support/go_corpus.rs"]`
		}
		if !shared && rel == "tests/go_conformance.rs" {
			expectedPath = `#[path = "../../../crates/market-feed/tests/support/go_corpus.rs"]`
		}
		if expectedPath == "" && len(paths) != 0 || expectedPath != "" && (len(paths) != 1 || rustMigrationCompact(paths[0]) != rustMigrationCompact(expectedPath)) {
			return fmt.Errorf("unreviewed source path attribute in %s", rel)
		}

		includes := regexp.MustCompile(`\binclude(?:_str|_bytes)?\s*!`).FindAllString(code, -1)
		if (!shared || rel != "src/lib.rs") && len(includes) != 0 {
			return fmt.Errorf("unreviewed external source/data include in %s", rel)
		}
		if forbidden.MatchString(lexical) || floats.MatchString(lexical) || trading.MatchString(code) {
			return fmt.Errorf("unsafe/float/copied contract/trading credential token in %s", rel)
		}
		count := len(publishing.FindAllString(code, -1))
		if rel != "src/publisher.rs" && count != 0 {
			return fmt.Errorf("publication outside publisher %s", rel)
		}
		if rel == "src/publisher.rs" {
			if count != 1 || !strings.Contains(rustMigrationCompact(code), `js.publish_with_headers(subject,headers,frame.encode_to_vec().into())`) {
				return fmt.Errorf("unreviewed publish call")
			}
			for _, v := range []string{`pub const BOOK_SUBJECT: &str = "market.book.snapshot";`, `pub const QUOTE_SUBJECT: &str = "market.crypto.quote";`} {
				if !strings.Contains(rustMigrationCompact(code), rustMigrationCompact(v)) {
					return fmt.Errorf("missing canonical market subject")
				}
			}
			subject := regexp.MustCompile(`^"[a-z][a-z0-9_-]*(?:\.[a-z][a-z0-9_-]*)+"$`)
			for _, literal := range quoted.FindAllString(code, -1) {
				if subject.MatchString(literal) && literal != `"market.book.snapshot"` && literal != `"market.crypto.quote"` {
					return fmt.Errorf("unreviewed publisher subject %s", literal)
				}
			}
		}
		if shared && rel == "src/lib.rs" {
			if strings.Count(code, "include!") != 2 {
				return fmt.Errorf("unexpected generated modules")
			}
			for _, v := range []string{`pub use kanz_decimal_compat::common;`, `include!(concat!(env!("OUT_DIR"), "/market.v1.rs"));`, `include!(concat!(env!("OUT_DIR"), "/envelope.v1.rs"));`} {
				if !strings.Contains(rustMigrationCompact(code), rustMigrationCompact(v)) {
					return fmt.Errorf("canonical generated module missing")
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for rel := range sources {
		if !found[rel] {
			return fmt.Errorf("required depth source missing %s", rel)
		}
	}
	return nil
}
func TestRustDepthRuntimeBoundary(t *testing.T) {
	if err := checkRustDepthBoundary(filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")); err != nil {
		t.Fatal(err)
	}
}
func TestRustDepthBoundaryRejectsMutations(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for _, tc := range []struct{ name, path, text string }{
		{"dependency", "services/binance-depth/Cargo.toml", "\n[dev-dependencies]\nreqwest=\"*\"\n"},
		{"alternatebuild", "crates/market-feed/build.rs", "\nfn other() {}\n"},
		{"alternatepath", "services/okx-depth/src/source.rs", `#[path="/tmp/extra.rs"] mod extra;`},
		{"alternateinclude", "services/okx-depth/src/source.rs", "\ninclude!(\"hidden.rs\");"},
		{"copiedproto", "services/binance-depth/proto/market.proto", "message PriceLevel {}"},
		{"unreviewedmodule", "crates/market-feed/src/extra.rs", "pub fn extra() {}"},
		{"unsafe", "crates/market-feed/src/book.rs", "\nunsafe fn raw() {}"},
		{"float", "crates/market-feed/src/decimal.rs", "\npub fn amount()->f64 { 1.0 }"},
		{"unsuffixedfloat", "crates/market-feed/src/decimal.rs", "\nfn amount(){let n=1e3;}"},
		{"copieddecimal", "crates/market-feed/src/book.rs", "\nstruct Decimal { coefficient:i64 }"},
		{"trading", "services/okx-depth/src/main.rs", "\nconst SUBJECT:&str=\"order.submit\";"},
		{"exchangecredential", "services/binance-depth/src/source.rs", "\nconst KEY:&str=\"X-MBX-APIKEY\";"},
		{"thirdsubject", "crates/market-feed/src/publisher.rs", "\nconst OTHER:&str=\"market.other\";"},
		{"alternatepublisher", "services/okx-depth/src/source.rs", "\nfn send(){client.publish(subject,body);}"},
		{"binancepublisher", "services/binance-depth/src/source.rs", "\nfn send(){client.publish(subject,body);}"},
		{"okxcredential", "services/okx-depth/src/source.rs", "\nconst KEY:&str=\"OK-ACCESS-KEY\";"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			for _, rel := range rustBoundaryFixtureFiles() {
				b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dst, filepath.FromSlash(rel))
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := checkRustDepthBoundary(dst); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			path := filepath.Join(dst, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := f.WriteString(tc.text)
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("%v %v", writeErr, closeErr)
			}
			if err := checkRustDepthBoundary(dst); err == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

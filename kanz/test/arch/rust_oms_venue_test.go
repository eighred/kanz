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

// #1413: exact dependency/generation and source marker guard, not a Rust data-flow proof.
const rustVenueCargo = `[package]
name = "kanz-oms-venue"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
kanz-decimal-compat = { path = "../.." }
kanz-order-core = { path = "../order-core" }
kanz-proto-wire = { path = "../proto-wire" }
prost = "=0.14.1"
prost-reflect = "=0.16.2"
bytes = "=1.12.1"
http = "=1.5.0"
tonic = { version = "=0.14.9", default-features = false, features = ["channel", "codegen"] }
tokio = { version = "=1.54.1", features = ["macros", "rt", "time", "net", "sync"] }

hyper = "=1.12.0"
hyper-util = { version = "=0.1.21", features = ["tokio"] }
tower = { version = "=0.5.3", features = ["util"] }
tokio-rustls = { version = "=0.26.6", default-features = false, features = ["ring", "tls12"] }
rustls = { version = "=0.23.46", default-features = false, features = ["ring", "std", "tls12"] }
spiffe = { version = "=0.19.0", features = ["x509-source"] }
spiffe-rustls = "=0.11.0"

[build-dependencies]
prost-build = "=0.14.1"
protoc-bin-vendored = "=3.2.0"

[dev-dependencies]
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"
base64 = "=0.22.1"
tokio = { version = "=1.54.1", features = ["macros", "rt-multi-thread", "io-std", "io-util"] }
`
const rustProtoWireCargo = `[package]
name = "kanz-proto-wire"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
prost = "=0.14.1"
prost-reflect = "=0.16.2"
`
const rustVenueBuild = `fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = "../../../kanz-schemas/proto";
    println!("cargo:rerun-if-changed={root}");
    let mut config = prost_build::Config::new();
    config.file_descriptor_set_path(
        std::path::PathBuf::from(std::env::var("OUT_DIR")?).join("venue-descriptors.bin"),
    );
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.extern_path(".common.v1", "::kanz_decimal_compat::common::v1");
    config.extern_path(".order.v1", "::kanz_order_core::proto::order::v1");
    config.compile_protos(&[format!("{root}/venue/v1/venue.proto")], &[root])?;
    Ok(())
}
`

func checkRustVenueBoundary(root string) error {
	for path, want := range map[string]string{"crates/oms-venue/Cargo.toml": rustVenueCargo, "crates/proto-wire/Cargo.toml": rustProtoWireCargo, "crates/oms-venue/build.rs": rustVenueBuild} {
		raw, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if e != nil {
			return e
		}
		code, e := rustMigrationWithoutComments(string(raw))
		if e != nil {
			return e
		}
		expected, e := rustMigrationWithoutComments(want)
		if e != nil {
			return e
		}
		if rustMigrationCompact(code) != rustMigrationCompact(expected) {
			return fmt.Errorf("unreviewed venue recipe %s", path)
		}
	}
	allowed := map[string]bool{"crates/oms-venue/build.rs": true, "crates/oms-venue/src/lib.rs": true, "crates/oms-venue/src/client.rs": true, "crates/oms-venue/src/transport.rs": true, "crates/oms-venue/src/wire.rs": true, "crates/oms-venue/examples/oms-venue-fixture.rs": true, "crates/proto-wire/src/canonical.rs": true, "crates/proto-wire/src/lib.rs": true}
	files := map[string]string{}
	for _, dir := range []string{"crates/oms-venue", "crates/proto-wire"} {
		if e := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("venue source symlink")
			}
			if d.IsDir() {
				return nil
			}
			rel, e := filepath.Rel(root, path)
			if e != nil {
				return e
			}
			rel = filepath.ToSlash(rel)
			if strings.EqualFold(filepath.Ext(rel), ".proto") || strings.EqualFold(d.Name(), "config.toml") || (d.Name() == "Cargo.toml" && rel != dir+"/Cargo.toml") {
				return fmt.Errorf("parallel venue configuration")
			}
			if filepath.Ext(rel) != ".rs" {
				return nil
			}
			if !allowed[rel] {
				return fmt.Errorf("unreviewed venue source %s", rel)
			}
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			files[rel] = string(b)
			code, e := rustMigrationWithoutComments(string(b))
			if e != nil {
				return e
			}
			if regexp.MustCompile(`\bunsafe\b|\bf(?:32|64)\b|(?:struct|enum|type)\s+(?:Decimal|Money|Fill|OrderState)\b`).MatchString(code) {
				return fmt.Errorf("parallel financial or unsafe venue model")
			}
			return nil
		}); e != nil {
			return e
		}
	}
	for name := range allowed {
		if _, ok := files[name]; !ok {
			return fmt.Errorf("missing venue source %s", name)
		}
	}
	return checkRustVenueMarkers(files)
}
func checkRustVenueMarkers(files map[string]string) error {
	required := map[string][]string{
		"crates/oms-venue/examples/oms-venue-fixture.rs": {`env!("KANZ_BUILD_VERSION")`, `Some("127.0.0.1"|"localhost"|"[::1]")`, `client.close().await`, `input.read(&mutbyte)`},
		"crates/oms-venue/src/transport.rs":              {`connection_host(&uri)?`, `parse::<std::net::Ipv6Addr>()`, `uri.scheme_str()!=Some("https")`, `starts_with("unix://")`, `spiffe_rustls::mtls_client(source)`, `spiffe_rustls::LocalOnly(domain)`, `spiffe_rustls::authorizer::exact([config.expected_peer.as_str()])`, `.authorize(authorizer)`, `tls.alpn_protocols=vec![b"h2".to_vec()]`, `alpn_protocol()!=Some(b"h2".as_slice())`, `TcpStream::connect`, `tls.connect(server_name,socket).await?`, `a.as_str().contains('@')`, `connect_with_connector(connector)`, `owner.close().await`, `self.owner.close().await`, `fortaskinself.tasks.abort(){let_=task.await;}`, `source.shutdown_with_timeout(Duration::from_secs(2)).await`, `tasks.handles.len()>=64`},
		"crates/oms-venue/src/client.rs":                 {`error.is::<tonic::TimeoutExpired>()`, `.map_err(rpc_error)`, `max_decoding_message_size(budget.bytes())`, `max_encoding_message_size(kanz_proto_wire::MAX_BYTES)`, `request.set_timeout(deadline)`, `tokio::time::timeout_at(expires,async`, `letexpires=tokio::time::Instant::now()+deadline`, `before_deadline(expires,result)`, `tokio::time::Instant::now()>=expires`, `error.code()==Code::Unimplemented`, `ViewState::Indeterminate`, `id.mic!=mic||id.account!=account`, `requirements.verified_account&&!id.account_verified`, `requirements.declared_order_types&&id.supported_order_types.is_empty()`},
		"crates/oms-venue/src/wire.rs":                   {`kanz_proto_wire::Canonical::decode(raw)`, `kanz_proto_wire::Canonical::from_value(value)`, `src.remaining()>self.1.bytes()`, `dst.put_slice(item.wire())`, `kanz_proto_wire::put_message(field.number(),state.wire(),&mutout)`, `Canonical::decode_with_budget(nested,budget)`},
		"crates/proto-wire/src/canonical.rs":             {`normalize_with_budget(T::descriptor()?,raw,budget)`, `value.encoded_len()>MAX_BYTES`},
		"crates/proto-wire/src/lib.rs":                   {`raw.len()>limit`, `out.len().checked_add(raw.len()).is_none_or(|n|n>limit)`, `ifdepth>32`, `unknown.raw.extend_from_slice(&raw[value..pos])`, `entries[0].merge(sidecar,&child,limit)?`, `field.is_list()||entries.is_empty()`, `field.is_map()||field.containing_oneof().is_some()`},
	}
	for name, markers := range required {
		code, e := rustMigrationWithoutComments(files[name])
		if e != nil {
			return e
		}
		code = rustMigrationCompact(code)
		for _, m := range markers {
			if !strings.Contains(code, m) {
				return fmt.Errorf("venue %s missing %s", name, m)
			}
		}
	}
	return nil
}
func TestRustVenueClientBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if e := checkRustVenueBoundary(root); e != nil {
		t.Fatal(e)
	}
	files := map[string]string{}
	for _, name := range []string{"crates/oms-venue/src/transport.rs", "crates/oms-venue/src/client.rs", "crates/oms-venue/src/wire.rs", "crates/proto-wire/src/lib.rs", "crates/proto-wire/src/canonical.rs", "crates/oms-venue/examples/oms-venue-fixture.rs"} {
		raw, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if e != nil {
			t.Fatal(e)
		}
		files[name] = string(raw)
	}
	for _, m := range []struct{ name, file, from, to string }{
		{"secure-endpoint", "transport.rs", `uri.scheme_str() != Some("https")`, `false`},
		{"h2", "transport.rs", `tls.alpn_protocols = vec![b"h2".to_vec()]`, `tls.alpn_protocols = vec![]`},
		{"tls-required", "transport.rs", `tls.connect(server_name, socket).await?`, `socket`},
		{"ipv6-host", "transport.rs", `let host = connection_host(&uri)?;`, `let host = uri.host().unwrap().to_owned();`},
		{"userinfo", "transport.rs", `a.as_str().contains('@')`, `false`},
		{"exact-peer", "transport.rs", `spiffe_rustls::authorizer::exact([config.expected_peer.as_str()])`, `spiffe_rustls::authorizer::any()`},
		{"joined-source", "transport.rs", `source.shutdown_with_timeout(Duration::from_secs(2)).await`, `Ok::<(),()>(())`},
		{"unknown-not-error", "client.rs", `error.code() == Code::Unimplemented`, `true`},
		{"own-deadline", "client.rs", `tokio::time::Instant::now() >= expires`, `false`},
		{"late-result", "client.rs", `before_deadline(expires, result)`, `result`},
		{"typed-timeout-only", "client.rs", `error.is::<tonic::TimeoutExpired>()`, `true`},
		{"identity", "client.rs", `id.mic != mic || id.account != account`, `false`},
		{"raw-wire", "wire.rs", `dst.put_slice(item.wire())`, `dst.put_slice(&item.value.encode_to_vec())`},
	} {
		t.Run(m.name, func(t *testing.T) {
			path := "crates/oms-venue/src/" + m.file
			before := files[path]
			after := strings.ReplaceAll(before, m.from, m.to)
			if after == before {
				t.Fatal("mutation missed")
			}
			files[path] = after
			defer func() { files[path] = before }()
			if checkRustVenueMarkers(files) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

func TestRustVenueActualEvidenceRequired(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	w, e := os.ReadFile(filepath.Join(root, ".github/workflows/rust-oms-fold.yml"))
	if e != nil {
		t.Fatal(e)
	}
	c, e := os.ReadFile(filepath.Join(root, ".github/scripts/check_oms_fold_evidence.py"))
	if e != nil {
		t.Fatal(e)
	}
	check := func(w, c string) bool {
		for _, marker := range []string{"cargo test --locked -p kanz-oms-venue --all-targets", "cargo clippy --locked -p kanz-oms-venue --all-targets -- -D warnings", "cargo build --locked -p kanz-oms-venue --example oms-venue-fixture", "KANZ_OMS_VENUE_BIN:", "KANZ_OMS_VENUE_VERSION:", "./services/oms/tools/venue-conformance", "set -o pipefail", "check_oms_fold_evidence.py"} {
			if !strings.Contains(w, marker) {
				return false
			}
		}
		for _, marker := range []string{"TestRustVenueActualTLSAndRPC/unknown-duplicate-and-noncanonical-wire", "TestRustVenueActualTLSAndRPC/query-three-state-and-unimplemented", "TestRustVenueIdentityRefusals/wrong-peer", "TestRustVenueIdentityRefusals/wrong-trust-domain", "TestRustVenueIdentityRefusals/plaintext-server", "TestRustVenueLifetime/cancel-connect-joins-before-process-exit", "TestRustVenueLifetime/close-joins-live-channel-and-workload-before-exit", "TestRustVenueRotationReconnect", "TestRustVenueHTTP1Refusal", "TestRustVenueIPv6RPC"} {
			if !strings.Contains(c, marker) {
				return false
			}
		}
		return true
	}
	if !check(string(w), string(c)) {
		t.Fatal("venue proof not mandatory")
	}
	for _, marker := range []string{"KANZ_OMS_VENUE_VERSION:", "./services/oms/tools/venue-conformance", "set -o pipefail"} {
		if check(strings.ReplaceAll(string(w), marker, "removed"), string(c)) {
			t.Fatal("workflow mutation accepted")
		}
	}
	for _, marker := range []string{"TestRustVenueRotationReconnect", "TestRustVenueHTTP1Refusal", "TestRustVenueIPv6RPC", "TestRustVenueIdentityRefusals/wrong-peer"} {
		if check(string(w), strings.ReplaceAll(string(c), marker, "removed")) {
			t.Fatal("missing actual case accepted")
		}
	}
}

// Workspace all-target checks compile the stamped venue example too.
func TestRustVenueWorkspaceBuildStamped(t *testing.T) {
	path := filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/architecture-migration.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	check := func(s string) bool {
		start := strings.Index(s, "- name: Rust format, compile and unit tests")
		if start < 0 {
			return false
		}
		s = s[start:]
		if end := strings.Index(s, "\n      - name:"); end >= 0 {
			s = s[:end]
		}
		return strings.Contains(s, `KANZ_BUILD_VERSION="$(git rev-parse HEAD)" cargo check --locked --workspace --all-targets`) &&
			strings.Contains(s, `KANZ_BUILD_VERSION="$(git rev-parse HEAD)" cargo clippy --locked --workspace --all-targets`) &&
			!strings.Contains(s, "export KANZ_BUILD_VERSION")
	}
	if !check(string(raw)) {
		t.Fatal("workspace venue example lacks immutable checkout stamp")
	}
	if check(strings.ReplaceAll(string(raw), `KANZ_BUILD_VERSION="$(git rev-parse HEAD)"`, "")) {
		t.Fatal("missing build stamp mutation accepted")
	}
}

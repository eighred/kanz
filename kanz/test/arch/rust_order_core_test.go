package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const rustOrderCoreCargo = `[package]
name = "kanz-order-core"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
kanz-decimal-compat = { path = "../.." }
kanz-proto-wire = { path = "../proto-wire" }
prost-reflect = "=0.16.2"
prost = "=0.14.1"
prost-types = "=0.14.1"
num-bigint = "=0.4.6"
num-rational = "=0.4.2"
num-traits = "=0.2.19"

[build-dependencies]
prost-build = "=0.14.1"
protoc-bin-vendored = "=3.2.0"

[dev-dependencies]
serde = { version = "=1.0.229", features = ["derive"] }
serde_json = "=1.0.154"
base64 = "=0.22.1"
`
const rustOrderCoreBuild = `fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = "../../../kanz-schemas/proto";
    println!("cargo:rerun-if-changed={root}");
    let mut config = prost_build::Config::new();
    config.file_descriptor_set_path(
        std::path::PathBuf::from(std::env::var("OUT_DIR")?).join("order-descriptors.bin"),
    );
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.extern_path(".common.v1", "::kanz_decimal_compat::common::v1");
    config.compile_protos(
        &[
            format!("{root}/order/v1/order.proto"),
            format!("{root}/order/v1/order_events.proto"),
            format!("{root}/envelope/v1/event_frame.proto"),
            format!("{root}/domain/v1/portfolio.proto"),
            format!("{root}/command/v1/command_outcome.proto"),
        ],
        &[root],
    )?;
    Ok(())
}
`

var rustOrderCoreSources = []string{"build.rs", "src/lib.rs", "src/aggregate.rs", "src/arithmetic.rs", "examples/order-aggregate-fixture.rs"}

func checkRustOrderCore(root string) error {
	dir := filepath.Join(root, "crates/order-core")
	for file, want := range map[string]string{"Cargo.toml": rustOrderCoreCargo, "build.rs": rustOrderCoreBuild} {
		raw, e := os.ReadFile(filepath.Join(dir, file))
		if e != nil {
			return e
		}
		if rustMigrationCompact(string(raw)) != rustMigrationCompact(want) {
			return fmt.Errorf("unreviewed pure order recipe %s", file)
		}
	}
	allowed := map[string]bool{}
	for _, p := range rustOrderCoreSources {
		allowed[p] = true
	}
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("order core symlink")
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, ".proto") || (d.Name() == "Cargo.toml" && rel != "Cargo.toml") {
			return fmt.Errorf("parallel order schema/config")
		}
		if !strings.HasSuffix(rel, ".rs") {
			return nil
		}
		if !allowed[rel] {
			return fmt.Errorf("unreviewed order source %s", rel)
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		files[rel] = string(raw)
		code, e := rustMigrationWithoutComments(string(raw))
		if e != nil {
			return e
		}
		if strings.HasPrefix(rel, "src/") && regexp.MustCompile(`\b(?:f32|f64|unsafe|TcpStream|Command|File)\b|std::(?:net|fs|process)|struct\s+(?:Decimal|Money|OrderState|Fill)\b`).MatchString(strings.ReplaceAll(code, "forbid(unsafe_code)", "")) {
			return fmt.Errorf("non-pure or parallel order model")
		}
		return nil
	})
	if err != nil {
		return err
	}
	for p := range allowed {
		if _, ok := files[p]; !ok {
			return fmt.Errorf("missing core source %s", p)
		}
	}
	return checkOrderCoreMarkers(files)
}
func checkOrderCoreMarkers(files map[string]string) error {
	required := map[string][]string{
		"src/lib.rs":                          {`pubusekanz_decimal_compat::common;`, `pubusearithmetic::{decimal,rational};`},
		"src/arithmetic.rs":                   {`(-64..=64).contains(&d.exponent)`, `rem.abs()*2>=d.abs()`, `rounded(value.numer()*BigInt::from(100_000_000),value.denom())`, `q=rounded(q,&BigInt::from(10))`},
		"src/aggregate.rs":                    {`working(st)?`, `ifq>leaves`, `ifq_value<filled`, `Action::Quarantine`, `st.venue_ack_at.is_none()`, `c.instrument_id.len()>64`, `b<0x20||b==0x7f`, `letmutnext=st.clone()`, `fields(st.wire(),|n,w|!changed_field(n,w))`, `next.status=ifremaining.is_zero()`, `carry(out,cmd,"new_quantity","ordered_quantity")`, `carry(state,cmd,from,to)`, `kanz_proto_wire::accepts_wire(&f,w)`, `n==source_number&&w==2`, `n!=target_number||w!=2`},
		"examples/order-aggregate-fixture.rs": {`env!("KANZ_BUILD_VERSION")`, `take(2*1024*1024+1)`, `state.message_wire("leaves_quantity")?`},
	}
	for path, markers := range required {
		code, e := rustMigrationWithoutComments(files[path])
		if e != nil {
			return e
		}
		code = rustMigrationCompact(code)
		for _, m := range markers {
			if !strings.Contains(code, m) {
				return fmt.Errorf("order %s missing %s", path, m)
			}
		}
	}
	return nil
}
func TestRustPureOrderOwnerBoundary(t *testing.T) {
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	if e := checkRustOrderCore(root); e != nil {
		t.Fatal(e)
	}
	files := map[string]string{}
	for _, p := range rustOrderCoreSources {
		raw, e := os.ReadFile(filepath.Join(root, "crates/order-core", p))
		if e != nil {
			t.Fatal(e)
		}
		files[p] = string(raw)
	}
	for _, m := range []struct{ name, file, from, to string }{
		{"half-away", "src/arithmetic.rs", "rem.abs() * 2 >= d.abs()", "rem.abs() * 2 > d.abs()"},
		{"rescale", "src/arithmetic.rs", "q = rounded(q, &BigInt::from(10))", "q /= 10"},
		{"domain", "src/arithmetic.rs", "(-64..=64).contains(&d.exponent)", "true"},
		{"overfill", "src/aggregate.rs", "if q > leaves", "if false"},
		{"amend-filled", "src/aggregate.rs", "if q_value < filled", "if false"},
		{"ack-presence", "src/aggregate.rs", "st.venue_ack_at.is_none()", "true"},
		{"raw-carry", "src/aggregate.rs", "fields(st.wire(), |n, w| !changed_field(n, w))", "fields(st.wire(), |_, _| false)"},
		{"known-wire-selection", "src/aggregate.rs", "kanz_proto_wire::accepts_wire(&f, w)", "true"},
		{"nested-carry", "src/aggregate.rs", "carry(out, cmd, \"new_quantity\", \"ordered_quantity\")", "Ok(out)"},
	} {
		t.Run(m.name, func(t *testing.T) {
			before := files[m.file]
			after := strings.ReplaceAll(before, m.from, m.to)
			if after == before {
				t.Fatal("mutation missed")
			}
			files[m.file] = after
			defer func() { files[m.file] = before }()
			if checkOrderCoreMarkers(files) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

func TestRustOrderCoreActualEvidence(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	workflow, e := os.ReadFile(filepath.Join(root, ".github/workflows/rust-oms-fold.yml"))
	if e != nil {
		t.Fatal(e)
	}
	checker, e := os.ReadFile(filepath.Join(root, ".github/scripts/check_oms_fold_evidence.py"))
	if e != nil {
		t.Fatal(e)
	}
	required := []string{"cargo test --locked -p kanz-order-core --all-targets", "cargo clippy --locked -p kanz-order-core --all-targets -- -D warnings", "cargo build --locked -p kanz-order-core --example order-aggregate-fixture", "KANZ_ORDER_AGGREGATE_BIN:", "KANZ_ORDER_AGGREGATE_VERSION:", "./services/oms/tools/aggregate-conformance"}
	check := func(w, c string) bool {
		for _, m := range required {
			if !strings.Contains(w, m) {
				return false
			}
		}
		for _, m := range []string{"TestAggregateIndependentOracle", "TestAggregateCanonicalBoundary", "TestRustOrderAggregateDifferential", "TestRustOrderAggregateRefusesMalformedWire", "TestRustOrderAggregateBoundedRefusals", "wire-accept-carries-nested-not-command-root", "numeric-sequential-double-round"} {
			if !strings.Contains(c, m) {
				return false
			}
		}
		return true
	}
	if !check(string(workflow), string(checker)) {
		t.Fatal("aggregate actual proof missing")
	}
	for _, m := range required {
		if check(strings.ReplaceAll(string(workflow), m, "removed"), string(checker)) {
			t.Fatalf("missing workflow gate accepted: %s", m)
		}
	}
	for _, m := range []string{"TestAggregateCanonicalBoundary", "TestRustOrderAggregateDifferential", "wire-accept-carries-nested-not-command-root"} {
		if check(string(workflow), strings.ReplaceAll(string(checker), m, "removed")) {
			t.Fatalf("missing actual case accepted: %s", m)
		}
	}
}

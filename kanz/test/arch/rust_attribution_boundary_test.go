package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func attributionPrimitiveBoundary(files map[string]string) bool {
	required := map[string][]string{
		"src/arithmetic.rs":                     {"decimal_fit(value, 64)", "decimal_fit(value, i32::MAX)", "exponent.checked_add(1)?"},
		"src/attribution.rs":                    {"measured == traded", "ok_or(Error::NoArrival)?", "Err(Error::NoFills)", "interval.to_nanos <= interval.from_nanos", "q / volume", "c.is_positive()"},
		"src/attribution_projection.rs":         {"!aggregate::is_terminal(st)", "!st.parent_order_id.is_empty()", "children.len() > 100_000", "bytes > MAX_BYTES", "!seen.insert", "count > 100_000", "v.numer().bits() > 4096", "v.denom().bits() > 4096", "parent.message_wire(name)?", "value.ordered_quantity = None", "value.arrival_at = None", "let identities = schedule_identity::ChildIds::new(&st.order_id);", ".map(|i| (identities.at(i64::from(i)), i))", "schedule_identity::interval", "Outcome::Unrepresentable", "Outcome::CapturedOutsideCanonicalDomain", "crate::decimal_domain(&dynamic, 0).is_ok()", "value.numer().bits() > 4096"},
		"src/schedule_identity.rs":              {"prefix.update(parent.as_bytes());", `prefix.update(b":");`, "let mut state = self.0.clone();", "state.update(index.to_string().as_bytes());", "&sum[..16]", ".min(i64::MAX as i128)", "span * i as u128 / slices as u128"},
		"examples/order-attribution-fixture.rs": {"struct Tape(BTreeMap<String, BTreeMap<String, Intervals>>)", "Entry::Occupied(_) => Err(Error::InvalidInput)", "self.0.get(instrument)", "std::ptr::eq(instrument, self.instrument)", "self.tape.0.get(instrument)", "t.for_instrument(&parent.value().instrument_id)", "venues.get(venue)", "intervals.get(&(from, to))"},
	}
	for path, markers := range required {
		source := strings.SplitN(files[path], "#[cfg(test)]", 2)[0]
		for _, marker := range markers {
			if !strings.Contains(source, marker) {
				return false
			}
		}
		for _, forbidden := range []string{"outbox::", "tokio_postgres", "Publisher", "Record", "Execute", "std::net", "std::fs"} {
			// Recorded is the canonical measurement message, not an outbox Record.
			if strings.Contains(strings.ReplaceAll(source, "ExecutionAttributionRecorded", "MeasurementMessage"), forbidden) {
				return false
			}
		}
	}
	fixture := strings.SplitN(files["examples/order-attribution-fixture.rs"], "#[cfg(test)]", 2)[0]
	resolve := strings.Index(fixture, "t.for_instrument(&parent.value().instrument_id)")
	measure := strings.Index(fixture, "let result = projection::measure(")
	if resolve < 0 || measure < 0 || resolve >= measure {
		return false
	}
	projection := files["src/attribution_projection.rs"]
	prefix := strings.Index(projection, "let identities = schedule_identity::ChildIds::new(&st.order_id);")
	loop := strings.Index(projection, "let index: std::collections::BTreeMap<_, _> = (0..count)")
	if prefix < 0 || loop < 0 || prefix >= loop || strings.Contains(projection, "schedule_identity::child_id(") {
		return false
	}
	return true
}

func TestRustAttributionPrimitiveBoundary(t *testing.T) {
	root := filepath.Join("..", "..", "..", "kanz-rs", "crates", "order-core")
	files := map[string]string{}
	for _, path := range []string{"src/arithmetic.rs", "src/attribution.rs", "src/attribution_projection.rs", "src/schedule_identity.rs", "examples/order-attribution-fixture.rs"} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		source, err := rustMigrationWithoutComments(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = source
	}
	if !attributionPrimitiveBoundary(files) {
		t.Fatal("attribution primitive authority, absence, byte or identity boundary changed")
	}
	for _, mutation := range []struct{ path, old, replacement string }{
		{"src/attribution_projection.rs", ".map(|i| (identities.at(i64::from(i)), i))", ".map(|i| (schedule_identity::child_id(&st.order_id, i64::from(i)), i))"},
		{"src/schedule_identity.rs", "let mut state = self.0.clone();", "let mut state = Sha256::new();"},
		{"examples/order-attribution-fixture.rs", "Entry::Occupied(_) => Err(Error::InvalidInput)", "Entry::Occupied(_) => Ok(())"},
		{"examples/order-attribution-fixture.rs", "intervals.get(&(from, to))", "intervals.values().next()"},
		{"src/attribution_projection.rs", "let identities = schedule_identity::ChildIds::new(&st.order_id);", ""},
		{"examples/order-attribution-fixture.rs", "std::ptr::eq(instrument, self.instrument)", "instrument == self.instrument"},
		{"examples/order-attribution-fixture.rs", "t.for_instrument(&parent.value().instrument_id)", "unresolved"},
		{"examples/order-attribution-fixture.rs", "self.tape.0.get(instrument)", "self.venues"},
		{"src/arithmetic.rs", "decimal_fit(value, 64)", "decimal_fit(value, i32::MAX)"},
		{"src/attribution_projection.rs", "crate::decimal_domain(&dynamic, 0).is_ok()", "true"},
		{"src/attribution.rs", "measured == traded", "measured > 0"},
		{"src/attribution_projection.rs", "count > 100_000", "false"},
		{"src/attribution_projection.rs", "bytes > MAX_BYTES", "false"},
		{"src/attribution_projection.rs", "parent.message_wire(name)?", "None"},
		{"src/attribution_projection.rs", "v.numer().bits() > 4096", "false"},
		{"src/schedule_identity.rs", ".min(i64::MAX as i128)", ""},
		{"src/schedule_identity.rs", "&sum[..16]", "&sum[..8]"},
	} {
		original := files[mutation.path]
		changed := strings.Replace(original, mutation.old, mutation.replacement, 1)
		if changed == original {
			t.Fatal("mutation did not apply")
		}
		files[mutation.path] = changed
		if attributionPrimitiveBoundary(files) {
			t.Fatal("attribution mutation accepted")
		}
		files[mutation.path] = original
	}
}

func TestRustAttributionActualEvidence(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	workflow, err := os.ReadFile(filepath.Join(root, ".github/workflows/rust-oms-fold.yml"))
	if err != nil {
		t.Fatal(err)
	}
	checker, err := os.ReadFile(filepath.Join(root, ".github/scripts/check_oms_attribution_evidence.py"))
	if err != nil {
		t.Fatal(err)
	}
	markers := []string{"cargo test --locked -p kanz-order-core --all-targets", "cargo clippy --locked -p kanz-order-core --all-targets -- -D warnings", "cargo build --locked -p kanz-order-core --example order-attribution-fixture", "KANZ_ORDER_ATTRIBUTION_BIN:", "KANZ_ORDER_ATTRIBUTION_VERSION:", "go test -race -p 1 ./services/oms/internal/order -run '^(TestAttribution|TestRustAttribution)'", "check_oms_attribution_evidence.py", "sha256sum \"$KANZ_ORDER_ATTRIBUTION_BIN\"", "--source \"$GITHUB_SHA\"", "--attempt \"$GITHUB_RUN_ATTEMPT\"", "name: oms-attribution-evidence"}
	valid := func(s string) bool {
		for _, marker := range markers {
			if !strings.Contains(s, marker) {
				return false
			}
		}
		return true
	}
	if !valid(string(workflow)) {
		t.Fatal("attribution actual qualification binding missing")
	}
	for _, marker := range markers {
		if valid(strings.ReplaceAll(string(workflow), marker, "removed")) {
			t.Fatal("missing actual qualification accepted")
		}
	}
	for _, marker := range []string{`e.get("Package") != "github.com/eighred/kanz/" + PACKAGE`, "read_bounded", "Counter", "set(counts) != NAMES", "n != 1", "TestAttributionCanonicalOutputDomainWitness", "long-parent-last-supported-index", "MAX_STREAM = 16 * 1024 * 1024"} {
		if !strings.Contains(string(checker), marker) {
			t.Fatal("attribution evidence boundary missing")
		}
	}
}

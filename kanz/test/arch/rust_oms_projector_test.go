package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRustOMSPositionProjectorBoundary(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs", "crates", "oms-store", "src", "position_projector.rs"))
	if err != nil {
		t.Fatal(err)
	}
	markers := []string{
		`envelope.len()>MAX_BYTES||payload.len()>MAX_BYTES-envelope.len()`,
		`tenant.is_empty()||(tenant!="__system__"&&env.tenant_id!=tenant)`,
		`crate::relay_wire::normalize(desc.clone(),payload)?`, `domain(&dynamic,0)?`,
		`kanz_order_core::decimal_domain(msg,depth)`, `env.event_type!=RECOVERED`, `env.event_class!=EventClass::Factasi32`, `env.tenant_id!=tenant`,
		`fill.order_id!=st.order_id`, `fill.venue_account_id!=st.venue_account_id`, `fill.instrument_id!=st.instrument_id`, `fill.side!=st.side`,
		`Sha256::digest(wire.without_recovery()?.journal_bytes()?)`, `fee_revision::terms(&wire)?`,
		`causation_id:env.event_id.clone()`, `trace_context:env.trace_context.clone()`,
		`position::apply(`, `recovery_ack(&env,&key,&wire,now)?`, `records.push(ack)`,
		`letout=relay.flush(&key).await.map_err(|e|e.error)?`, `ifout.stalled.is_some()`, `returnErr(Error::PublicationUnconfirmed)`,
	}
	check := func(raw string) bool {
		code, err := rustMigrationWithoutComments(raw)
		if err != nil {
			return false
		}
		code = rustMigrationCompact(code)
		for _, marker := range markers {
			if !strings.Contains(code, marker) {
				return false
			}
		}
		for _, forbidden := range []string{`SystemTime::now(`, `.publish(`, `.connect(`, `tokio::spawn(`, `input.records`} {
			if strings.Contains(code, forbidden) {
				return false
			}
		}
		return strings.Index(code, "position::apply(") < strings.Index(code, "relay.flush(&key)")
	}
	s := string(raw)
	if !check(s) {
		t.Fatal("projector transaction, provenance or publication boundary missing")
	}
	for _, mutation := range []string{"domain(&dynamic, 0)?", "fill.order_id != st.order_id", "fill.venue_account_id != st.venue_account_id", "causation_id: env.event_id.clone()", "recovery_ack(&env, &key, &wire, now)?", "records.push(ack)", "relay.flush(&key)", "out.stalled.is_some()", "env.event_class != EventClass::Fact as i32", "wire.without_recovery()?"} {
		changed := strings.ReplaceAll(s, mutation, "removed")
		if changed == s || check(changed) {
			t.Fatalf("projector mutation missed/accepted: %s", mutation)
		}
	}
}

func TestRustOMSProjectorActualCasesRequired(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	b, err := os.ReadFile(filepath.Join(root, ".github", "scripts", "check_oms_fold_evidence.py"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"TestRustPositionProjectorPostgresNATS", "filled-partial-lineage-and-restart", "unknown-and-nil-fill-ignore-but-foreign-refuses", "unknown-and-duplicate-embedded-wire-journal", "recovery-ack-binding-and-invalid-provenance-rollback", "flush-failure-keeps-committed-book-and-restart-retries", "deep-domain-and-malformed-refuse-without-writes", "commit-rollback-and-go-rust-go-handoff", "whole-input-capacity-refuses-without-prefix", "legacy-system-scope-and-strict-recovery-tenant"}
	check := func(s string) bool {
		for _, n := range names {
			if !strings.Contains(s, n) {
				return false
			}
		}
		return true
	}
	s := string(b)
	if !check(s) {
		t.Fatal("actual projector proof missing")
	}
	for _, n := range names {
		if check(strings.ReplaceAll(s, n, "removed")) {
			t.Fatal("missing actual projector case accepted")
		}
	}
}

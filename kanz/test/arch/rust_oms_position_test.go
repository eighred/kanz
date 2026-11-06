package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkRustOMSPosition(raw string) error {
	s, e := rustMigrationWithoutComments(raw)
	if e != nil {
		return e
	}
	s = rustMigrationCompact(s)
	for _, m := range []string{`pubasyncfnapply<F>`, `pubasyncfnsnapshot(`, `scoped(client,tenant).await?`, `relrowsecurityANDrelforcerowsecurity`, `ifn!=4`, `pg_advisory_xact_lock(hashtext(app_current_tenant()||'/'||$1),hashtext($2))`, `ONCONFLICT(tenant_id,fill_id)DONOTHING`, `ifclaimed>0`, `verify_duplicate(&tx,portfolio,&wire).await?`, `enqueue(&tx,&records).await?`, `commit(tx).await?`, `bounded_records(&records)?`, `record_execution(&tx,portfolio,&wire,as_of,l).await?`, `fee_journal::record_for(tx,Book::Position`, `fee_journal::head_for(tx,Book::Position`, `wire.journal_bytes()?`, `ORDERBYexecution_time,execution_keyLIMIT10001`, `ORDERBYexecution_timeDESC,execution_keyDESCLIMIT1`, `if(backdated||f.recovery.is_some())&&!complete`, `returnErr(Error::PositionHistoryUnknown)`, `ifrows.len()>MAX_ROWS`, `n<=10000ANDbytes<=1048576`, `sum(octet_length(fill)::bigint)OVER()`, `if!identity::domain(execution.value())`, `l.fold(execution.value())?`, `math::aggregate(`, `st.unrealized_pnl=None`, `log_position:None`, `tx.rollback().await?`, `result.encoded_len()>MAX_BYTES`, `applied.venue.encoded_len().checked_add(applied.aggregate.encoded_len())`, `stored.iter().any(|v|v.len()>4096)`, `math::bound(&nav)?`} {
		if !strings.Contains(s, m) {
			return fmt.Errorf("position owner missing %s", m)
		}
	}
	for _, m := range []string{`SystemTime::now(`, `.publish(`, `.connect(`, `DELETEFROMposition`, `TRUNCATE`, `cash_balance:Some`, `log_position:Some`} {
		if strings.Contains(s, m) {
			return fmt.Errorf("position authority/financial drift %s", m)
		}
	}
	return nil
}
func TestRustOMSPositionBoundary(t *testing.T) {
	p := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs", "crates", "oms-store", "src", "position.rs")
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	s := string(b)
	if e = checkRustOMSPosition(s); e != nil {
		t.Fatal(e)
	}
	for _, m := range []struct{ name, from, to string }{
		{"tenant", "scoped(client, tenant)", "client.transaction()"},
		{"forced-rls", "relforcerowsecurity", "true"},
		{"instrument-lock", "pg_advisory_xact_lock", "ignored_lock"},
		{"claim", "ON CONFLICT(tenant_id,fill_id) DO NOTHING", "ON CONFLICT DO UPDATE"},
		{"duplicate", "verify_duplicate(&tx, portfolio, &wire).await?", "Ok::<(), Error>(())?"},
		{"atomic-fact", "enqueue(&tx, &records).await?", "Ok::<(), Error>(())?"},
		{"history-provenance", "if (backdated || f.recovery.is_some()) && !complete", "if false"},
		{"replay-cap", "rows.len() > MAX_ROWS", "false"},
		{"byte-cap", "bytes<=1048576", "true"},
		{"fee-book", "Book::Position", "Book::Oms"},
		{"raw-journal", "wire.journal_bytes()?", "Vec::new()"},
		{"unknown-log", "log_position: None", "log_position: Some(Default::default())"},
	} {
		t.Run(m.name, func(t *testing.T) {
			changed := strings.ReplaceAll(s, m.from, m.to)
			if changed == s {
				t.Fatal("mutation missed")
			}
			if checkRustOMSPosition(changed) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

func TestRustOMSPositionMathBoundary(t *testing.T) {
	b, e := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs", "crates", "oms-store", "src", "position_math.rs"))
	if e != nil {
		t.Fatal(e)
	}
	check := func(raw string) bool {
		code, e := rustMigrationWithoutComments(raw)
		if e != nil {
			return false
		}
		code = rustMigrationCompact(code)
		for _, m := range []string{`kanz_order_core::{decimal,`, `rational,`, `v.numer().bits()>16384||v.denom().bits()>16384`, `s.len()>4096`, `ifd<=BigInt::zero()`, `bound(&out.qty)?`, `bound(&out.realized)?`, `bound(&weight)?`, `bound(&v)?`, `decimal(v).ok_or(Error::Capacity)?`} {
			if !strings.Contains(code, m) {
				return false
			}
		}
		return !strings.Contains(code, "fnrational(") && !strings.Contains(code, "fndecimal(")
	}
	s := string(b)
	if !check(s) {
		t.Fatal("math ownership/bounds missing")
	}
	for _, m := range []string{"v.numer().bits() > 16384", "s.len() > 4096", "bound(&weight)?", "bound(&v)?"} {
		t.Run(m, func(t *testing.T) {
			changed := strings.ReplaceAll(s, m, "false")
			if changed == s || check(changed) {
				t.Fatal("mutation missed/accepted")
			}
		})
	}
}

func TestRustOMSPositionCanonicalBrokerRequired(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), ".github", "workflows", "rust-oms-fold.yml"))
	if err != nil {
		t.Fatal(err)
	}
	markers := []string{
		"kanz/infra/nats/bootstrap-job.yaml",
		"natsio/nats-box@sha256:ffce8bd103383f179f8c7f11cf645726acf5d17280706c530c3b342dbe16334c",
		"-e NATS_REPLICAS=1",
		"sh -s < \"$RUNNER_TEMP/oms-bootstrap.sh\"",
		"./services/oms/internal/position",
	}
	check := func(s string) bool {
		for _, marker := range markers {
			if !strings.Contains(s, marker) {
				return false
			}
		}
		start := strings.Index(s, "- name: Start isolated pinned relay broker")
		bootstrap := strings.Index(s, "- name: Bootstrap canonical disposable position streams")
		compare := strings.Index(s, "- name: Compare actual stamped worker through the OMS supervisor")
		return start >= 0 && bootstrap > start && compare > bootstrap
	}
	s := string(b)
	if !check(s) {
		t.Fatal("canonical position broker proof missing or out of order")
	}
	for _, marker := range markers {
		if check(strings.ReplaceAll(s, marker, "removed")) {
			t.Fatal("missing broker prerequisite accepted")
		}
	}
	reordered := strings.Replace(s, "- name: Bootstrap canonical disposable position streams", "- name: removed", 1)
	if check(reordered) {
		t.Fatal("missing bootstrap stage accepted")
	}
}

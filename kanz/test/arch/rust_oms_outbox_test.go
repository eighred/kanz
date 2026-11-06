package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Repository-source boundary only. Actual PostgreSQL proves advisory-lock
// interoperability, raw records and transaction results in the required lane.
func checkRustOMSOutbox(source string) error {
	code, e := rustMigrationWithoutComments(source)
	if e != nil {
		return e
	}
	code = rustMigrationCompact(code)
	for _, marker := range []string{
		`constMAX_PAGE:i64=4096`, `constSUFFIX:&str="/oms/outbox"`,
		`current_database()||'/'||('outbox'::regclass)::oid::text||'/'||app_current_tenant()||$2`,
		`producer_service='oms'`, `producer_serviceISNULL`, `sum(octet_length(`,
		`scoped(client,tenant).await?`, `scoped(&mutsession.client,tenant).await?`,
		`self.driver.abort()`, `implDropforSession`,
		`pg_advisory_lock($1,$2)`, `pg_try_advisory_lock($1,$2)`, `pg_advisory_unlock($1,$2)`,
		`Duration::from_secs(5)`, `if!unlocked`,
		`published_at=COALESCE(published_at,now())`, `attempts=attempts+1`,
		`tx.commit().await.map_err(|_|Error::CommitUnconfirmed)`,
		`row.try_get::<_,Option<Vec<u8>>>(6)?.ok_or(Error::Capacity)?`,
	} {
		if !strings.Contains(code, marker) {
			return fmt.Errorf("OMS outbox boundary missing %s", marker)
		}
	}
	for _, name := range []string{"pending_keys", "pending", "mark_published", "mark_failed", "oldest_pending_age", "lock_key"} {
		if !strings.Contains(code, "pubasyncfn"+name+"(") {
			return fmt.Errorf("queue operation absent: %s", name)
		}
	}
	return nil
}

func TestRustOMSOutboxBoundary(t *testing.T) {
	path := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs", "crates", "oms-store", "src", "outbox.rs")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	source := string(raw)
	if e = checkRustOMSOutbox(source); e != nil {
		t.Fatal(e)
	}
	for _, mutation := range []struct{ name, from, to string }{
		{"namespace", "/oms/outbox", "/other/outbox"},
		{"tenant", "app_current_tenant()", "'shared'"},
		{"physical-table", "('outbox'::regclass)::oid::text", "'outbox'"},
		{"session-drop", "self.driver.abort()", "drop(0)"},
		{"unlock-check", "if !unlocked", "if false"},
		{"commit-ambiguity", "Error::CommitUnconfirmed", "Error::Database"},
		{"raw-bytes", "Option<Vec<u8>>", "Option<String>"},
		{"service", "producer_service='oms'", "producer_service IS NOT NULL"},
		{"published-idempotency", "COALESCE(published_at,now())", "now()"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := strings.ReplaceAll(source, mutation.from, mutation.to)
			if changed == source {
				t.Fatal("mutation missed source")
			}
			if checkRustOMSOutbox(changed) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

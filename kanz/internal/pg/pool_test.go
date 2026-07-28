package pg_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
)

// A pool with no tenant can read nothing (RLS) and is indistinguishable, at the
// database, from a service that forgot to scope itself. It is refused at
// construction — the composition root cannot build one by accident.
func TestNewTenantPoolRefusesAnUnscopedPool(t *testing.T) {
	_, err := pg.NewTenantPool(context.Background(), "postgres://x/y", "")
	if err == nil {
		t.Fatal("a pool with no tenant was built: every query it makes would read nothing, silently")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("the error must say what is missing; got %v", err)
	}
}

// TestAnUnscopedQueryErrorsRatherThanReturningEmpty pins MT-01e at the ENGINE.
//
// Isolation used to be `USING (tenant_id = current_setting('app.tenant_id', true))`
// — and that `true` is missing_ok. A session that never set the GUC therefore got
// NULL, the predicate went NULL, and the query returned ZERO ROWS. Silently. An
// unscoped read and a tenant with genuinely no data were the SAME OBSERVABLE EVENT,
// and the dangerous one was the invisible one. accounting shipped exactly that: a
// pool with no AfterConnect, a ledger that read nothing and wrote nothing, and a
// green test suite.
//
// Now app_current_tenant() RAISES. This drives it through a REAL pool against a REAL
// database, both ways:
//
//	no tenant on the session   → ERROR. Nobody said who is asking.
//	a tenant, no matching rows → 0 rows. That is isolation working, not a failure.
func TestAnUnscopedQueryErrorsRatherThanReturningEmpty(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_URL to drive the tenant-scope guarantee against a real database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A properly scoped pool: this is the ONLY way a service builds one.
	pool, err := pg.NewTenantPool(ctx, dsn, "acme")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	const ddl = `
		CREATE OR REPLACE FUNCTION app_current_tenant() RETURNS text
		LANGUAGE plpgsql STABLE AS $fn$
		DECLARE t text;
		BEGIN
		  t := current_setting('app.tenant_id', true);
		  IF t IS NULL OR t = '' THEN
		    RAISE EXCEPTION 'kanz: tenant scope missing' USING ERRCODE = '42501';
		  END IF;
		  RETURN t;
		END $fn$;
		DROP TABLE IF EXISTS scope_probe;
		CREATE TABLE scope_probe (tenant_id TEXT NOT NULL DEFAULT app_current_tenant(), v TEXT);
		ALTER TABLE scope_probe ENABLE ROW LEVEL SECURITY;
		ALTER TABLE scope_probe FORCE  ROW LEVEL SECURITY;
		CREATE POLICY tenant_isolation ON scope_probe
		  USING (tenant_id = app_current_tenant())
		  WITH CHECK (tenant_id = app_current_tenant());`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("provision the probe table (needs a non-superuser owner): %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, "DROP TABLE IF EXISTS scope_probe")
	})

	if _, err := pool.Exec(ctx, "INSERT INTO scope_probe (v) VALUES ('acme secret')"); err != nil {
		t.Fatalf("insert under the scoped pool: %v", err)
	}

	// THE SCOPED READ — the tenant sees its own row.
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM scope_probe").Scan(&n); err != nil {
		t.Fatalf("scoped read: %v", err)
	}
	if n != 1 {
		t.Fatalf("the scoped pool saw %d rows, want 1", n)
	}

	// THE UNSCOPED READ. A raw connection that never set the GUC — a bare
	// pgxpool.New(), which is precisely what accounting shipped.
	unscoped, err := unscopedConn(ctx, dsn)
	if err != nil {
		t.Fatalf("open an unscoped connection: %v", err)
	}
	defer unscoped.Close(ctx)

	err = unscoped.QueryRow(ctx, "SELECT count(*) FROM scope_probe").Scan(&n)
	if err == nil {
		t.Fatalf("AN UNSCOPED QUERY RETURNED %d ROWS INSTEAD OF ERRORING.\n"+
			"Nobody told the database who was asking, and it answered anyway. That answer is indistinguishable "+
			"from 'this tenant has no data' — which is how a service can be silently unable to read its own book "+
			"of record while every probe stays green.", n)
	}
	if !strings.Contains(err.Error(), "tenant scope missing") {
		t.Fatalf("the unscoped query failed, but not for the right reason: %v", err)
	}

	// And a scoped-but-EMPTY read is NOT an error: a tenant with no data is a real
	// answer, and isolation is doing its job.
	other, err := pg.NewTenantPool(ctx, dsn, "some-other-tenant")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.QueryRow(ctx, "SELECT count(*) FROM scope_probe").Scan(&n); err != nil {
		t.Fatalf("a correctly scoped tenant with no rows must get an ANSWER, not an error: %v", err)
	}
	if n != 0 {
		t.Fatalf("tenant isolation leaked: another tenant saw %d rows", n)
	}
}

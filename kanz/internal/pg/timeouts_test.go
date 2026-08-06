package pg_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
)

// TestTheTimeBoundsReachTheDatabase drives #228's time bounds against a REAL
// Postgres, because nothing else does and the issue's own acceptance command
// cannot.
//
// #228's "Verified when" reads:
//
//	psql "$TEST_POSTGRES_URL" -c "show statement_timeout"   # today: 0
//
// That was correct as EVIDENCE — it is how the gap was found — and it is useless
// as PROOF. prepare() sets these three GUCs as startup-packet RuntimeParams on
// the pgx connection (pool.go, cfg.ConnConfig.RuntimeParams), so they are a
// property of a connection THIS PACKAGE opened. psql opens its own connection
// and never runs that code, so it reports the server default — 0 — whether the
// fix works perfectly or was reverted this morning. Running it post-fix and
// reading the 0 as "still broken" is the trap; running it and reading 0 as
// "fine, that is just psql" is the worse one.
//
// So the assertion has to come through a pool built the way a service builds it.
//
// IT READS pg_settings RATHER THAN `SHOW`. SHOW renders prose — `90s`, `1min`,
// `5s` — and a test comparing those strings is testing Postgres's formatter,
// which has changed before and will change again. pg_settings.setting is the
// value in the GUC's base unit (ms for all three here) as an integer string.
//
// IT ASSERTS source = 'client' AS WELL AS THE VALUE, and that is the half that
// carries this repository's rule that "nothing configured" and "checked, and
// fine" must never look the same. A bound the estate never set and a bound the
// estate deliberately set to the same number are the identical NUMBER and
// different SOURCES: 'default' means nobody decided, 'client' means the startup
// packet carried a decision. Migration's Unbounded is the case that makes this
// concrete — it is 0, the same 0 as an unconfigured server, and only the source
// column tells the two apart.
func TestTheTimeBoundsReachTheDatabase(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_URL to drive #228's time bounds against a real database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The profile must not be vacuously zero, or every assertion below would pass
	// against a pool that bounds nothing.
	if pg.Service.StatementTimeout <= 0 || pg.Service.LockTimeout <= 0 || pg.Service.IdleInTxTimeout <= 0 {
		t.Fatalf("the Service profile carries a non-positive bound (%v/%v/%v); #228's whole claim is that "+
			"every service statement is bounded, so this test would be asserting nothing",
			pg.Service.StatementTimeout, pg.Service.LockTimeout, pg.Service.IdleInTxTimeout)
	}

	// A global pool rather than a tenant one: this test is about the time bounds,
	// which prepare() applies to every profile identically, and a tenant pool would
	// drag RLS provisioning into a test that is not about isolation.
	pool, err := pg.NewGlobalPool(ctx, dsn, "this test asserts connection-level GUCs, not tenant-scoped rows")
	if err != nil {
		t.Fatalf("open a service pool: %v", err)
	}
	defer pool.Close()

	// (a) of #228's Verified-when: MaxConns comes from configuration, not from
	// pgx's max(4, runtime.NumCPU()) default. Read off the live pool's config.
	if got := pool.Config().MaxConns; got != pg.ServiceMaxConns {
		t.Errorf("the service pool is sized %d, want ServiceMaxConns=%d — an unsized pool inherits "+
			"max(4, NumCPU()), which on a container with no CPU quota is the host's core count",
			got, pg.ServiceMaxConns)
	}

	for _, c := range []struct {
		guc  string
		want time.Duration
	}{
		{"statement_timeout", pg.Service.StatementTimeout},
		{"lock_timeout", pg.Service.LockTimeout},
		{"idle_in_transaction_session_timeout", pg.Service.IdleInTxTimeout},
	} {
		var setting, source string
		err := pool.QueryRow(ctx,
			"SELECT setting, source FROM pg_settings WHERE name = $1", c.guc).Scan(&setting, &source)
		if err != nil {
			t.Fatalf("read %s from pg_settings: %v", c.guc, err)
		}
		if want := millisStr(c.want); setting != want {
			t.Errorf("%s on a service connection is %s ms, want %s ms", c.guc, setting, want)
		}
		if source != "client" {
			t.Errorf("%s is %q on the connection but its source is %q, want \"client\". A value that "+
				"happens to match while sourced from the server default is not this estate's decision — "+
				"it is the server's, and it changes when the server does", c.guc, setting, source)
		}
	}

	// THE CONTRAST, and the reason #228's psql command reads 0 forever. A bare
	// connection to the SAME DSN — which is what psql is — carries none of this.
	// If this half ever stops holding, the bounds moved to the server or the role
	// and the mechanism under test is no longer the one being verified.
	bare, err := unscopedConn(ctx, dsn)
	if err != nil {
		t.Fatalf("open a bare connection: %v", err)
	}
	defer bare.Close(ctx)

	var bareSetting, bareSource string
	if err := bare.QueryRow(ctx,
		"SELECT setting, source FROM pg_settings WHERE name = 'statement_timeout'").
		Scan(&bareSetting, &bareSource); err != nil {
		t.Fatalf("read statement_timeout on the bare connection: %v", err)
	}
	if bareSource == "client" {
		t.Errorf("a bare pgx.Connect carries statement_timeout=%s from the client, so this test is no "+
			"longer proving that internal/pg is what bounds a connection", bareSetting)
	}
}

// TestMigrationBoundsAreDecidedNotDefaulted pins the case the value alone cannot
// express. Migration is deliberately Unbounded for statement_timeout — DDL runs
// long and kanz-migrate's -timeout flag is the one bound — so its GUC is 0, the
// SAME 0 a server that was never configured reports. Only pg_settings.source
// separates "we decided not to bound this, and wrote down why" from "nobody
// looked". Its lock_timeout is bounded and different from Service's, which is
// what proves the profile is really being applied rather than one global setting
// reaching every pool.
func TestMigrationBoundsAreDecidedNotDefaulted(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_URL to drive the migration profile against a real database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pg.NewMigrationPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open a migration pool: %v", err)
	}
	defer pool.Close()

	if got := pool.Config().MaxConns; got != pg.Migration.MaxConns {
		t.Errorf("the migration pool is sized %d, want %d — a second connection is a second migrator",
			got, pg.Migration.MaxConns)
	}

	var setting, source string
	if err := pool.QueryRow(ctx,
		"SELECT setting, source FROM pg_settings WHERE name = 'statement_timeout'").
		Scan(&setting, &source); err != nil {
		t.Fatalf("read statement_timeout: %v", err)
	}
	if want := millisStr(pg.Migration.StatementTimeout); setting != want {
		t.Errorf("the migration statement_timeout is %s ms, want %s ms", setting, want)
	}
	if source != "client" {
		t.Errorf("the migration statement_timeout is unbounded by DECISION (DDL legitimately runs long, "+
			"and -timeout is the bound), but its source is %q rather than \"client\" — which makes it "+
			"indistinguishable from a server nobody configured", source)
	}

	// Service and Migration must not agree here, or a single server-side setting
	// could satisfy both and the per-profile mechanism would be untested.
	if pg.Migration.LockTimeout == pg.Service.LockTimeout {
		t.Fatalf("Migration and Service now share a lock_timeout (%v). This test distinguishes "+
			"per-profile application from one global setting, and it can no longer do that",
			pg.Migration.LockTimeout)
	}
	if err := pool.QueryRow(ctx,
		"SELECT setting, source FROM pg_settings WHERE name = 'lock_timeout'").
		Scan(&setting, &source); err != nil {
		t.Fatalf("read lock_timeout: %v", err)
	}
	if want := millisStr(pg.Migration.LockTimeout); setting != want {
		t.Errorf("the migration lock_timeout is %s ms, want %s ms — a queued ALTER TABLE takes every "+
			"later reader down with it, because Postgres's lock queue is FIFO", setting, want)
	}
}

// millisStr renders a duration the way pg_settings reports these three GUCs:
// an integer count of milliseconds, with 0 meaning explicitly unbounded.
func millisStr(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

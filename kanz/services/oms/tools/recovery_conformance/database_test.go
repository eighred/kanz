package recoveryconformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

type fixture struct {
	t              *testing.T
	ctx            context.Context
	schema, tenant string
	postgresURL    string
	pool           *pgxpool.Pool
}

func newFixture(t *testing.T) *fixture { return newFixtureWithin(t, 180*time.Second) }
func newFixtureWithin(t *testing.T, lifetime time.Duration) *fixture {
	return newFixtureAt(t, lifetime, os.Getenv("TEST_POSTGRES_URL"))
}
func newFixtureAt(t *testing.T, lifetime time.Duration, postgresURL string) *fixture {
	if lifetime <= 0 || lifetime > 15*time.Minute {
		t.Fatal("fixture lifetime outside qualification bound")
	}
	t.Helper()
	binary(t)
	if postgresURL == "" {
		t.Skip("real non-bypass PostgreSQL unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	t.Cleanup(cancel)
	parsed, e := pgxpool.ParseConfig(postgresURL)
	if e != nil {
		t.Fatal("database configuration")
	}
	loopback := func(host string) bool {
		return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
	}
	if !loopback(parsed.ConnConfig.Host) {
		t.Fatal("disposable loopback PostgreSQL required")
	}
	for _, fallback := range parsed.ConnConfig.Fallbacks {
		if !loopback(fallback.Host) {
			t.Fatal("nonloopback PostgreSQL fallback refused")
		}
	}
	admin, e := pgxpool.NewWithConfig(ctx, parsed)
	if e != nil {
		t.Fatal("database configuration")
	}
	t.Cleanup(admin.Close)
	var super, bypass bool
	if e = admin.QueryRow(ctx, "SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&super, &bypass); e != nil || super || bypass {
		t.Fatal("NOSUPERUSER NOBYPASSRLS required")
	}
	t.Log("actual PostgreSQL role: nosuperuser=true nobypassrls=true")
	var nonce [12]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		t.Fatal(e)
	}
	schema := "oms_recovery_" + hex.EncodeToString(nonce[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+quoted); e != nil {
		t.Fatal("create schema")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, e := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); e != nil {
			t.Error("cleanup isolated schema")
		}
	})
	f := &fixture{t: t, ctx: ctx, schema: schema, tenant: "recovery-tenant", postgresURL: postgresURL}
	f.pool = f.open(f.tenant)
	files, e := filepath.Glob("../../migrations/*.sql")
	if e != nil || len(files) == 0 {
		t.Fatal("missing canonical migrations")
	}
	sort.Strings(files)
	for _, file := range files {
		ddl, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.pool.Exec(ctx, string(ddl)); e != nil {
			t.Fatalf("canonical migration %s: %v", filepath.Base(file), e)
		}
	}
	var forced int
	if e = f.pool.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relname IN ('orders','order_fills','order_executions','outbox','execution_fee_revisions','execution_account_mappings','execution_recovery_cases','execution_recovery_targets','execution_recovery_acks','execution_recovery_history','execution_fee_proposals','execution_fee_approvals') AND relrowsecurity AND relforcerowsecurity").Scan(&forced); e != nil || forced != 12 {
		t.Fatal("all twelve recovery owner tables require forced RLS")
	}
	t.Log("actual PostgreSQL recovery owner tables: forced_rls=12 expected=12")
	f.ensureBroker(t)
	return f
}
func (f *fixture) open(tenant string) *pgxpool.Pool {
	f.t.Helper()
	cfg, e := pgxpool.ParseConfig(f.postgresURL)
	if e != nil {
		f.t.Fatal("database configuration")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = f.schema
	cfg.ConnConfig.RuntimeParams["app.tenant_id"] = tenant
	p, e := pgxpool.NewWithConfig(f.ctx, cfg)
	if e != nil {
		f.t.Fatal("scoped pool")
	}
	f.t.Cleanup(p.Close)
	return p
}

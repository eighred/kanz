package projection

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This fixture uses administrative authority only to create disposable roles and
// a private schema. Migration and projection connections authenticate separately;
// neither is superuser, bypasses RLS, or inherits the administrative principal.
func TestTokyoTVSyncSeparateRolesAndRecovery(t *testing.T) {
	url := os.Getenv("TV_SYNC_TEST_ADMIN_URL")
	if url == "" {
		t.Skip("set TV_SYNC_TEST_ADMIN_URL for disposable role qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal("connect fixture administrator")
	}
	defer func() { _ = admin.Close(context.Background()) }()
	stem := fmt.Sprintf("tv_roles_%d", time.Now().UnixNano())
	migrationRole, appRole := stem+"_migrate", stem+"_app"
	quote := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	password := stem + "_disposable_only"
	for _, role := range []string{migrationRole, appRole} {
		_, err = admin.Exec(ctx, "CREATE ROLE "+quote(role)+" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD '"+password+"'")
		if err != nil {
			t.Fatal("create disposable constrained role")
		}
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			c, e := pgx.Connect(cleanup, url)
			if e != nil {
				t.Error("cleanup connection failed")
				return
			}
			defer func() { _ = c.Close(cleanup) }()
			if _, e = c.Exec(cleanup, "DROP OWNED BY "+quote(role)+"; DROP ROLE "+quote(role)); e != nil {
				t.Error("disposable role cleanup failed")
			}
		})
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quote(stem)+" AUTHORIZATION "+quote(migrationRole)); err != nil {
		t.Fatal("create private migration schema")
	}
	connect := func(role, tenant string) *pgxpool.Pool {
		t.Helper()
		cfg, e := pgxpool.ParseConfig(url)
		if e != nil {
			t.Fatal("parse fixture connection")
		}
		cfg.ConnConfig.User, cfg.ConnConfig.Password = role, password
		cfg.ConnConfig.RuntimeParams["search_path"] = stem
		cfg.ConnConfig.RuntimeParams["app.tenant_id"] = tenant
		cfg.MaxConns = 2
		p, e := pgxpool.NewWithConfig(ctx, cfg)
		if e != nil {
			t.Fatal("configure constrained connection")
		}
		t.Cleanup(p.Close)
		var safe bool
		if e = p.QueryRow(ctx, "SELECT NOT rolsuper AND NOT rolbypassrls AND NOT rolcreatedb AND NOT rolcreaterole FROM pg_roles WHERE rolname=current_user").Scan(&safe); e != nil || !safe {
			t.Fatal("constrained principal not verified")
		}
		return p
	}
	migrate := connect(migrationRole, testTenant)
	// These are the Tokyo provisioner's existing-object and future-object grants,
	// narrowed to the disposable schema. Future grants must exist before migration.
	grant := "GRANT USAGE ON SCHEMA " + quote(stem) + " TO " + quote(appRole) + "; " +
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + quote(migrationRole) + " IN SCHEMA " + quote(stem) + " GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + quote(appRole) + "; " +
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + quote(migrationRole) + " IN SCHEMA " + quote(stem) + " GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO " + quote(appRole)
	if _, err = migrate.Exec(ctx, grant); err != nil {
		t.Fatal("grant future application access")
	}
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) != 2 {
		t.Fatal("expected both fact and checkpoint migrations")
	}
	sort.Strings(files)
	for _, name := range files {
		ddl, e := os.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = migrate.Exec(ctx, string(ddl)); e != nil {
			t.Fatalf("migration %s failed: %v", filepath.Base(name), e)
		}
	}
	app := connect(appRole, testTenant)
	t.Run("RuntimeCannotDDL", func(t *testing.T) {
		if _, e := app.Exec(ctx, "CREATE TABLE forbidden(id integer)"); e == nil {
			t.Fatal("runtime can create tables")
		}
		if _, e := app.Exec(ctx, "ALTER TABLE tv_facts DISABLE ROW LEVEL SECURITY"); e == nil {
			t.Fatal("runtime can disable RLS")
		}
	})
	t.Run("FutureTableGrant", func(t *testing.T) {
		if _, e := migrate.Exec(ctx, "CREATE TABLE future_grant_probe(id bigserial PRIMARY KEY, value text)"); e != nil {
			t.Fatal(e)
		}
		if _, e := app.Exec(ctx, "INSERT INTO future_grant_probe(value) VALUES ('synthetic')"); e != nil {
			t.Fatal("future table/sequence grant absent")
		}
	})
	log := NewPostgresLog(app)
	// The fact log's knowledge timestamps use PostgreSQL microsecond precision.
	// Keep this role/recovery fixture representable; the existing checkpoint
	// suite separately qualifies sub-microsecond as-of replay boundaries.
	fixtureTime := time.Unix(1_800_000_000, 123_456_000).UTC()
	clock := func() time.Time { return fixtureTime }
	live := New(clock, nil, WithLog(log, testTenant))
	deliver(t, live, "role-1", evtFilled, filledOrder("role-o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100))
	if err = live.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	fixtureTime = fixtureTime.Add(time.Second)
	deliver(t, live, "role-2", evtFilled, filledOrder("role-o2", "fund-alpha", "BTC", orderpb.Side_SIDE_SELL, 1, 150))
	t.Run("CheckpointAndTail", func(t *testing.T) {
		// New authenticated pool represents a successor process, not reused memory.
		restarted := New(time.Now, nil, WithLog(NewPostgresLog(connect(appRole, testTenant)), testTenant))
		if e := restarted.Rehydrate(ctx); e != nil {
			t.Fatal(e)
		}
		if !restarted.BootedFromCheckpoint() {
			t.Fatal("checkpoint path was not exercised")
		}
		assertSameView(t, live, restarted)
		full := New(time.Now, nil, WithLog(log, testTenant))
		if e := full.replayEverythingForTest(ctx); e != nil {
			t.Fatal(e)
		}
		assertSameView(t, full, restarted)
	})
	t.Run("TenantIsolation", func(t *testing.T) {
		other := connect(appRole, "other-tenant")
		for _, table := range []string{"tv_facts", "tv_checkpoints"} {
			var n int
			if e := other.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 0 {
				t.Fatal("cross-tenant rows visible")
			}
			var forced bool
			if e := app.QueryRow(ctx, "SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid=$1::regclass", table).Scan(&forced); e != nil || !forced {
				t.Fatal("FORCE RLS missing")
			}
		}
		if _, e := other.Exec(ctx, "INSERT INTO tv_checkpoints(tenant_id,seq,payload) VALUES ($1,1,$2)", testTenant, []byte{1}); e == nil {
			t.Fatal("cross-tenant checkpoint write accepted")
		}
		unscoped := connect(appRole, "")
		var n int
		if e := unscoped.QueryRow(ctx, "SELECT count(*) FROM tv_facts").Scan(&n); e == nil {
			t.Fatal("missing tenant scope accepted")
		}
	})
}

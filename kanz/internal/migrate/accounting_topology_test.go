package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestLoadRejectsInvalidPreflight(t *testing.T) {
	for _, tt := range []struct{ name, file, sql, want string }{
		{"orphan", "0002_wrong.sql.preflight", "SELECT 1", "no matching migration"},
		{"empty", "0001_table.sql.preflight", " \n", "empty preflight"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeMigrations(t, map[string]string{
				"0001_table.sql": "CREATE TABLE x(id int)", tt.file: tt.sql,
			})
			if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPreflightIsReadOnly(t *testing.T) {
	pool := newPool(t)
	dir := writeMigrations(t, map[string]string{
		"0001_table.sql":           "CREATE TABLE x(id int)",
		"0001_table.sql.preflight": "CREATE TABLE premature(id int)",
	})
	migs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := New(pool).Up(context.Background(), migs)
	var pgErr *pgconn.PgError
	if len(applied) != 0 || !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("expected a read-only SQL transaction, applied=%d err=%v", len(applied), err)
	}
}

func TestAccountingBootstrapTopology(t *testing.T) {
	// Other packages legitimately leave a public accounting ledger in the
	// shared CI database. Use a separate database with the same restricted role.
	dsn := os.Getenv("TEST_ACCOUNTING_MIGRATION_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_ACCOUNTING_MIGRATION_POSTGRES_URL to a disposable dedicated database")
	}
	t.Setenv("TEST_POSTGRES_URL", dsn)
	target, peer := newPool(t), newPool(t)
	ctx := context.Background()
	var restricted bool
	if err := target.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolbypassrls
		FROM pg_roles WHERE rolname = current_user`).Scan(&restricted); err != nil || !restricted {
		t.Fatalf("expected restricted migration role, restricted=%v err=%v", restricted, err)
	}
	migrations, err := Load(filepath.Join("..", "..", "services", "accounting", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	runner := New(target)
	addPeer := func() {
		t.Helper()
		if _, err := peer.Exec(ctx, `CREATE TABLE ledger_entries(id int);
			ALTER TABLE ledger_entries ENABLE ROW LEVEL SECURITY;
			CREATE POLICY peer_policy ON ledger_entries USING (id > 0)`); err != nil {
			t.Fatal(err)
		}
	}
	removePeer := func() {
		t.Helper()
		if _, err := peer.Exec(ctx, `DROP TABLE ledger_entries`); err != nil {
			t.Fatal(err)
		}
	}
	refusesPeer := func() {
		t.Helper()
		applied, err := runner.Up(ctx, migrations)
		var pgErr *pgconn.PgError
		if len(applied) != 0 || !errors.As(err, &pgErr) || pgErr.Code != "55000" {
			t.Fatalf("expected topology refusal before applying migrations, applied=%d err=%v", len(applied), err)
		}
		var preserved bool
		if err := peer.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_policies
			WHERE schemaname=current_schema() AND tablename='ledger_entries'
			  AND policyname='peer_policy' AND qual='(id > 0)')`).Scan(&preserved); err != nil || !preserved {
			t.Fatalf("peer policy preserved=%v err=%v", preserved, err)
		}
	}

	addPeer()
	refusesPeer()
	var fresh bool
	if err := target.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NULL AND
		to_regclass('ledger_entries') IS NULL`).Scan(&fresh); err != nil || !fresh {
		t.Fatalf("fresh schema unchanged=%v err=%v", fresh, err)
	}
	removePeer()
	applied, err := runner.Up(ctx, migrations)
	if err != nil || len(applied) != len(migrations) {
		t.Fatalf("fresh accounting database applied %d/%d migrations: %v", len(applied), len(migrations), err)
	}
	addPeer()
	refusesPeer()
	removePeer()
	if applied, err := runner.Up(ctx, migrations); err != nil || len(applied) != 0 {
		t.Fatalf("already-applied accounting migrations replayed=%d err=%v", len(applied), err)
	}
}

func TestExistingAccountingMigrationsKeepTheirChecksums(t *testing.T) {
	dsn := os.Getenv("TEST_ACCOUNTING_MIGRATION_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_ACCOUNTING_MIGRATION_POSTGRES_URL to a disposable dedicated database")
	}
	t.Setenv("TEST_POSTGRES_URL", dsn)
	target := newPool(t)
	migrations, err := Load(filepath.Join("..", "..", "services", "accounting", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the installation that applied 0001–0003 before the preflight
	// shipped. Preserve its original version, name, SQL and checksum.
	var historical []Migration
	for _, m := range migrations {
		if m.Version <= 3 {
			m.Preflight = ""
			historical = append(historical, m)
		}
	}
	ctx := context.Background()
	runner := New(target)
	if applied, err := runner.Up(ctx, historical); err != nil || len(applied) != 3 {
		t.Fatalf("historical installation applied=%d err=%v", len(applied), err)
	}
	applied, err := runner.Up(ctx, migrations)
	if err != nil || len(applied) != len(migrations)-len(historical) {
		t.Fatalf("upgrade applied=%d err=%v", len(applied), err)
	}
	for _, m := range historical {
		var intact bool
		err := target.QueryRow(ctx, `SELECT EXISTS(SELECT FROM schema_migrations
			WHERE set_name='' AND version=$1 AND name=$2 AND checksum=$3)`, m.Version, m.Name, m.Checksum).Scan(&intact)
		if err != nil || !intact {
			t.Fatalf("historical %s checksum preserved=%v err=%v", m.Name, intact, err)
		}
	}
}

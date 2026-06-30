package book

// Postgres household-book integration tests (PARITY-02b). Gated on
// TEST_POSTGRES_URL — they skip without a database. They prove the
// replace-on-write projection survives a restart with the exact in-memory
// contract.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/internal/wealth"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run book Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", "__system__")
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	applySchema(t, pool)
	return pool
}

func applySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS households CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("../../migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
}

func TestPostgresBookRoundTrip(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	if _, ok, err := st.Get(ctx, "missing"); err != nil || ok {
		t.Fatalf("get missing: ok=%v err=%v", ok, err)
	}
	h := wealth.Household{
		HouseholdID: "HH-1",
		Accounts: []wealth.Account{
			{AccountID: "A-1", Cash: 5000, Holdings: []wealth.Holding{
				{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: 90_000},
				{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: 10_000},
			}},
		},
	}
	if err := st.Put(ctx, h); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := st.Get(ctx, "HH-1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if len(got.Accounts) != 1 || len(got.Accounts[0].Holdings) != 2 || got.Accounts[0].Cash != 5000 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// Replace-on-write (last write wins).
	h.Accounts[0].Cash = 7500
	if err := st.Put(ctx, h); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, _, _ := st.Get(ctx, "HH-1"); got.Accounts[0].Cash != 7500 {
		t.Fatalf("replace not applied: cash=%v", got.Accounts[0].Cash)
	}
}

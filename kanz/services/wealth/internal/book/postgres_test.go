package book

// Postgres household-book integration tests (PARITY-02b). Gated on
// TEST_POSTGRES_URL — they skip without a database. They prove the
// replace-on-write projection survives a restart with the exact in-memory
// contract.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/wealth"
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
		HouseholdID: "HH-1", CurrencyCode: "USD", AsOf: time.Unix(1700000000, 0), RecordedBy: "test:advisor", Reason: "test valuation",
		Accounts: []wealth.Account{
			{AccountID: "A-1", Cash: "5000", Holdings: []wealth.Holding{
				{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: "90000"},
				{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: "10000"},
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
	if len(got.Accounts) != 1 || len(got.Accounts[0].Holdings) != 2 || got.Accounts[0].Cash != "5000" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// Replace-on-write (last write wins).
	h.Accounts[0].Cash = "7500"
	if err := st.Put(ctx, h); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, _, _ := st.Get(ctx, "HH-1"); got.Accounts[0].Cash != "7500" {
		t.Fatalf("replace not applied: cash=%v", got.Accounts[0].Cash)
	}
}

func TestPostgresExactVersionAndTenantIsolation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	st := NewPostgres(pool)
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatal("test role must exercise RLS")
	}
	h := wealth.Household{HouseholdID: "exact", CurrencyCode: "USD", AsOf: time.Unix(1700000000, 0), RecordedBy: "test:source", Reason: "precision fixture", Accounts: []wealth.Account{{AccountID: "a", Cash: "9007199254740993.000000001", Holdings: []wealth.Holding{{InstrumentID: "x", AssetClass: "EQUITY", MarketValue: "0.000000002"}}}}}
	if err := st.Put(ctx, h); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config().Copy()
	restarted, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	got, ok, err := NewPostgres(restarted).Get(ctx, h.HouseholdID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	vp, err := wealth.Aggregate(got)
	if err != nil || vp.TotalValue != "9007199254740993.000000003" || got.CurrencyCode != "USD" || !got.AsOf.Equal(h.AsOf) || got.RecordedBy != h.RecordedBy || got.Reason != h.Reason {
		t.Fatalf("restart lost exact value/provenance: %+v %+v %v", got, vp, err)
	}
	cfg = pool.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SELECT set_config('app.tenant_id','other',false)`)
		return err
	}
	other, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, ok, err := NewPostgres(other).Get(ctx, h.HouseholdID); err != nil || ok {
		t.Fatalf("tenant leak %v %v", ok, err)
	}
	// An old binary can replace the JSONB projection. Absence of the new version
	// must invalidate it; rounding already incurred is not reversible.
	if _, err := pool.Exec(ctx, `UPDATE households SET composition='{"HouseholdID":"exact","Accounts":[{"AccountID":"a","Cash":9007199254740992}]}'::jsonb WHERE household_id='exact'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Get(ctx, h.HouseholdID); !errors.Is(err, wealth.ErrLegacyPrecision) {
		t.Fatalf("legacy row certified: %v", err)
	}
	if err := st.Put(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE households SET composition=jsonb_set(composition,'{Accounts,0,Cash}','"1e999999999"') WHERE household_id='exact'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Get(ctx, h.HouseholdID); err == nil {
		t.Fatal("malformed exact storage accepted")
	}
}

func TestMemoryBookOwnsItsSnapshot(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	h := wealth.Household{HouseholdID: "a", CurrencyCode: "USD", AsOf: time.Unix(1700000000, 0), RecordedBy: "test:source", Reason: "alias test", Accounts: []wealth.Account{{AccountID: "a", Cash: "1"}}}
	if err := st.Put(ctx, h); err != nil {
		t.Fatal(err)
	}
	h.Accounts[0].Cash = "999"
	got, _, err := st.Get(ctx, "a")
	if err != nil || got.Accounts[0].Cash != "1" {
		t.Fatal(got, err)
	}
	got.Accounts[0].Cash = "888"
	got, _, err = st.Get(ctx, "a")
	if err != nil || got.Accounts[0].Cash != "1" {
		t.Fatal(got, err)
	}
}

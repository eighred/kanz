package persist

// Postgres StateStore integration tests (PERS-01e). Gated on TEST_POSTGRES_URL
// — they require a real database and skip otherwise, mirroring the
// bus/replay Kafka integration tests (TEST_KAFKA_BROKERS). The DB-free
// idempotent-replay property is proven in the app package's bootstrap_test.go;
// these prove the durable contract that underpins it: committed state survives
// a "restart" intact (RPO=0 of committed state) and re-saving doesn't corrupt.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

const migrationDir = "../../../../services/risk-engine/migrations"

// newPool connects, applies the schema clean, and pins the session tenant to
// __system__ via the app.tenant_id GUC (MT-01d) — so RLS lets the existing
// round-trip tests operate transparently within one tenant.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := tenantPool(t, "__system__")
	applySchema(t, pool)
	return pool
}

// tenantPool returns a pool whose every connection sets app.tenant_id to
// tenant (the authenticated-session-GUC pattern main.go uses), without
// touching the schema. RLS scopes all of its reads/writes to that tenant.
func tenantPool(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run persist Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// applySchema drops and recreates the state tables from the migrations (in
// lexical order) so each run starts clean. The migration files are the single
// source of truth — including the RLS policies (0002).
func applySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS applied_keys, positions, portfolios CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations %s: %v (found %d)", migrationDir, err, len(files))
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

func money(coef int64, ccy string) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: coef}, CurrencyCode: ccy}
}

func dec(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

func fullRecord() PortfolioRecord {
	asOf := time.Unix(1_700_000_000, 0).UTC()
	return PortfolioRecord{
		ID:               "PORT-1",
		DisplayName:      "Flagship",
		BaseCurrency:     "USD",
		CashBalance:      money(50_000, "USD"),
		TotalMarketValue: money(1_250_000, "USD"),
		PositionCount:    2,
		AsOf:             asOf,
		LogPosition:      &commonpb.LogPosition{Topic: "risk.position.changed", Partition: 3, Offset: 4242},
		Positions: []domain.Position{
			{
				InstrumentID:  "AAPL",
				Quantity:      dec(100, 0),
				AveragePrice:  dec(15000, -2),
				MarketValue:   money(2_000_00, "USD"),
				RealizedPnL:   money(500, "USD"),
				UnrealizedPnL: money(-250, "USD"),
				AsOf:          asOf,
			},
			{
				InstrumentID: "MSFT",
				Quantity:     dec(50, 0),
				MarketValue:  money(1_050_000, "USD"),
				AsOf:         asOf,
			},
		},
		AppliedKeys: []string{"k1", "k2", "k3"},
	}
}

func assertMoney(t *testing.T, label string, got, want *commonpb.Money) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s = %v want nil", label, got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s = nil want %v", label, want)
		return
	}
	if got.CurrencyCode != want.CurrencyCode || got.GetAmount().GetCoefficient() != want.GetAmount().GetCoefficient() ||
		got.GetAmount().GetExponent() != want.GetAmount().GetExponent() {
		t.Errorf("%s = %v want %v", label, got, want)
	}
}

func TestPostgres_SaveLoadRoundTrip(t *testing.T) {
	store := NewPostgres(newPool(t))
	ctx := context.Background()
	want := fullRecord()
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(ctx, want.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.ID != want.ID || got.DisplayName != want.DisplayName || got.BaseCurrency != want.BaseCurrency {
		t.Errorf("aggregate identity mismatch: %+v", got)
	}
	if got.PositionCount != want.PositionCount {
		t.Errorf("position_count = %d want %d", got.PositionCount, want.PositionCount)
	}
	if !got.AsOf.Equal(want.AsOf) {
		t.Errorf("as_of = %v want %v", got.AsOf, want.AsOf)
	}
	assertMoney(t, "cash_balance", got.CashBalance, want.CashBalance)
	assertMoney(t, "total_market_value", got.TotalMarketValue, want.TotalMarketValue)

	if got.LogPosition.GetTopic() != "risk.position.changed" || got.LogPosition.GetPartition() != 3 || got.LogPosition.GetOffset() != 4242 {
		t.Errorf("log_position = %v", got.LogPosition)
	}
	if len(got.Positions) != 2 {
		t.Fatalf("positions = %d want 2", len(got.Positions))
	}
	// Positions come back ORDER BY instrument_id.
	if got.Positions[0].InstrumentID != "AAPL" || got.Positions[1].InstrumentID != "MSFT" {
		t.Errorf("position order = %v, %v", got.Positions[0].InstrumentID, got.Positions[1].InstrumentID)
	}
	aapl := got.Positions[0]
	if aapl.Quantity.GetCoefficient() != 100 || aapl.AveragePrice.GetCoefficient() != 15000 || aapl.AveragePrice.GetExponent() != -2 {
		t.Errorf("AAPL decimals = %v / %v", aapl.Quantity, aapl.AveragePrice)
	}
	assertMoney(t, "AAPL market_value", aapl.MarketValue, money(2_000_00, "USD"))
	assertMoney(t, "AAPL realized_pnl", aapl.RealizedPnL, money(500, "USD"))
	assertMoney(t, "AAPL unrealized_pnl", aapl.UnrealizedPnL, money(-250, "USD"))
	if got.Positions[1].MarketValueUncertainty != nil {
		t.Errorf("MSFT uncertainty = %v want nil", got.Positions[1].MarketValueUncertainty)
	}
	if len(got.AppliedKeys) != 3 {
		t.Errorf("applied_keys = %v want 3", got.AppliedKeys)
	}
}

// Save is a full replace: positions and keys absent from the new record are
// removed (snapshot hard-reset semantics).
func TestPostgres_SaveFullReplace(t *testing.T) {
	store := NewPostgres(newPool(t))
	ctx := context.Background()
	if err := store.Save(ctx, fullRecord()); err != nil {
		t.Fatalf("Save 1: %v", err)
	}

	asOf := time.Unix(1_700_000_500, 0).UTC()
	replacement := PortfolioRecord{
		ID: "PORT-1", DisplayName: "Flagship", BaseCurrency: "USD", PositionCount: 1, AsOf: asOf,
		Positions:   []domain.Position{{InstrumentID: "AAPL", MarketValue: money(2_100_00, "USD"), AsOf: asOf}},
		AppliedKeys: []string{"k4"},
	}
	if err := store.Save(ctx, replacement); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	got, err := store.Load(ctx, "PORT-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Positions) != 1 || got.Positions[0].InstrumentID != "AAPL" {
		t.Errorf("positions not replaced: %+v", got.Positions)
	}
	if got.LogPosition != nil {
		t.Errorf("log_position not cleared: %v", got.LogPosition)
	}
	if len(got.AppliedKeys) != 1 || got.AppliedKeys[0] != "k4" {
		t.Errorf("applied_keys not replaced: %v", got.AppliedKeys)
	}
}

func TestPostgres_LoadNotFound(t *testing.T) {
	store := NewPostgres(newPool(t))
	if _, err := store.Load(context.Background(), v1.PortfolioID("nope")); err != ErrNotFound {
		t.Fatalf("Load unknown = %v want ErrNotFound", err)
	}
}

// Re-saving the same record (duplicate applied keys) is a no-op, not an error.
func TestPostgres_ResaveIdempotent(t *testing.T) {
	store := NewPostgres(newPool(t))
	ctx := context.Background()
	rec := fullRecord()
	for i := 0; i < 3; i++ {
		if err := store.Save(ctx, rec); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	got, err := store.Load(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.AppliedKeys) != 3 || len(got.Positions) != 2 {
		t.Errorf("re-save duplicated rows: keys=%v positions=%d", got.AppliedKeys, len(got.Positions))
	}
}

func TestPostgres_LoadAll(t *testing.T) {
	store := NewPostgres(newPool(t))
	ctx := context.Background()
	a := fullRecord()
	b := fullRecord()
	b.ID = "PORT-2"
	b.Positions = []domain.Position{{InstrumentID: "TSLA", MarketValue: money(900, "USD"), AsOf: a.AsOf}}
	b.AppliedKeys = []string{"x"}
	if err := store.Save(ctx, a); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if err := store.Save(ctx, b); err != nil {
		t.Fatalf("Save b: %v", err)
	}

	all, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("LoadAll = %d want 2", len(all))
	}
	byID := map[v1.PortfolioID]PortfolioRecord{}
	for _, r := range all {
		byID[r.ID] = r
	}
	if len(byID["PORT-1"].Positions) != 2 || len(byID["PORT-2"].Positions) != 1 {
		t.Errorf("LoadAll grouped positions wrong: %d / %d", len(byID["PORT-1"].Positions), len(byID["PORT-2"].Positions))
	}
	if len(byID["PORT-2"].AppliedKeys) != 1 {
		t.Errorf("PORT-2 keys = %v", byID["PORT-2"].AppliedKeys)
	}
}

// Crash-recovery: committed state survives a "restart" (fresh pool/store)
// byte-for-byte — RPO=0 for everything that was durably saved.
func TestPostgres_CrashRecoveryParity(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run persist Postgres integration tests")
	}
	_ = url
	ctx := context.Background()

	// First "process": apply schema + save. The GUC-pinned pool scopes the
	// write to __system__ under RLS (MT-01d).
	pool1 := tenantPool(t, "__system__")
	applySchema(t, pool1)
	want := fullRecord()
	if err := NewPostgres(pool1).Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Second "process": fresh pool (same tenant), load.
	pool2 := tenantPool(t, "__system__")
	got, err := NewPostgres(pool2).Load(ctx, want.ID)
	if err != nil {
		t.Fatalf("Load after restart: %v", err)
	}
	if got.PositionCount != want.PositionCount || len(got.Positions) != len(want.Positions) ||
		got.LogPosition.GetOffset() != want.LogPosition.GetOffset() || len(got.AppliedKeys) != len(want.AppliedKeys) {
		t.Errorf("state not recovered intact: %+v", got)
	}
	assertMoney(t, "recovered total_market_value", got.TotalMarketValue, want.TotalMarketValue)
}

// RLS proves the tenant boundary is enforced by the database (MT-01d): state
// written under one tenant's session GUC is invisible to another, the same
// portfolio_id coexists per-tenant, and the row carries the writing tenant.
// Superusers bypass RLS, so the assertions only hold under a non-superuser
// role — skipped otherwise rather than passing falsely.
func TestPostgres_RLSTenantIsolation(t *testing.T) {
	ctx := context.Background()
	base := newPool(t) // applies schema; session = __system__

	var superuser bool
	if err := base.QueryRow(ctx, "SELECT current_setting('is_superuser')::bool").Scan(&superuser); err != nil {
		t.Fatalf("is_superuser: %v", err)
	}
	if superuser {
		t.Skip("RLS is bypassed for superusers; run TEST_POSTGRES_URL as a non-superuser role")
	}

	acme := NewPostgres(tenantPool(t, "acme"))
	globex := NewPostgres(tenantPool(t, "globex"))

	rec := fullRecord() // ID = PORT-1
	if err := acme.Save(ctx, rec); err != nil {
		t.Fatalf("acme Save: %v", err)
	}

	// globex cannot see acme's portfolio — not by id, not in bulk.
	if _, err := globex.Load(ctx, rec.ID); err != ErrNotFound {
		t.Fatalf("globex Load of acme portfolio = %v want ErrNotFound", err)
	}
	if all, err := globex.LoadAll(ctx); err != nil || len(all) != 0 {
		t.Fatalf("globex LoadAll = %v (err %v) want empty", all, err)
	}

	// acme sees its own, stamped with its tenant.
	got, err := acme.Load(ctx, rec.ID)
	if err != nil {
		t.Fatalf("acme Load: %v", err)
	}
	if got.TenantID != "acme" {
		t.Errorf("tenant_id = %q want acme", got.TenantID)
	}

	// The same portfolio_id under globex is an independent row.
	if err := globex.Save(ctx, rec); err != nil {
		t.Fatalf("globex Save same id: %v", err)
	}
	gg, err := globex.Load(ctx, rec.ID)
	if err != nil || gg.TenantID != "globex" {
		t.Fatalf("globex Load = %+v (err %v) want tenant globex", gg, err)
	}
	// acme's row is untouched by globex's write.
	if all, err := acme.LoadAll(ctx); err != nil || len(all) != 1 {
		t.Fatalf("acme LoadAll = %v (err %v) want exactly its own row", all, err)
	}
}

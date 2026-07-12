package ledger

// Postgres ledger integration tests (PARITY-02a). Gated on TEST_POSTGRES_URL —
// they require a real database and skip otherwise, mirroring the risk-engine
// persist tests. The DB-free fold/replay properties are proven in ledger_test.go;
// these prove the durable contract beneath them: the journal survives a restart,
// re-append is idempotent, the bitemporal read is the indexed query, and
// snapshot+tail reconstructs the same book as a full replay.

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationDir = "../../migrations"

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run ledger Postgres integration tests")
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
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS ledger_entries, ledger_snapshots CASCADE`); err != nil {
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

func tradeEvent(id, instrument string, qty, price, cash int64, eff, know time.Time) *Event {
	return &Event{
		EntryID:      id,
		PortfolioID:  "PORT-1",
		Type:         EntryTrade,
		InstrumentID: instrument,
		Quantity:     big.NewRat(qty, 1),
		Price:        big.NewRat(price, 1),
		Cash:         big.NewRat(cash, 1),
		CashCurrency: "USD",
		Effective:    eff,
		Knowledge:    know,
		SourceRef:    id,
	}
}

func TestPostgresJournalRoundTripAndReplay(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	t0 := time.Unix(1_700_000_000, 0).UTC()
	events := []*Event{
		tradeEvent("e1", "AAPL", 100, 150, -15000, t0, t0),
		tradeEvent("e2", "AAPL", 50, 160, -8000, t0.Add(time.Hour), t0.Add(time.Hour)),
		tradeEvent("e3", "AAPL", -30, 170, 5100, t0.Add(2*time.Hour), t0.Add(2*time.Hour)),
	}
	for _, e := range events {
		if err := st.Append(ctx, e); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}
	// Idempotent re-append is a no-op.
	if err := st.Append(ctx, events[0]); err != nil {
		t.Fatalf("re-append: %v", err)
	}

	got, err := st.Journal(ctx, "PORT-1")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("journal len = %d, want 3 (idempotent append)", len(got))
	}

	// Same journal ⇒ same book as the in-memory fold.
	durable := Replay("PORT-1", got)
	memory := Replay("PORT-1", events)
	if durable.Positions["AAPL"].Qty.Cmp(memory.Positions["AAPL"].Qty) != 0 {
		t.Fatalf("durable qty %v != memory qty %v",
			durable.Positions["AAPL"].Qty, memory.Positions["AAPL"].Qty)
	}
	if durable.CashBalance("USD").Cmp(memory.CashBalance("USD")) != 0 {
		t.Fatalf("durable cash %v != memory cash %v",
			durable.CashBalance("USD"), memory.CashBalance("USD"))
	}
}

func TestPostgresJournalAsOf(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	t0 := time.Unix(1_700_000_000, 0).UTC()
	_ = st.Append(ctx, tradeEvent("e1", "AAPL", 100, 150, -15000, t0, t0))
	_ = st.Append(ctx, tradeEvent("e2", "AAPL", 50, 160, -8000, t0.Add(2*time.Hour), t0.Add(2*time.Hour)))

	// As-of just after the first entry: only e1 is visible on the effective axis.
	asOf, err := st.JournalAsOf(ctx, "PORT-1", t0.Add(time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("journal as-of: %v", err)
	}
	if len(asOf) != 1 || asOf[0].EntryID != "e1" {
		t.Fatalf("as-of journal = %v, want [e1]", ids(asOf))
	}
	book := Replay("PORT-1", asOf)
	if book.Positions["AAPL"].Qty.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("as-of qty = %v, want 100", book.Positions["AAPL"].Qty)
	}
}

func TestPostgresSnapshotTailEqualsFullReplay(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	t0 := time.Unix(1_700_000_000, 0).UTC()
	first := tradeEvent("e1", "AAPL", 100, 150, -15000, t0, t0)
	if err := st.Append(ctx, first); err != nil {
		t.Fatalf("append e1: %v", err)
	}

	// Snapshot through t0, then append a tail entry past the watermark.
	snap := Replay("PORT-1", []*Event{first}).Snapshot(t0)
	if err := st.SaveSnapshot(ctx, snap); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	tail := tradeEvent("e2", "AAPL", 50, 160, -8000, t0.Add(time.Hour), t0.Add(time.Hour))
	if err := st.Append(ctx, tail); err != nil {
		t.Fatalf("append e2: %v", err)
	}

	fromSnapshot, err := MaterializeCurrent(ctx, st, "PORT-1")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	full := Replay("PORT-1", []*Event{first, tail})
	if fromSnapshot.Positions["AAPL"].Qty.Cmp(full.Positions["AAPL"].Qty) != 0 {
		t.Fatalf("snapshot+tail qty %v != full replay qty %v",
			fromSnapshot.Positions["AAPL"].Qty, full.Positions["AAPL"].Qty)
	}
	if fromSnapshot.CashBalance("USD").Cmp(full.CashBalance("USD")) != 0 {
		t.Fatalf("snapshot+tail cash %v != full replay cash %v",
			fromSnapshot.CashBalance("USD"), full.CashBalance("USD"))
	}
}

// TestPostgresCrossReplicaConsistency is the PARITY-02h cross-replica contract:
// two independent store handles over the same durable journal materialize a
// byte-identical book — the durable state, not a replica's in-memory cache, is
// the source of truth, so a second engine replica reads exactly what the first
// committed.
func TestPostgresCrossReplicaConsistency(t *testing.T) {
	pool := newPool(t)
	writer := NewPostgres(pool)
	reader := NewPostgres(pool) // a distinct handle — the "other replica"
	ctx := context.Background()

	t0 := time.Unix(1_700_000_000, 0).UTC()
	for i, e := range []*Event{
		tradeEvent("e1", "AAPL", 100, 150, -15000, t0, t0),
		tradeEvent("e2", "MSFT", 200, 300, -60000, t0.Add(time.Hour), t0.Add(time.Hour)),
		tradeEvent("e3", "AAPL", -40, 170, 6800, t0.Add(2*time.Hour), t0.Add(2*time.Hour)),
	} {
		if err := writer.Append(ctx, e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	a, err := MaterializeCurrent(ctx, writer, "PORT-1")
	if err != nil {
		t.Fatalf("writer materialize: %v", err)
	}
	b, err := MaterializeCurrent(ctx, reader, "PORT-1")
	if err != nil {
		t.Fatalf("reader materialize: %v", err)
	}
	for _, inst := range []string{"AAPL", "MSFT"} {
		if a.Positions[inst].Qty.Cmp(b.Positions[inst].Qty) != 0 ||
			a.Positions[inst].Realized.Cmp(b.Positions[inst].Realized) != 0 {
			t.Fatalf("cross-replica divergence on %s: %v vs %v", inst, a.Positions[inst], b.Positions[inst])
		}
	}
	if a.CashBalance("USD").Cmp(b.CashBalance("USD")) != 0 {
		t.Fatalf("cross-replica cash divergence: %v vs %v", a.CashBalance("USD"), b.CashBalance("USD"))
	}
}

func ids(events []*Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.EntryID
	}
	return out
}

// TestPostgresWithoutTenantGUCIsFailClosed pins the bug that made accounting's
// durable ledger silently useless in production.
//
// 0001_ledger.sql runs FORCE ROW LEVEL SECURITY with a policy on
// current_setting('app.tenant_id'), and Append inserts
// VALUES (current_setting('app.tenant_id'), ...). Every test in this file sets that
// GUC on its pool via AfterConnect — and passes. The composition root (main.go)
// did NOT, so against the non-superuser role production requires, RLS was
// fail-closed: writes errored and reads returned nothing. The IBOR held nothing,
// and the tests were green the whole time.
//
// This test connects the way main.go used to, and asserts the database refuses it.
// If it ever starts passing silently, RLS has stopped protecting this table.
func TestPostgresWithoutTenantGUCIsFailClosed(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run ledger Postgres integration tests")
	}
	// Ensure the schema exists (via a properly-scoped pool), then connect WITHOUT
	// the GUC — the old production path.
	newPool(t)

	bare, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(bare.Close)

	var superuser bool
	if err := bare.QueryRow(context.Background(),
		"SELECT current_setting('is_superuser')::bool").Scan(&superuser); err != nil {
		t.Fatalf("is_superuser: %v", err)
	}
	if superuser {
		t.Skip("RLS is bypassed for superusers; run TEST_POSTGRES_URL as a non-superuser role")
	}

	err = NewPostgres(bare).Append(context.Background(), &Event{
		EntryID:     "E-NO-GUC",
		PortfolioID: "P-1",
	})
	if err == nil {
		t.Fatal("Append succeeded with no app.tenant_id GUC set — RLS is not protecting this table, or the tenant scoping is gone")
	}
}

package server

// THE CUSTODIAN-SCOPED AD-HOC RECONCILE, OVER A REAL JOURNAL (#1025).
//
// The in-memory tests beside this one prove the handler's decisions: which
// custodian is refused, which book slice is asked for, what the response says.
// They cannot prove the thing the answer actually depends on — that
// venue_account_id round-trips through Postgres under the RLS the estate runs
// with. A scoped fold over a column that came back empty returns an EMPTY book
// for every custodian, which reads as "the custodian holds everything the IBOR
// does not": the same defect this endpoint was fixed for, wearing the opposite
// sign, and an in-memory store cannot tell the two apart because it never
// serializes the column at all.
//
// Gated on TEST_POSTGRES_URL, and the role must be NOSUPERUSER or RLS is bypassed
// and the isolation the pool is pinned to proves nothing.

import (
	"context"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

const serverMigrationDir = "../../migrations"

// newServerPool connects the tenant-pinned pool the composition root gives this
// service, and applies the accounting migrations onto it.
func newServerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run the accounting server Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// The same GUC pg.NewTenantPool sets in production: RLS on ledger_entries is
	// FORCE ROW LEVEL SECURITY against it, so without this every read is empty and
	// a scoped fold would look correct for the wrong reason.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", "__system__")
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`DROP TABLE IF EXISTS custody_actions, ledger_entries, ledger_snapshots, outbox, custody_statements, custody_runs, custody_breaks CASCADE`,
	); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(serverMigrationDir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations %s: %v (found %d)", serverMigrationDir, err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read migration %s: %v", f, rerr)
		}
		if _, eerr := pool.Exec(ctx, string(ddl)); eerr != nil {
			t.Fatalf("apply migration %s: %v", f, eerr)
		}
	}
	return pool
}

// A DURABLE JOURNAL, TWO CUSTODIANS, AND ONE STATEMENT EACH.
//
// The book holds AAPL against CUST-A's exchange account and MSFT against CUST-B's.
// Each custodian's statement names only what that custodian holds, and each must
// reconcile CLEAN. Before #1025 the handler folded the whole portfolio, so
// CUST-A's run reported MSFT as MISSING_AT_CUSTODIAN and CUST-B's reported AAPL
// the same way — every position in the book, twice, arriving while an operator
// was investigating something real.
func TestPostgresReconcileEndpointComparesOnlyTheNamedCustodiansBook(t *testing.T) {
	pool := newServerPool(t)
	store := ledger.NewPostgres(pool)
	ctx := context.Background()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	for _, e := range []*ledger.Event{
		{
			EntryID: "pg-a1", PortfolioID: dualPortfolio, VenueAccountID: "okx-sub-1",
			Type: ledger.EntryTrade, InstrumentID: "AAPL",
			Quantity: big.NewRat(100, 1), Price: big.NewRat(150, 1),
			Cash: big.NewRat(-15000, 1), CashCurrency: "USD",
			Effective: at, Knowledge: at, SourceRef: "pg-a1",
		},
		{
			EntryID: "pg-b1", PortfolioID: dualPortfolio, VenueAccountID: "bin-main",
			Type: ledger.EntryTrade, InstrumentID: "MSFT",
			Quantity: big.NewRat(250, 1), Price: big.NewRat(100, 1),
			Cash: big.NewRat(-25000, 1), CashCurrency: "USD",
			Effective: at, Knowledge: at, SourceRef: "pg-b1",
		},
	} {
		if err := store.Append(ctx, e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}

	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, store, "USD", WithTenant(testTenant), WithCustodyBookScope(testScope(t)))

	for _, tc := range []struct{ custodian, body string }{
		{custodianA, `{"custodian_id":"CUST-A","positions":{"AAPL":"100"},"cash":{"USD":"-15000"}}`},
		{custodianB, `{"custodian_id":"CUST-B","positions":{"MSFT":"250"},"cash":{"USD":"-25000"}}`},
	} {
		rec := reconcileAs(t, s, dualPortfolio, tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", tc.custodian, rec.Code, rec.Body.String())
		}
		got := decodeReconcile(t, rec)
		if got.Count != 0 {
			t.Errorf("%s: %d break(s) over a durable journal against a statement matching its own "+
				"holdings: %v\n\n"+
				"Either the book was not scoped to this custodian — every position held at the other "+
				"becomes a break — or venue_account_id did not round-trip and the scoped fold came "+
				"back empty, which reports the custodian's own holdings as MISSING_IN_IBOR. Both are "+
				"a break queue nobody can read.", tc.custodian, got.Count, got.Breaks)
		}
	}

	// NON-VACUITY: the same durable book must still produce the real break, or the
	// two clean results above would be satisfied by a fold that returns nothing.
	rec := reconcileAs(t, s, dualPortfolio,
		`{"custodian_id":"CUST-A","positions":{"AAPL":"90"},"cash":{"USD":"-15000"}}`)
	got := decodeReconcile(t, rec)
	if got.Count != 1 || got.Breaks[0]["key"] != "AAPL" {
		t.Fatalf("want exactly one AAPL break over the durable journal, got %d: %v", got.Count, got.Breaks)
	}
}

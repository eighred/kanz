package custody

import (
	"context"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// THE TRANSACTION GRAIN, AGAINST A REAL DATABASE AND A REAL JOURNAL (#1049).
//
// The in-memory store hands back the Go values it was given, so a grain and a
// trade line survive it whatever the columns do. Only Postgres executes the
// INSERT, the JSONB round trip and the ON CONFLICT update — and only a real
// journal proves that SourceRef and the settlement axis survive the ledger's own
// columns, which is what the whole comparison is keyed on. A grain that came back
// as UNKNOWN through storage would switch the execution comparison off in every
// deployment that has one, permanently and quietly.

// TestPostgresStatementCarriesTheTransactionGrain: the lines and the grain
// round-trip exactly, and an empty list is still distinguishable from a
// balances-only feed after a restart.
func TestPostgresStatementCarriesTheTransactionGrain(t *testing.T) {
	st, ctx := newCustodyStore(t)

	// A quantity whose scaled coefficient exceeds float64's exact integer range,
	// so a column that round-tripped it through a JSON number would alter it.
	exact := new(big.Rat)
	exact.SetString("123456789.01234567")
	in := Statement{
		StatementID: "S-tx", CustodianID: "CUST-A", PortfolioID: "PF1",
		BusinessDate: BusinessDay(t0), ReceivedAt: t0,
		Positions: map[string]*big.Rat{"AAPL": exact},
		Grain:     recon.GrainTransactions,
		Transactions: []recon.Transaction{{
			ExternalRef: "fill-a", InstrumentID: "AAPL", Quantity: exact,
			Price: dec.Rat("150.5"), Cash: dec.Rat("-18583333.5"), CurrencyCode: "USD",
			TradeDate: t0, SettlementDate: t0.Add(48 * time.Hour),
		}},
	}
	if err := st.SaveStatement(ctx, in); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	got, err := st.LatestStatement(ctx, subject())
	if err != nil {
		t.Fatalf("LatestStatement: %v", err)
	}
	if got.Grain != recon.GrainTransactions {
		t.Fatalf("grain = %s, want %s. A trade-level feed read back as %s reconciles at the netted "+
			"grain forever, and nothing in the run says which it is", got.Grain, recon.GrainTransactions, got.Grain)
	}
	if len(got.Transactions) != 1 {
		t.Fatalf("got %d transaction(s), want 1", len(got.Transactions))
	}
	tx := got.Transactions[0]
	if tx.ExternalRef != "fill-a" {
		t.Errorf("external_ref = %q, want fill-a — it is the whole of the match", tx.ExternalRef)
	}
	if tx.Quantity.Cmp(exact) != 0 {
		t.Errorf("quantity = %s, want %s — precision lost through storage",
			tx.Quantity.RatString(), exact.RatString())
	}
	if !tx.SettlementDate.Equal(t0.Add(48 * time.Hour).UTC()) {
		t.Errorf("settlement_date = %s, want %s. It is the axis #1043 put on the journal and the one "+
			"the book side is windowed by; a date lost in storage puts the line in the wrong day",
			tx.SettlementDate, t0.Add(48*time.Hour).UTC())
	}

	// AN EMPTY LIST IS STILL NOT "NO TRANSACTIONS OCCURRED" AFTER A RESTART. The
	// grain is a column, never re-derived from whether the array is empty.
	balances := Statement{
		StatementID: "S-bal", CustodianID: "CUST-A", PortfolioID: "PF1",
		BusinessDate: BusinessDay(t0), ReceivedAt: t0.Add(time.Hour),
		Positions: map[string]*big.Rat{"AAPL": exact},
		Grain:     recon.GrainBalancesOnly,
	}
	if err := st.SaveStatement(ctx, balances); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	back, err := st.LatestStatement(ctx, subject())
	if err != nil {
		t.Fatalf("LatestStatement: %v", err)
	}
	if back.Grain != recon.GrainBalancesOnly || len(back.Transactions) != 0 {
		t.Fatalf("grain = %s with %d transaction(s), want %s with none",
			back.Grain, len(back.Transactions), recon.GrainBalancesOnly)
	}
}

// TestPostgresReconciliationNamesTheExecutionTheCustodianNeverSaw is the whole
// issue end to end over real storage: a real journal, a real statement, a real
// run, and the break persisted with its lifecycle.
//
// THE NETTED TOTALS AGREE EXACTLY. The book holds fill-a (+60) and fill-b (+40);
// the custodian settled fill-a and an execution the book has never heard of
// (venue-x9, +40). Both sides are +100 AAPL, so every balance-grain kind reports
// the line as fully correct — and the run must still find both executions.
func TestPostgresReconciliationNamesTheExecutionTheCustodianNeverSaw(t *testing.T) {
	pool := newCustodyPool(t, "__system__")
	applyCustodySchema(t, pool)
	ctx := context.Background()
	store := NewPostgres(pool)
	journal := ledger.NewPostgres(pool)

	settledAt := BusinessDay(t0)
	for _, e := range []*ledger.Event{
		{
			EntryID: "fill:fill-a", PortfolioID: "PF1", VenueAccountID: "okx-sub-1",
			Type: ledger.EntryTrade, InstrumentID: "AAPL",
			Quantity: big.NewRat(60, 1), Price: big.NewRat(1, 1),
			Cash: big.NewRat(-60, 1), CashCurrency: "USD",
			Effective: settledAt, Knowledge: settledAt,
			SettlementBasis: ledger.SettlementSettled, SettlementDate: settledAt,
			SourceRef: "fill-a",
		},
		{
			EntryID: "fill:fill-b", PortfolioID: "PF1", VenueAccountID: "okx-sub-1",
			Type: ledger.EntryTrade, InstrumentID: "AAPL",
			Quantity: big.NewRat(40, 1), Price: big.NewRat(1, 1),
			Cash: big.NewRat(-40, 1), CashCurrency: "USD",
			Effective: settledAt, Knowledge: settledAt,
			SettlementBasis: ledger.SettlementSettled, SettlementDate: settledAt,
			SourceRef: "fill-b",
		},
	} {
		if err := journal.Append(ctx, e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}

	stmt := Statement{
		StatementID: "S-pg", CustodianID: "CUST-A", PortfolioID: "PF1",
		BusinessDate: settledAt, ReceivedAt: t0,
		Positions: map[string]*big.Rat{"AAPL": big.NewRat(100, 1)},
		Cash:      map[string]*big.Rat{"USD": big.NewRat(-100, 1)},
		Grain:     recon.GrainTransactions,
		Transactions: []recon.Transaction{
			{ExternalRef: "fill-a", InstrumentID: "AAPL", Quantity: big.NewRat(60, 1), SettlementDate: settledAt},
			{ExternalRef: "venue-x9", InstrumentID: "AAPL", Quantity: big.NewRat(40, 1), SettlementDate: settledAt},
		},
	}
	if err := store.SaveStatement(ctx, stmt); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}

	scope, err := NewBookScope([]Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}}, nil)
	if err != nil {
		t.Fatalf("NewBookScope: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r, err := NewReconciler(store, LedgerBookLoader(journal, scope, logger), nil, new(big.Rat), nil, logger,
		func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	run, err := r.Reconcile(ctx, Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: settledAt})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if run.Outcome != OutcomeBreaks {
		t.Fatalf("outcome = %s, want %s.\n\nThe netted position and cash totals AGREE, so this is "+
			"exactly the state the pre-#1049 control reported as a clean book while it held an "+
			"execution the custodian never settled.", run.Outcome, OutcomeBreaks)
	}

	outstanding, err := store.OutstandingBreaks(ctx)
	if err != nil {
		t.Fatalf("OutstandingBreaks: %v", err)
	}
	want := map[string]recon.BreakKind{
		BreakID("PF1", "CUST-A", recon.BreakExecutionMissingAtCustodian, "fill-b"): recon.BreakExecutionMissingAtCustodian,
		BreakID("PF1", "CUST-A", recon.BreakExecutionMissingInIBOR, "venue-x9"):    recon.BreakExecutionMissingInIBOR,
	}
	got := map[string]recon.BreakKind{}
	for _, b := range outstanding {
		got[b.BreakID] = b.Kind
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Fatalf("stored break %q has kind %s, want %s. Got %v.\n\nThe kind column round-trips "+
				"through recon.BreakKinds(); a kind that does not survive storage comes back "+
				"unclassified and its gauge label is wrong.", id, got[id], kind, got)
		}
	}

	// AND THE LIFECYCLE IS THE SAME ONE. A second identical run must not reset the
	// age or the assignee: an execution break an operator is working is a working
	// item like any other.
	b := outstanding[0]
	if err := b.Assign("ops-desk", t0); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := recordFixtureLifecycle(t, store, ctx, b); err != nil {
		t.Fatalf("SaveBreak: %v", err)
	}
	later := t0.Add(24 * time.Hour)
	r2, err := NewReconciler(store, LedgerBookLoader(journal, scope, logger), nil, new(big.Rat), nil, logger,
		func() time.Time { return later })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	if _, err := r2.Reconcile(ctx, Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: settledAt}); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	again, err := store.LoadBreak(ctx, b.BreakID)
	if err != nil {
		t.Fatalf("LoadBreak: %v", err)
	}
	if again.Assignee != "ops-desk" || again.Status != BreakAssigned {
		t.Errorf("redetection reset the operator's work: assignee=%q status=%s", again.Assignee, again.Status)
	}
	if !again.FirstSeenAt.Equal(b.FirstSeenAt) {
		t.Errorf("FirstSeenAt advanced on redetection (%s -> %s) — the break would be permanently one "+
			"run old and the ageing alert could never fire", b.FirstSeenAt, again.FirstSeenAt)
	}
}

// testWriter routes slog output into the test log so a failing run carries the
// loader's residue and leg lines with it.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

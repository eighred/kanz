package custody

import (
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func TestPostgresDatedCustodyEvidence(t *testing.T) {
	const tenant = "dated-tenant"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	st := NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	journal := ledger.NewPostgres(pool)
	now := t0.AddDate(0, 0, 3)
	comparer, err := NewReconciler(st, LedgerBookLoader(journal, closeScope(t, true), testLogger()), &capturePublisher{}, nil, nil, testLogger(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	feed := manualSignedFeed(t, ctx, st)
	check := func(state, source, runID, outcome string) {
		t.Helper()
		got, err := st.DatedResult(ctx, sub)
		if err != nil || got.State != state || got.StatementID != source || got.Run.RunID != runID || (runID != "" && got.Run.Outcome.String() != outcome) {
			t.Fatalf("want %s %s %s %s; got %+v %v", state, source, runID, outcome, got, err)
		}
	}
	execute := func() Run {
		t.Helper()
		now = now.Add(time.Minute)
		run, err := comparer.Reconcile(ctx, sub)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	check("not_compared", "", "", "")
	absent := execute()
	check("recorded", "", absent.RunID, "no_statement")
	if err := journal.Append(ctx, closeTrade("initial", 10, -100, t0), nil); err != nil {
		t.Fatal(err)
	}
	first := feed(8, -100)
	check("pending", first, "", "")
	opened := execute()
	check("recorded", first, opened.RunID, "breaks")
	correction := feed(10, -100)
	check("pending", correction, "", "")
	// An older clean result must stay unknown after restoring a valid signed level.
	legacy := Run{RunID: "legacy-result", Subject: sub, StatementID: correction, Outcome: OutcomeClean, CompletedAt: t0.AddDate(0, 0, 10), Basis: opened.Basis}
	if err := st.SaveRun(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	check("unknown", "", "", "")
	closed := execute()
	check("recorded", correction, closed.RunID, "clean")
	failed := Run{RunID: "later-failure", Subject: sub, StatementID: correction, Outcome: OutcomeFailed, CompletedAt: t0.AddDate(0, 0, 20)}
	if err := st.SaveRun(ctx, failed); err != nil {
		t.Fatal(err)
	}
	check("recorded", correction, closed.RunID, "clean")
	if err := journal.Append(ctx, closeTrade("tomorrow", 8, -80, BusinessDay(t0).AddDate(0, 0, 1)), nil); err != nil {
		t.Fatal(err)
	}
	check("recorded", correction, closed.RunID, "clean")
	// A backdated entry invalidates the old book even before a new comparison.
	if err := journal.Append(ctx, closeTrade("late", 2, -20, t0), nil); err != nil {
		t.Fatal(err)
	}
	check("pending", correction, "", "")
	advanced := execute()
	check("recorded", correction, advanced.RunID, "breaks")
	got, err := st.DatedResult(ctx, sub)
	if err != nil || got.Run.Basis == nil || got.Run.Basis.JournalPosition != advanced.Basis.JournalPosition {
		t.Fatal("journal position lost", err)
	}
	foreign := NewPostgres(newCustodyPool(t, "other-tenant"))
	got, err = foreign.DatedResult(bus.WithTenantID(t.Context(), "other-tenant"), sub)
	if err != nil || got.State != "not_compared" || got.StatementID != "" || got.Run.RunID != "" {
		t.Fatal("foreign tenant result leaked", err)
	}
	if _, err := st.DatedResult(bus.WithTenantID(t.Context(), "other-tenant"), sub); err == nil {
		t.Fatal("accepted incompatible RLS session")
	}

	if _, err := pool.Exec(ctx, "UPDATE ledger_heads SET position=position-1 WHERE portfolio_id=$1", sub.PortfolioID); err != nil {
		t.Fatal(err)
	}
	check("unknown", "", "", "")
	if _, err := pool.Exec(ctx, "UPDATE ledger_heads SET position=position+1 WHERE portfolio_id=$1", sub.PortfolioID); err != nil {
		t.Fatal(err)
	}

	// Reaching this storage state requires damage or a restore. An old signed-looking
	// identifier and a true numeric-precision flag cannot attest the actual statement.
	tx, err := st.exactTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `UPDATE custody_statements SET positions='{"AAPL":"99"}' WHERE statement_id=$1`, correction); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	check("unknown", "", "", "")
	latest := feed(12, -120)
	if _, err := pool.Exec(ctx, "DELETE FROM custody_statements WHERE statement_id=$1", latest); err != nil {
		t.Fatal(err)
	}
	check("pending", latest, "", "")
	// A later unreceipted level must never be displayed as the signed customer's result.
	unsigned := statement("legacy-level", map[string]int64{"AAPL": 12}, map[string]int64{"USD": -120})
	unsigned.ReceivedAt = time.Now().UTC().Add(time.Hour)
	if err := st.SaveStatement(ctx, unsigned); err != nil {
		t.Fatal(err)
	}
	check("unknown", "", "", "")
}

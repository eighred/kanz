package custody

import (
	"encoding/json"
	"fmt"
	"reflect"
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

func TestSignedDatedReferenceCoverage(t *testing.T) {
	const tenant = "reference-coverage"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	st, journal := NewPostgres(pool), ledger.NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	now := t0.AddDate(0, 0, 3)
	r, err := NewReconciler(st, LedgerBookLoader(journal, closeScope(t, true), testLogger()), &capturePublisher{}, nil, nil, testLogger(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	write := signedStatementFeed(t, ctx, st)
	date := sub.BusinessDate.Format(time.RFC3339)
	feed := func(grain, trades string) string {
		t.Helper()
		return write(json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":%q,"positions":[],"cash":[],"transactions":%s,"grain":"STATEMENT_GRAIN_%s"}`, date, trades, grain)))
	}
	check := func(source string, outcome Outcome, want *TransactionComparison) Run {
		t.Helper()
		now = now.Add(time.Minute)
		run, err := r.Reconcile(ctx, sub)
		if err != nil || run.Outcome != outcome {
			t.Fatalf("reconcile %s: %+v, %v", source, run, err)
		}
		got, err := NewPostgres(pool).DatedResult(ctx, sub)
		if err != nil || got.State != "recorded" || got.StatementID != source || got.Run.RunID != run.RunID || got.Run.Basis == nil || !reflect.DeepEqual(got.Run.Basis.Transactions, want) {
			t.Fatalf("dated reference coverage: %+v, want %+v, err=%v", got, want, err)
		}
		return run
	}
	ran := func(statement, book, missing int) *TransactionComparison {
		return &TransactionComparison{Pass: "ran", StatementReferences: &statement, BookReferences: &book, UnreferencedBookTrades: &missing}
	}
	zero := feed("BALANCES_ONLY", "[]")
	check(zero, OutcomeClean, &TransactionComparison{Pass: "balances_only"})
	full := feed("TRANSACTIONS", "[]")
	measured := check(full, OutcomeClean, ran(0, 0, 0))
	// A retained run written before reference evidence existed can still
	// establish its signed balances. Its historical transaction scope is unknown.
	if _, err := pool.Exec(ctx, "UPDATE custody_runs SET book_basis=book_basis-'transactions' WHERE run_id=$1", measured.RunID); err != nil {
		t.Fatal(err)
	}
	old, err := st.DatedResult(ctx, sub)
	if err != nil || old.State != "recorded" || old.Run.Basis == nil || old.Run.Basis.Transactions != nil {
		t.Fatal("legacy run invented reference coverage", old, err)
	}
	check(full, OutcomeClean, ran(0, 0, 0))
	post := func(id, ref string, qty int64, day time.Time) {
		t.Helper()
		entry := closeTrade(id, qty, -10*qty, day)
		entry.SourceRef = ref
		if err := journal.Append(ctx, entry, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Both dates net to zero. Only two of these four unreferenced trades
	// settle on the compared date.
	post("earlier-buy", "", 1, t0.AddDate(0, 0, -1))
	post("earlier-sell", "", -1, t0.AddDate(0, 0, -1))
	post("today-buy", "", 1, t0)
	post("today-sell", "", -1, t0)
	check(full, OutcomeClean, ran(0, 0, 2))
	post("book-entry-one", "independent-buy", 1, t0)
	post("book-entry-two", "independent-sell", -1, t0)
	trades := func(buyReference string) string {
		line := func(ref string, qty int) string {
			return fmt.Sprintf(`{"externalRef":%q,"instrumentId":"AAPL","currencyCode":"USD","tradeDate":%q,"quantity":{"coefficient":"%d"},"price":{"coefficient":"10"},"cash":{"coefficient":"%d"}}`, ref, date, qty, -10*qty)
		}
		return "[" + line(buyReference, 1) + "," + line("independent-sell", -1) + "]"
	}
	disputed := feed("TRANSACTIONS", trades("different-buy"))
	check(disputed, OutcomeBreaks, ran(2, 2, 2))
	matching := feed("TRANSACTIONS", trades("independent-buy"))
	last := check(matching, OutcomeClean, ran(2, 2, 2))
	// Retained counts must fit the actual prefix, the signed inventory, and
	// the membership result. None of these changes can describe the measured run.
	for _, counts := range []string{
		`{"pass":"ran","book_references":2,"unreferenced_book_trades":2}`,
		`{"pass":"ran","statement_references":1,"book_references":2,"unreferenced_book_trades":2}`,
		`{"pass":"ran","statement_references":2,"book_references":1,"unreferenced_book_trades":2}`,
		`{"pass":"ran","statement_references":2,"book_references":2,"unreferenced_book_trades":5}`,
		`{"pass":"balances_only"}`,
	} {
		if _, err := pool.Exec(ctx, "UPDATE custody_runs SET book_basis=jsonb_set(book_basis,'{transactions}',$2::jsonb) WHERE run_id=$1", last.RunID, counts); err != nil {
			t.Fatal(err)
		}
		got, err := st.DatedResult(ctx, sub)
		if err != nil || got.State != "unknown" || got.Run.RunID != "" {
			t.Fatalf("inconsistent coverage exposed: %+v, %v", got, err)
		}
	}
}

package custody

import (
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/google/uuid"
)

func TestPreviousComparisonOfCorrectionWithOutstandingBreaks(t *testing.T) {
	const tenant, account = "synthetic-custody-pilot", "sandbox-account"
	ctx := bus.WithTenantID(t.Context(), tenant)
	pool, _, venue, governance := assignmentTestPools(t, tenant)
	enroll := signedAccountFixture(t, pool, ctx)
	if err := enroll(venue, "venue_identity", nil); err != nil {
		t.Fatal(err)
	}
	if err := enroll(governance, "custody_assignment", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO custody_assignment_readers VALUES(session_user,$1)", tenant); err != nil {
		t.Fatal(err)
	}
	store, journal := NewPostgres(pool), ledger.NewPostgres(pool)
	sub := Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: BusinessDay(t0)}
	scope, err := NewBookScope([]Subject{sub}, map[string]map[string][]string{"PF1": {"CUST-A": {account}}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err = scope.AdmitAssignments(ctx, sub.BusinessDate, store)
	if err != nil {
		t.Fatal(err)
	}
	entry := closeTrade("purchase", 10, -100, t0)
	entry.VenueAccountID = account
	if err := journal.Append(ctx, entry, nil); err != nil {
		t.Fatal(err)
	}
	reconciler := manualReconciler(t, store, LedgerBookLoader(journal, scope, testLogger()), &capturePublisher{})
	deliver := signedStatementFeedForAccount(t, ctx, store, account)
	compare := func(qty int) Run {
		t.Helper()
		id := deliver(json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":%q,"positions":[{"instrumentId":"AAPL","quantity":{"coefficient":"%d"}}],"cash":[{"currencyCode":"USD","balance":{"coefficient":"-100"}}],"transactions":[],"grain":"STATEMENT_GRAIN_BALANCES_ONLY"}`, sub.BusinessDate.Format(time.RFC3339), qty)))
		run, err := reconciler.ReconcileManual(ctx, sub, uuid.NewString(), "operator")
		if err != nil || run.StatementID != id || run.Outcome != OutcomeBreaks || len(run.Breaks) != 1 || run.Breaks[0].Diff.Cmp(big.NewRat(int64(10-qty), 1)) != 0 {
			t.Fatalf("signed comparison with quantity %d: %+v %v", qty, run, err)
		}
		return run
	}
	r1, r2 := compare(8), compare(9)
	previous, err := store.PreviousComparison(ctx, sub, account, r2.RunID)
	if err != nil || previous.State != "recorded" || !previous.AccountAssignmentVerified || previous.PreviousOfStatementID != r2.StatementID ||
		previous.Run.RunID != r1.RunID || previous.StatementID != r1.StatementID || len(previous.Run.Breaks) != 1 || previous.Run.Breaks[0].Diff.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("earlier source and original break: %+v %v", previous, err)
	}
	// This operator repeats r2 at exactly the same completion time. The two
	// authentic receipts cannot identify which comparison of r2 came first.
	repeated, err := reconciler.ReconcileManual(ctx, sub, uuid.NewString(), "operator")
	if err != nil || repeated.RunID == r2.RunID || repeated.StatementID != r2.StatementID || !repeated.CompletedAt.Equal(r2.CompletedAt) {
		t.Fatalf("repeat of r2: %+v %v", repeated, err)
	}
	r3 := compare(7)
	for _, lookup := range []struct{ account, run string }{{account, r1.RunID}, {"other-account", r2.RunID}, {account, r3.RunID}} {
		result, err := store.PreviousComparison(ctx, sub, lookup.account, lookup.run)
		if err != nil || result.State != "unknown" || result.StatementID != "" || result.Run.RunID != "" {
			t.Fatalf("unbound predecessor: %+v %v", result, err)
		}
	}
}

func TestPreviousBookComparisonOrdersOnlyComparableReceipts(t *testing.T) {
	const tenant, account = "synthetic-custody-pilot", "sandbox-account"
	ctx := bus.WithTenantID(t.Context(), tenant)
	pool, _, venue, governance := assignmentTestPools(t, tenant)
	enroll := signedAccountFixture(t, pool, ctx)
	if err := enroll(venue, "venue_identity", nil); err != nil {
		t.Fatal(err)
	}
	if err := enroll(governance, "custody_assignment", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO custody_assignment_readers VALUES(session_user,$1)", tenant); err != nil {
		t.Fatal(err)
	}
	store, journal := NewPostgres(pool), ledger.NewPostgres(pool)
	sub := Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: BusinessDay(t0)}
	scope, err := NewBookScope([]Subject{sub}, map[string]map[string][]string{"PF1": {"CUST-A": {account}}})
	if err != nil {
		t.Fatal(err)
	}
	admit := func() *BookScope {
		t.Helper()
		a, err := scope.AdmitAssignments(ctx, sub.BusinessDate, store)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	admitted := admit()
	statement := json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":%q,"positions":[],"cash":[{"currencyCode":"USD","balance":{"coefficient":"0"}}],"transactions":[],"grain":"STATEMENT_GRAIN_BALANCES_ONLY"}`, sub.BusinessDate.Format(time.RFC3339)))
	deliver := signedStatementFeedForAccount(t, ctx, store, account)
	statementID := deliver(statement)
	baseTime := time.Now().UTC().Truncate(time.Microsecond)
	compare := func(position int64, offset time.Duration, tolerance *big.Rat) Run {
		t.Helper()
		r, err := NewReconciler(store, LedgerBookLoader(journal, admitted, testLogger()), &capturePublisher{}, tolerance, nil, testLogger(), func() time.Time { return baseTime.Add(offset) })
		if err != nil {
			t.Fatal(err)
		}
		run, err := r.ReconcileManual(ctx, sub, uuid.NewString(), "operator:book-comparison")
		if err != nil || !run.Outcome.IsFullComparison() || run.StatementID != statementID || run.Basis == nil || run.Basis.JournalPosition != position {
			t.Fatal("capture", run, err)
		}
		return run
	}
	appendCash := func(id string) {
		t.Helper()
		if err := journal.Append(ctx, &ledger.Event{EntryID: id, PortfolioID: sub.PortfolioID, VenueAccountID: account, Type: ledger.EntryCash, CashCurrency: "USD", Cash: big.NewRat(1, 1), Effective: t0, Knowledge: time.Now().UTC()}, nil); err != nil {
			t.Fatal(err)
		}
	}
	prior := func(anchor Run, want Run) {
		t.Helper()
		got, err := store.PreviousBookComparison(ctx, sub, account, anchor.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if want.RunID == "" {
			if got.State != "unknown" || got.StatementID != "" || got.Run.RunID != "" || got.SignedStatementSHA256 != "" || got.AccountAssignmentVerified {
				t.Fatalf("expected unavailable predecessor for %s; got %+v", anchor.RunID, got)
			}
			return
		}
		if got.State != "recorded" || !got.AccountAssignmentVerified || got.Run.RunID != want.RunID || got.StatementID != anchor.StatementID ||
			got.PreviousOfStatementID != anchor.StatementID || got.PreviousOfJournalPosition == nil || *got.PreviousOfJournalPosition != anchor.Basis.JournalPosition ||
			got.Run.Basis.JournalPosition != want.Basis.JournalPosition || !got.Run.CompletedAt.Equal(want.CompletedAt) {
			t.Fatalf("prior book comparison %+v; want %s at %d", got, want.RunID, want.Basis.JournalPosition)
		}
	}

	// A repeated close at the anchor prefix never qualifies, including zero.
	zero := compare(0, 0, nil)
	zeroLatest := compare(0, time.Second, nil)
	prior(zero, Run{})
	prior(zeroLatest, Run{})
	appendCash("first-cash")
	one := compare(1, 2*time.Second, nil)
	prior(one, zeroLatest)
	oneLatest := compare(1, 3*time.Second, nil)
	prior(oneLatest, zeroLatest)
	appendCash("second-cash")
	// A new receipt with the same database capture time as oneLatest may
	// only link to the demonstrably earlier capture at that journal position.
	prior(compare(2, 3*time.Second+200*time.Nanosecond, nil), one)
	two := compare(2, 4*time.Second, nil)
	prior(two, oneLatest)
	prior(compare(2, 5*time.Second, big.NewRat(1, 10)), Run{})

	// Two different original receipt payloads whose timestamps land in the
	// same PostgreSQL microsecond cannot identify one latest prior receipt.
	compare(2, 6*time.Second, nil)
	compare(2, 6*time.Second+100*time.Nanosecond, nil)
	appendCash("third-cash")
	three := compare(3, 7*time.Second, nil)
	prior(three, Run{})

	wrongDate := sub
	wrongDate.BusinessDate = wrongDate.BusinessDate.AddDate(0, 0, -1)
	for _, q := range []struct {
		sub                 Subject
		account, id, tenant string
	}{
		{sub, "outside-account", two.RunID, tenant}, {wrongDate, account, two.RunID, tenant},
		{sub, account, two.RunID, "outside-tenant"}, {sub, account, "\n", tenant},
	} {
		got, err := store.PreviousBookComparison(bus.WithTenantID(t.Context(), q.tenant), q.sub, q.account, q.id)
		if err != nil || got.State != "unknown" || got.StatementID != "" || got.Run.RunID != "" || got.SignedStatementSHA256 != "" {
			t.Fatal("unavailable scope", got, err)
		}
	}

	// Independently signed reinstatement changes the captured authority even
	// when the venue, account string and signed statement all stay identical.
	if err := enroll(governance, "custody_assignment_withdrawn", nil); err != nil {
		t.Fatal(err)
	}
	if err := enroll(governance, "custody_assignment_reinstated", nil); err != nil {
		t.Fatal(err)
	}
	admitted = admit()
	appendCash("fourth-cash")
	four := compare(4, 8*time.Second, nil)
	if four.Basis.sameScope(three.Basis, sub.CustodianID) {
		t.Fatal("assignment did not change")
	}
	prior(four, Run{})
	// Original receipts can still recover their own historically valid scope.
	prior(two, oneLatest)
	statementID = deliver(statement)
	revised := compare(4, 9*time.Second, nil)
	if revised.StatementID == four.StatementID {
		t.Fatal("expected a separately signed revision")
	}
	prior(revised, Run{})
}

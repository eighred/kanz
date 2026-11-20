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

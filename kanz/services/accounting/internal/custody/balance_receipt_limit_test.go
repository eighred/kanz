package custody

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/prometheus/client_golang/prometheus"
)

func TestSignedBalanceCloseRefusesUnreadableReceipt(t *testing.T) {
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
	store := NewPostgres(pool)
	sub := Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: BusinessDay(t0)}
	scope, err := NewBookScope([]Subject{sub}, map[string]map[string][]string{"PF1": {"CUST-A": {account}}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err = scope.AdmitAssignments(ctx, sub.BusinessDate, store)
	if err != nil {
		t.Fatal(err)
	}

	instruments := make([]string, historicalBreakLimit+1)
	for i := range instruments {
		instruments[i] = fmt.Sprintf("K%04d", i)
	}
	retain := retainedStatementFeedForAccount(t, ctx, store, account, instruments...)
	deliver := func(ids []string) string {
		t.Helper()
		positions := make([]string, len(ids))
		for i, id := range ids {
			positions[i] = fmt.Sprintf(`{"instrumentId":%q,"quantity":{"coefficient":"1"}}`, id)
		}
		level := fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":%q,"positions":[%s],"cash":[],"transactions":[],"grain":"STATEMENT_GRAIN_BALANCES_ONLY"}`, sub.BusinessDate.Format(time.RFC3339), strings.Join(positions, ","))
		id, apply := retain(json.RawMessage(level))
		apply()
		return id
	}
	now := t0.AddDate(0, 0, 1)
	reg := prometheus.NewRegistry()
	r, err := NewReconciler(store, LedgerBookLoader(ledger.NewPostgres(pool), scope, testLogger()), &capturePublisher{}, nil, NewMetrics(reg), testLogger(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first := deliver([]string{"AAPL"})
	prior, err := r.Reconcile(ctx, sub)
	if err != nil || prior.StatementID != first || prior.Outcome != OutcomeBreaks || len(prior.Breaks) != 1 {
		t.Fatal("initial signed comparison", prior, err)
	}
	original := prior.Breaks[0]

	now = now.Add(time.Minute)
	oversized := deliver(instruments)
	failed, err := r.Reconcile(ctx, sub)
	if !errors.Is(err, errRunReceiptLimit) || failed.Outcome != OutcomeFailed || len(failed.Breaks) != 0 {
		read, readErr := store.HistoricalReceipt(ctx, sub, account, failed.RunID)
		t.Fatalf("oversized signed close: outcome=%s breaks=%d saved=%s error=%v saved_error=%v", failed.Outcome, len(failed.Breaks), read.State, err, readErr)
	}
	var durable, fact []byte
	err = pool.QueryRow(ctx, `SELECT r.manual_receipt,o.payload FROM custody_runs r JOIN outbox o ON o.tenant_id=r.tenant_id AND o.correlation_id=r.run_id
		WHERE r.run_id=$1 AND r.outcome='failed' AND o.event_type=$2`, failed.RunID, SubjectRun).Scan(&durable, &fact)
	if err != nil || !bytes.Equal(durable, fact) {
		t.Fatal("durable failed comparison and completion", err)
	}
	recorded, err := runFromRecord(fact)
	if err != nil || recorded.Outcome != OutcomeFailed || !strings.Contains(recorded.FailureReason, errRunReceiptLimit.Error()) {
		t.Fatal("retained refusal", recorded, err)
	}
	open, err := store.OutstandingBreaks(ctx, sub.PortfolioID)
	if err != nil || len(open) != 1 || open[0].BreakID != original.BreakID || open[0].Detection.RunID != prior.RunID || open[0].Status != BreakOpen {
		t.Fatal("original working queue", len(open), err)
	}
	freshness, ok := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": sub.PortfolioID, "custodian": sub.CustodianID})
	if !ok || freshness != float64(prior.CompletedAt.Unix()) {
		t.Fatal("oversized close advanced freshness", freshness)
	}
	saved, err := store.HistoricalReceipt(ctx, sub, account, prior.RunID)
	if err != nil || saved.State != "recorded" || !saved.AccountAssignmentVerified || saved.StatementID != first || len(saved.Run.Breaks) != 1 || saved.Run.Breaks[0].BreakID != original.BreakID {
		t.Fatal("original retained signed comparison", saved.State, err)
	}
	current, err := store.DatedResult(ctx, sub)
	if err != nil || current.State != "pending" || current.StatementID != oversized {
		t.Fatal("uncompared current revision", current.State, err)
	}
	now = now.Add(time.Minute)
	boundary := deliver(instruments[:historicalBreakLimit])
	bounded, err := r.Reconcile(ctx, sub)
	if err != nil || bounded.StatementID != boundary || bounded.Outcome != OutcomeBreaks || len(bounded.Breaks) != historicalBreakLimit {
		t.Fatal("signed close at the row limit", bounded.Outcome, len(bounded.Breaks), err)
	}
	saved, err = store.HistoricalReceipt(ctx, sub, account, bounded.RunID)
	if err != nil || saved.State != "recorded" || saved.StatementID != boundary || len(saved.Run.Breaks) != historicalBreakLimit {
		t.Fatal("saved comparison at the row limit", saved.State, len(saved.Run.Breaks), err)
	}
	now = now.Add(time.Minute)
	fixed := deliver(nil)
	clean, err := r.Reconcile(ctx, sub)
	if err != nil || clean.Outcome != OutcomeClean || clean.StatementID != fixed {
		t.Fatal("corrected signed close", clean, err)
	}
}

func TestSignedBalanceCompletionBoundsFinalAnnotatedBytes(t *testing.T) {
	ctx := bus.WithTenantID(t.Context(), "receipt-size")
	run := Run{Subject: subject(), RunID: "size", StatementID: batchStatementID("source", 1, strings.Repeat("a", 64)), CompletedAt: t0, Outcome: OutcomeBreaks}
	// Independent 4 KiB explanations are individually permitted. Carrying the
	// existing operator annotations into the full completion can exceed 1 MiB
	// while the count is comfortably within the saved reader's row limit.
	row := detectedQuantityBreak(t0, 2, 1)[0]
	row.Status, row.Assignee, row.Explanation = BreakExplained, "operator", strings.Repeat("x", 4096)
	for i := range 260 {
		row.Key = fmt.Sprintf("K%04d", i)
		row.BreakID = BreakID(row.PortfolioID, row.CustodianID, row.Kind, row.Key)
		run.Breaks = append(run.Breaks, row)
	}
	if _, err := runRecord(ctx, "receipt-size", run); !errors.Is(err, errRunReceiptLimit) {
		t.Fatal("oversized signed balance completion", err)
	}
	unsigned := run
	unsigned.StatementID = "legacy-statement"
	legacy, err := runRecord(ctx, "receipt-size", unsigned)
	if err != nil || len(legacy.Payload) <= BatchLimit {
		t.Fatal("legacy completion", len(legacy.Payload), err)
	}
	run.Breaks = run.Breaks[:240]
	bounded, err := runRecord(ctx, "receipt-size", run)
	if err != nil || len(bounded.Payload) > BatchLimit || len(bounded.Payload) <= BatchLimit/2 {
		t.Fatal("bounded signed balance completion", len(bounded.Payload), err)
	}
}

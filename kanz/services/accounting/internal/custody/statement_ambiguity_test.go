package custody

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/prometheus/client_golang/prometheus"
)

func TestNewestStatementAmbiguityRefusesBothArrivalOrders(t *testing.T) {
	for _, sameValue := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("same_value=%t/reverse=%t", sameValue, reverse), func(t *testing.T) {
				store := NewMemoryStore()
				first := statement("first", map[string]int64{"AAPL": 90}, nil)
				second := statement("second", map[string]int64{"AAPL": 100}, nil)
				if sameValue {
					second.Positions = first.Positions
				}
				if reverse {
					first, second = second, first
				}
				for _, s := range []Statement{first, first, second, second} {
					if err := store.SaveStatement(t.Context(), s); err != nil {
						t.Fatal(err)
					}
				}
				if got, err := store.LatestStatement(t.Context(), subject()); !errors.Is(err, ErrAmbiguousStatement) || got.StatementID != "" {
					t.Fatalf("arrival order manufactured a newest statement: id=%s err=%v", got.StatementID, err)
				}
				if len(store.statements[subject().Key()]) != 2 {
					t.Fatal("redelivery duplicated or ambiguity removed source evidence")
				}
			})
		}
	}
}

func TestNewestStatementAmbiguityRecoversOnlyWithLaterReceipt(t *testing.T) {
	store := NewMemoryStore()
	assertStatementAmbiguityRecovery(t, store)
}

// The same observable contract runs against both store implementations. The
// unique later receipt is the ordering evidence; statement IDs are never ranked.
func assertStatementAmbiguityRecovery(t *testing.T, store Store) {
	t.Helper()
	ctx := t.Context()
	first := statement("first", map[string]int64{"AAPL": 90}, nil)
	if err := store.SaveStatement(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got, err := store.LatestStatement(ctx, subject()); err != nil || got.StatementID != first.StatementID {
		t.Fatalf("one statement was not usable: %+v %v", got, err)
	}
	second := statement("second", map[string]int64{"AAPL": 100}, nil)
	for _, s := range []Statement{first, second, first} {
		if err := store.SaveStatement(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := store.LatestStatement(ctx, subject()); !errors.Is(err, ErrAmbiguousStatement) || got.StatementID != "" {
		t.Fatalf("equal latest receipts produced a verdict: %+v %v", got, err)
	}
	for _, field := range []string{"portfolio", "custodian", "date"} {
		other := statement("other-"+field, nil, nil)
		switch field {
		case "portfolio":
			other.PortfolioID = "OTHER-PF"
		case "custodian":
			other.CustodianID = "OTHER-CUST"
		case "date":
			other.BusinessDate = t0.AddDate(0, 0, 1)
		}
		if err := store.SaveStatement(ctx, other); err != nil {
			t.Fatal(err)
		}
		if got, err := store.LatestStatement(ctx, other.Subject()); err != nil || got.StatementID != other.StatementID {
			t.Fatalf("ambiguity leaked across %s: %+v %v", field, got, err)
		}
	}
	newer := statement("newer", map[string]int64{"AAPL": 100}, nil)
	newer.ReceivedAt = t0.Add(time.Hour)
	for _, s := range []Statement{newer, second, first, newer} {
		if err := store.SaveStatement(ctx, s); err != nil {
			t.Fatal(err)
		}
		got, err := store.LatestStatement(ctx, subject())
		if err != nil || got.StatementID != newer.StatementID || !got.ReceivedAt.Equal(newer.ReceivedAt) {
			t.Fatalf("older tie/redelivery displaced uniquely newer receipt: %+v %v", got, err)
		}
	}
}

func TestAmbiguousStatementRunPreservesLifecycleAndCompletion(t *testing.T) {
	ctx := testRunContext(t.Context())
	store := NewMemoryStore()
	for _, id := range []string{"first", "second"} {
		if err := store.SaveStatement(ctx, statement(id, nil, nil)); err != nil {
			t.Fatal(err)
		}
	}
	prior := investigatedReferenceBreaks(t, ctx, store)
	pub := &capturePublisher{err: errors.New("fixture publisher unavailable")}
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	metrics.SeedPair("PF1", "CUST-A")
	loaded := 0
	loader := func(context.Context, Subject) (*ledger.Book, []ledger.Execution, error) {
		loaded++
		return bookWith(nil, nil), nil, nil
	}
	r, err := NewReconciler(store, loader, pub, nil, metrics, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, ErrAmbiguousStatement) || !errors.Is(err, ErrPublicationPending) || first.Outcome != OutcomeFailed || store.outbox.PendingCount() != 1 {
		t.Fatalf("statement ambiguity lost its committed failure: %+v %v", first, err)
	}
	pub.err = nil
	for range 2 {
		run, err := r.Reconcile(ctx, subject())
		if !errors.Is(err, ErrAmbiguousStatement) || errors.Is(err, ErrPublicationPending) || run.RunID != first.RunID || run.Outcome != OutcomeFailed || loaded != 0 {
			t.Fatalf("unresolved statement was compared or changed on retry: %+v loaded=%d err=%v", run, loaded, err)
		}
		assertReferenceLifecycle(t, ctx, store, prior)
	}
	if len(pub.events) != 1 || store.outbox.PendingCount() != 0 {
		t.Fatal("retry did not deliver exactly one immutable completion")
	}
	assertAmbiguousStatementFact(t, pub.lastRun(t), first)
	if fresh, _ := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": "PF1", "custodian": "CUST-A"}); fresh != 0 {
		t.Fatal("ambiguous statement advanced successful assessment freshness")
	}
	// Refusal is a statement-read failure, not a new kind of economic break.
	if first.StatementID != "" || len(first.Breaks) != 0 {
		t.Fatalf("failed load claimed a comparison: %+v", first)
	}
}

func assertAmbiguousStatementFact(t *testing.T, fact *accountingpb.ReconciliationRun, failed Run) {
	t.Helper()
	if fact.GetRunId() != failed.RunID || fact.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_FAILED ||
		fact.GetStatementId() != "" || fact.GetFailureReason() != failed.FailureReason ||
		!strings.Contains(fact.GetFailureReason(), ErrAmbiguousStatement.Error()) || len(fact.GetBreaks()) != 0 {
		t.Fatalf("completion claimed assessed or different evidence: %v", fact)
	}
}

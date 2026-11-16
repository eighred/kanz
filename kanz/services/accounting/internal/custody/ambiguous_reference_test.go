package custody

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const ambiguousRef = "BTCUSDT-7"

func ambiguousReferenceBook(t *testing.T, ctx context.Context, store ledger.Store) BookLoader {
	t.Helper()
	for _, account := range []string{"account-a", "account-b"} {
		fill := &orderpb.Fill{
			FillId: ambiguousRef, OrderId: "order-" + account, Venue: "BINANCE",
			VenueAccountId: account, VenueExecutionId: "7", InstrumentId: "BTC-USD",
			Side: orderpb.Side_SIDE_BUY, Quantity: &commonpb.Decimal{Coefficient: 1},
			Price:      &commonpb.Decimal{Coefficient: 100},
			Fee:        &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1}},
			ExecutedAt: timestamppb.New(t0.Add(-time.Hour)),
		}
		entry, err := ledger.FromFill("PF1", fill, "USD", t0.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		// A genuine redelivery is still one immutable stored identity. Each
		// account's distinct execution must survive that idempotency boundary.
		for range 2 {
			if err := store.Append(ctx, entry, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	loader := LedgerBookLoader(store, nil, testLogger())
	book, executions, err := loader(ctx, subject())
	if err != nil {
		t.Fatal(err)
	}
	// Independent cash/quantity oracle: two buys, each 1*100 + 1 USD fee.
	if len(executions) != 2 || executions[0].EntryID == executions[1].EntryID ||
		executions[0].Ref != ambiguousRef || executions[1].Ref != ambiguousRef ||
		book.Positions["BTC-USD"].Qty.Cmp(big.NewRat(2, 1)) != 0 || book.Cash["USD"].Cmp(big.NewRat(-202, 1)) != 0 {
		t.Fatalf("scoped executions were lost or misbooked: %v %+v", executions, book)
	}
	return loader
}

func ambiguousReferenceStatement() Statement {
	stmt := statement("ambiguous-ref", map[string]int64{"BTC-USD": 2}, map[string]int64{"USD": -202})
	stmt.Grain = recon.GrainTransactions
	// Reference-only transaction evidence is legitimate; #1194 keeps absent
	// optional figures unavailable rather than manufacturing measured zeros.
	stmt.Transactions = []recon.Transaction{{ExternalRef: ambiguousRef}}
	return stmt
}

func TestAmbiguousExecutionReferenceRecordsFailedRun(t *testing.T) {
	ctx := testRunContext(t.Context())
	store := NewMemoryStore()
	if err := store.SaveStatement(ctx, ambiguousReferenceStatement()); err != nil {
		t.Fatal(err)
	}
	pub := &capturePublisher{}
	prior := investigatedReferenceBreaks(t, ctx, store)
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	metrics.SeedPair("PF1", "CUST-A")
	r, err := NewReconciler(store, ambiguousReferenceBook(t, ctx, ledger.NewMemoryStore()), pub, nil, metrics, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, recon.ErrAmbiguousExecutionReference) || run.Outcome != OutcomeFailed || len(run.Breaks) != 0 {
		t.Fatalf("ambiguous reference produced an assessed verdict: outcome=%s breaks=%d err=%v", run.Outcome, len(run.Breaks), err)
	}
	assertReferenceLifecycle(t, ctx, store, prior)
	fact := pub.lastRun(t)
	if fact.GetFailureReason() != run.FailureReason || !strings.Contains(run.FailureReason, "ambiguous execution reference") || len(pub.events) != 1 {
		t.Fatalf("failed run was not recorded/published once: %+v", fact)
	}
	if fresh, _ := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": "PF1", "custodian": "CUST-A"}); fresh != 0 {
		t.Fatal("failed comparison advanced freshness")
	}
}

func investigatedReferenceBreaks(t *testing.T, ctx context.Context, store Store) []Break {
	t.Helper()
	seed := FromRecon(subject(), []recon.Break{
		{Kind: recon.BreakCash, Key: "EUR", IBOR: big.NewRat(5, 1), Custodian: new(big.Rat), Diff: big.NewRat(5, 1)},
		{Kind: recon.BreakCash, Key: "CHF", IBOR: big.NewRat(7, 1), Custodian: new(big.Rat), Diff: big.NewRat(7, 1)},
	}, t0.Add(-2*time.Hour))
	if _, err := store.UpsertBreaks(ctx, subject(), seed, t0.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var before []Break
	for i, b := range seed {
		if store.ActionsEnabled() {
			action := Action{Tenant: bus.TenantIDFromContext(ctx), Actor: "fixture-operator", RequestID: fmt.Sprintf("ref-claim-%d", i), BreakID: b.BreakID, Kind: "claim", ExpectedRevision: 1}
			claimed, err := store.ApplyAction(ctx, action)
			if err != nil {
				t.Fatal(err)
			}
			if i == 1 {
				action.Kind, action.RequestID, action.Explanation, action.ExpectedRevision = "explain", "ref-explain", "awaiting independent confirmation", claimed.After.Revision
				if _, err := store.ApplyAction(ctx, action); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			// Volatile fixtures prove carry-forward only, not durable human actions.
			if err := b.Assign("fixture-operator", t0.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if i == 1 {
				if err := b.Explain("awaiting independent confirmation", t0.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if err := recordFixtureLifecycle(t, store, ctx, b); err != nil {
				t.Fatal(err)
			}
		}
		stored, err := store.LoadBreak(ctx, b.BreakID)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, stored)
	}
	if before[0].Status != BreakAssigned || before[1].Status != BreakExplained {
		t.Fatal("fixture did not establish prior investigation")
	}
	return before
}

func assertReferenceLifecycle(t *testing.T, ctx context.Context, store Store, before []Break) {
	t.Helper()
	for _, want := range before {
		got, err := store.LoadBreak(ctx, want.BreakID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("unassessed comparison changed lifecycle: before=%+v after=%+v err=%v", want, got, err)
		}
	}
}

func TestAmbiguousReferencePublicationPendingPreservesFailure(t *testing.T) {
	ctx := testRunContext(t.Context())
	store := NewMemoryStore()
	if err := store.SaveStatement(ctx, ambiguousReferenceStatement()); err != nil {
		t.Fatal(err)
	}
	prior := investigatedReferenceBreaks(t, ctx, store)
	pub := &capturePublisher{err: errors.New("fixture publisher unavailable")}
	r, err := NewReconciler(store, ambiguousReferenceBook(t, ctx, ledger.NewMemoryStore()), pub, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, recon.ErrAmbiguousExecutionReference) || !errors.Is(err, ErrPublicationPending) || first.Outcome != OutcomeFailed || store.outbox.PendingCount() != 1 || len(pub.events) != 0 {
		t.Fatalf("commit/refusal/pending delivery confused: %+v %v", first, err)
	}
	pub.err = nil
	for range 2 {
		run, err := r.Reconcile(ctx, subject())
		if !errors.Is(err, recon.ErrAmbiguousExecutionReference) || errors.Is(err, ErrPublicationPending) || run.RunID != first.RunID || store.outbox.PendingCount() != 0 || len(pub.events) != 1 {
			t.Fatalf("retry changed committed refusal or duplicated its FACT: %+v %v", run, err)
		}
		assertReferenceLifecycle(t, ctx, store, prior)
	}
}

func TestSchedulerContinuesAfterAmbiguousReference(t *testing.T) {
	ctx := testRunContext(t.Context())
	store := NewMemoryStore()
	if err := store.SaveStatement(ctx, ambiguousReferenceStatement()); err != nil {
		t.Fatal(err)
	}
	good := statement("good", nil, nil)
	good.PortfolioID = "PF2"
	good.Grain = recon.GrainTransactions
	if err := store.SaveStatement(ctx, good); err != nil {
		t.Fatal(err)
	}
	badLoader := ambiguousReferenceBook(t, ctx, ledger.NewMemoryStore())
	loader := func(ctx context.Context, subject Subject) (*ledger.Book, []ledger.Execution, error) {
		if subject.PortfolioID == "PF1" {
			return badLoader(ctx, subject)
		}
		return ledger.NewBook(subject.PortfolioID), nil, nil
	}
	pub := &capturePublisher{}
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	metrics.SeedPair("PF1", "CUST-A")
	metrics.SeedPair("PF2", "CUST-A")
	r, err := NewReconciler(store, loader, pub, nil, metrics, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(r, SchedulerConfig{Pairs: []Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}, {PortfolioID: "PF2", CustodianID: "CUST-A"}}, Interval: time.Hour}, testLogger(), func() time.Time { return t0.AddDate(0, 0, 1) })
	if err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(ctx)
	for portfolio, outcome := range map[string]Outcome{"PF1": OutcomeFailed, "PF2": OutcomeClean} {
		run, err := store.LatestRun(ctx, portfolio, "CUST-A")
		if err != nil || run.Outcome != outcome {
			t.Fatalf("scheduler lost a pair's independent outcome: %s %+v %v", portfolio, run, err)
		}
		fresh, _ := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": portfolio, "custodian": "CUST-A"})
		if (portfolio == "PF1" && fresh != 0) || (portfolio == "PF2" && fresh != float64(t0.Unix())) {
			t.Fatalf("freshness was not confined to a published successful assessment: %s %v", portfolio, fresh)
		}
	}
	if len(pub.events) != 2 {
		t.Fatal("scheduler did not publish both failure and successful control")
	}
}

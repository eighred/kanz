package custody

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

func manualReconciler(t *testing.T, store Store, load BookLoader, pub Publisher) *Reconciler {
	t.Helper()
	r, err := NewReconciler(store, load, pub, new(big.Rat), NewMetrics(prometheus.NewRegistry()), testLogger(), func() time.Time { return t0.AddDate(0, 0, 3) })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestManualDatedReceiptSurvivesCorrectionAndRestart(t *testing.T) {
	const tenant, actor = "manual-tenant", "operator-A"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	store := NewPostgres(pool)
	journal := ledger.NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	loader := LedgerBookLoader(journal, closeScope(t, true), testLogger())
	r := manualReconciler(t, store, loader, &capturePublisher{})
	key := uuid.NewString()
	missing, err := r.ReconcileManual(ctx, sub, key, actor)
	if err != nil || missing.Outcome != OutcomeNoStatement || missing.RequestID != key || missing.Actor != actor {
		t.Fatalf("absent: %+v %v", missing, err)
	}
	if err := journal.Append(ctx, closeTrade("within-close", 10, -100, t0), nil); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(ctx, closeTrade("next-midnight", 25, -250, sub.BusinessDate.AddDate(0, 0, 1)), nil); err != nil {
		t.Fatal(err)
	}
	signed := manualSignedFeed(t, ctx, store)
	firstID := signed(8, -100)
	originalKey := uuid.NewString()
	original, err := r.ReconcileManual(ctx, sub, originalKey, actor)
	if err != nil || original.Outcome != OutcomeBreaks || len(original.Breaks) != 1 || original.Breaks[0].Diff.Cmp(big.NewRat(2, 1)) != 0 ||
		original.Basis == nil || original.Basis.Mode != BasisDeclared || original.Basis.JournalPosition != 2 {
		t.Fatalf("original dated basis: %+v %v", original, err)
	}
	fixedID := signed(10, -100)
	corrected, err := r.ReconcileManual(ctx, sub, uuid.NewString(), actor)
	if err != nil || corrected.Outcome != OutcomeClean || corrected.StatementID != fixedID {
		t.Fatalf("correction: %+v %v", corrected, err)
	}
	if b, err := store.LoadBreak(ctx, original.Breaks[0].BreakID); err != nil || b.Status != BreakResolved {
		t.Fatalf("corrected break: %+v %v", b, err)
	}
	var payload []byte
	var cause string
	var count int
	if err := pool.QueryRow(ctx, "SELECT payload,causation_id FROM outbox WHERE event_type=$1 AND correlation_id=$2", SubjectRun, original.RunID).Scan(&payload, &cause); err != nil {
		t.Fatal(err)
	}
	var fact accountingpb.ReconciliationRun
	if err := proto.Unmarshal(payload, &fact); err != nil || cause != originalKey || fact.Actor != actor || fact.RequestId != cause || fact.StatementId != firstID {
		t.Fatalf("manual FACT attribution: %v %v", fact.RequestId, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM custody_runs").Scan(&count); err != nil || count != 3 {
		t.Fatalf("runs %d %v", count, err)
	}
	// The request must remain reproducible when the book cannot be reread or
	// the already-published transport record has been removed.
	if _, err := pool.Exec(ctx, "DELETE FROM outbox WHERE event_type=$1 AND published_at IS NOT NULL", SubjectRun); err != nil {
		t.Fatal(err)
	}
	noRead := func(context.Context, Subject) (*ledger.Book, []ledger.Execution, *RunBasis, error) {
		t.Fatal("receipt replay re-read the book")
		return nil, nil, nil, nil
	}
	restart := manualReconciler(t, NewPostgres(pool), noRead, &capturePublisher{})
	for _, prior := range []Run{missing, original} {
		got, err := restart.ReconcileManual(ctx, sub, prior.RequestID, actor)
		if err != nil || got.RunID != prior.RunID || got.Outcome != prior.Outcome || got.StatementID != prior.StatementID ||
			got.Actor != actor || !got.CompletedAt.Equal(prior.CompletedAt) || len(got.Breaks) != len(prior.Breaks) {
			t.Fatalf("immutable receipt: %+v %v", got, err)
		}
		if got.Basis != nil && got.Basis.JournalPosition != 2 {
			t.Fatal("replayed basis changed")
		}
	}
	if _, err := restart.ReconcileManual(ctx, sub, originalKey, "another-actor"); !errors.Is(err, ErrManualRequestConflict) {
		t.Fatalf("actor rebind: %v", err)
	}
	wrong := sub
	wrong.PortfolioID = "another-portfolio"
	if _, err := restart.ReconcileManual(ctx, wrong, originalKey, actor); !errors.Is(err, ErrManualRequestConflict) {
		t.Fatalf("portfolio rebind: %v", err)
	}
	if _, err := restart.ReconcileManual(bus.WithTenantID(ctx, "another-tenant"), sub, originalKey, actor); !errors.Is(err, ErrInvalidManualRequest) {
		t.Fatalf("tenant mismatch: %v", err)
	}
	later := sub
	later.BusinessDate = later.BusinessDate.AddDate(0, 0, 1)
	laterSt := statement("later-break", map[string]int64{"AAPL": 10}, map[string]int64{"USD": -100})
	laterSt.BusinessDate = later.BusinessDate
	laterSt.ReceivedAt = t0.AddDate(0, 0, 1)
	if err := store.SaveStatement(ctx, laterSt); err != nil {
		t.Fatal(err)
	}
	scheduled, err := r.Reconcile(ctx, later)
	if err != nil || scheduled.Outcome != OutcomeBreaks || len(scheduled.Breaks) == 0 {
		t.Fatalf("later scheduled run: %+v %v", scheduled, err)
	}
	staleKey := uuid.NewString()
	for range 2 {
		refused, err := r.ReconcileManual(ctx, sub, staleKey, actor)
		if !errors.Is(err, ErrHistoricalLifecycle) || refused.Outcome != OutcomeFailed || refused.StatementID != fixedID {
			t.Fatalf("historical receipt: %+v %v", refused, err)
		}
		if b, err := store.LoadBreak(ctx, scheduled.Breaks[0].BreakID); err != nil || b.Status == BreakResolved {
			t.Fatal("older comparison resolved later break")
		}
	}
}

func TestManualScheduledRaceUsesNewestJournalBasis(t *testing.T) {
	const tenant = "manual-race"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 15*time.Second)
	defer cancel()
	store := NewPostgres(pool)
	journal := ledger.NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	if err := journal.Append(ctx, closeTrade("initial", 10, -100, t0), nil); err != nil {
		t.Fatal(err)
	}
	manualSignedFeed(t, ctx, store)(10, -100)
	load := LedgerBookLoader(journal, closeScope(t, true), testLogger())
	loaded, resume := make(chan struct{}), make(chan struct{})
	slow := manualReconciler(t, store, func(ctx context.Context, s Subject) (*ledger.Book, []ledger.Execution, *RunBasis, error) {
		book, ex, basis, err := load(ctx, s)
		close(loaded)
		select {
		case <-resume:
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		}
		return book, ex, basis, err
	}, &capturePublisher{})
	request := uuid.NewString()
	done := make(chan error, 1)
	go func() { _, err := slow.ReconcileManual(ctx, sub, request, "operator"); done <- err }()
	select {
	case <-loaded:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := journal.Append(ctx, closeTrade("late-backdated", 5, -50, t0.Add(time.Hour)), nil); err != nil {
		t.Fatal(err)
	}
	current := manualReconciler(t, store, load, &capturePublisher{})
	newer, err := current.Reconcile(ctx, sub)
	if err != nil || newer.Outcome != OutcomeBreaks || newer.Basis.JournalPosition != 2 {
		t.Fatalf("scheduled: %+v %v", newer, err)
	}
	close(resume)
	if err := <-done; !errors.Is(err, ErrBookBasisChanged) {
		t.Fatalf("stale comparator: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM custody_runs").Scan(&count); err != nil || count != 1 {
		t.Fatal("invalidated manual request consumed its key")
	}
	fresh, err := current.ReconcileManual(ctx, sub, request, "operator")
	if err != nil || fresh.Outcome != OutcomeBreaks || fresh.Basis.JournalPosition != 2 {
		t.Fatalf("manual retry: %+v %v", fresh, err)
	}
}

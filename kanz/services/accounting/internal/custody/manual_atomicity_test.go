package custody

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/google/uuid"
)

// pauseRead lets statement ingestion commit after the missing read, before the
// comparison takes the same pair lock as scheduled reconciliation.
type pausedManualStatement struct {
	*Postgres
	observed, resume chan struct{}
}

func (p *pausedManualStatement) LatestStatement(ctx context.Context, s Subject) (Statement, error) {
	st, err := p.Postgres.LatestStatement(ctx, s)
	close(p.observed)
	select {
	case <-p.resume:
	case <-ctx.Done():
		return Statement{}, ctx.Err()
	}
	return st, err
}

func TestManualAbsentReadCannotOverwriteArrivedStatement(t *testing.T) {
	const tenant = "manual-absence"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 15*time.Second)
	defer cancel()
	store := NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	paused := &pausedManualStatement{Postgres: store, observed: make(chan struct{}), resume: make(chan struct{})}
	load := LedgerBookLoader(ledger.NewPostgres(pool), closeScope(t, true), testLogger())
	slower := manualReconciler(t, paused, load, &capturePublisher{})
	key := uuid.NewString()
	done := make(chan error, 1)
	go func() { _, err := slower.ReconcileManual(ctx, sub, key, "operator"); done <- err }()
	select {
	case <-paused.observed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	arrivedID := manualSignedFeed(t, ctx, store)(0, 0)
	close(paused.resume)
	if err := <-done; !errors.Is(err, ErrStatementChanged) {
		t.Fatalf("stale absence: %v", err)
	}
	pub := &capturePublisher{err: errors.New("broker offline")}
	live := manualReconciler(t, store, load, pub)
	first, err := live.ReconcileManual(ctx, sub, key, "operator")
	if !errors.Is(err, ErrPublicationPending) || first.Outcome != OutcomeClean || first.StatementID != arrivedID {
		t.Fatalf("pending completion: %+v %v", first, err)
	}
	pub.err = nil
	second, err := live.ReconcileManual(ctx, sub, key, "operator")
	if err != nil || second.RunID != first.RunID || second.Actor != first.Actor || !second.CompletedAt.Equal(first.CompletedAt) {
		t.Fatalf("retry: %+v %v", second, err)
	}
	var runs, events int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM custody_runs),(SELECT count(*) FROM outbox WHERE causation_id=$1 AND published_at IS NOT NULL)", key).Scan(&runs, &events); err != nil || runs != 1 || events != 1 {
		t.Fatalf("runs=%d published=%d err=%v", runs, events, err)
	}
}

func TestManualReceiptWriteRefusalRollsBackLifecycle(t *testing.T) {
	const tenant = "manual-readonly"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	store := NewPostgres(pool)
	journal := ledger.NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	if err := journal.Append(ctx, closeTrade("ten", 10, -100, t0), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStatement(ctx, statement("original", map[string]int64{"AAPL": 8}, map[string]int64{"USD": -100})); err != nil {
		t.Fatal(err)
	}
	// This additional policy permits scheduled records but refuses a manual
	// snapshot. Production policy ownership stays with the deployment profile.
	if _, err := pool.Exec(ctx, `CREATE POLICY fixture_no_manual_receipt ON custody_runs AS RESTRICTIVE FOR INSERT WITH CHECK (run_id NOT LIKE 'manual:%' OR manual_receipt IS NULL)`); err != nil {
		t.Fatal(err)
	}
	r := manualReconciler(t, store, LedgerBookLoader(journal, closeScope(t, true), testLogger()), &capturePublisher{})
	prior, err := r.Reconcile(ctx, sub)
	if err != nil || prior.Outcome != OutcomeBreaks || len(prior.Breaks) != 1 {
		t.Fatalf("scheduled: %+v %v", prior, err)
	}
	manualSignedFeed(t, ctx, store)(10, -100)
	key := uuid.NewString()
	got, err := r.ReconcileManual(ctx, sub, key, "operator")
	if err == nil || got.RunID != "" {
		t.Fatalf("uncommitted result escaped: %+v %v", got, err)
	}
	var runs, events int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM custody_runs),(SELECT count(*) FROM outbox WHERE event_type=$1)", SubjectRun).Scan(&runs, &events); err != nil || runs != 1 || events != 1 {
		t.Fatalf("runs=%d facts=%d err=%v", runs, events, err)
	}
	b, err := store.LoadBreak(ctx, prior.Breaks[0].BreakID)
	if err != nil || b.Status != BreakOpen || b.Revision != prior.Breaks[0].Revision {
		t.Fatalf("refusal changed break: %+v %v", b, err)
	}
}

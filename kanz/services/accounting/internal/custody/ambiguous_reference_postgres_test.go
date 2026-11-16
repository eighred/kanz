package custody

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func TestPostgresAmbiguousExecutionReferenceKeepsLifecycleAndCompletion(t *testing.T) {
	const tenant = "ambiguous-reference"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	store := NewPostgres(pool)
	if err := store.SaveStatement(ctx, ambiguousReferenceStatement()); err != nil {
		t.Fatal(err)
	}
	prior := investigatedReferenceBreaks(t, ctx, store)
	pub := &capturePublisher{}
	r, err := NewReconciler(store, ambiguousReferenceBook(t, ctx, ledger.NewPostgres(pool)), pub, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, recon.ErrAmbiguousExecutionReference) || errors.Is(err, ErrPublicationPending) || run.Outcome != OutcomeFailed || len(run.Breaks) != 0 {
		t.Fatalf("ambiguous journal was assessed: %+v %v", run, err)
	}
	assertReferenceLifecycle(t, ctx, store, prior)
	assertReferenceRunRows(t, ctx, store, 1)
	stored, err := store.LatestRun(ctx, "PF1", "CUST-A")
	if err != nil || stored.Outcome != OutcomeFailed || stored.FailureReason != run.FailureReason {
		t.Fatalf("failure history missing: %+v %v", stored, err)
	}
	// Another process sees the same immutable failure. A retry cannot turn its
	// earlier unassessed comparison into a CLEAN run or resolve operator work.
	retry := run
	retry.Outcome, retry.FailureReason = OutcomeClean, ""
	repeated, err := NewPostgres(pool).CommitRun(ctx, retry)
	if err != nil || repeated.Outcome != OutcomeFailed || repeated.FailureReason != run.FailureReason {
		t.Fatalf("retry replaced immutable failure: %+v %v", repeated, err)
	}
	assertReferenceLifecycle(t, ctx, store, prior)
	assertReferenceRunRows(t, ctx, store, 1)
	if len(pub.events) != 1 {
		t.Fatal("failure completion was not published once")
	}
}

func assertReferenceRunRows(t *testing.T, ctx context.Context, store *Postgres, published int) {
	t.Helper()
	var runs, facts, delivered, actions int
	if err := store.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM custody_runs),
		(SELECT count(*) FROM outbox WHERE event_type=$1),
		(SELECT count(published_at) FROM outbox WHERE event_type=$1),
		(SELECT count(*) FROM custody_actions)`, SubjectRun).Scan(&runs, &facts, &delivered, &actions); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || facts != 1 || delivered != published || actions != 3 {
		t.Fatalf("failed comparison duplicated/missed durable state: runs=%d facts=%d delivered=%d actions=%d", runs, facts, delivered, actions)
	}
}

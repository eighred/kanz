package custody

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type observedSchedulerStore struct {
	*Postgres
	afterCommit func(Run, error)
}

func (s *observedSchedulerStore) CommitRun(ctx context.Context, run Run, checks ...func(context.Context, bus.Event) error) (Run, error) {
	stored, err := s.Postgres.CommitRun(ctx, run, checks...)
	s.afterCommit(stored, err)
	return stored, err
}

func TestSchedulerRetriesUncommittedSignedPairContention(t *testing.T) {
	const tenant = "custody-scheduler-restart"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	store := NewPostgres(pool)
	subject := Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: BusinessDay(t0)}
	id := manualSignedFeed(t, ctx, store)(1, 0)
	// Hold the same shared transaction lock as saveStatement, on an independent
	// connection. Only an observed, real failed commit may release this lock.
	held, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(ctx) }()
	if _, err := held.Exec(ctx, "SELECT pg_advisory_xact_lock_shared("+custodyPairLockKeySQL+")", pairKey(subject.PortfolioID, subject.CustodianID)); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	var finished Run
	observed := &observedSchedulerStore{Postgres: store, afterCommit: func(got Run, err error) {
		attempts++
		if attempts == 1 {
			if !errors.Is(err, ErrRunBusy) || got.RunID != "" {
				t.Fatalf("expected uncommitted busy comparison; got %q %v", got.RunID, err)
			}
			var runs, facts, breaks int
			if e := held.QueryRow(ctx, `SELECT (SELECT count(*) FROM custody_runs),(SELECT count(*) FROM outbox WHERE event_type=$1),(SELECT count(*) FROM custody_breaks)`, SubjectRun).Scan(&runs, &facts, &breaks); e != nil || runs != 0 || facts != 0 || breaks != 0 {
				t.Fatalf("busy commit persisted runs=%d FACTs=%d breaks=%d: %v", runs, facts, breaks, e)
			}
			if e := held.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			return
		}
		if attempts != 2 || err != nil || got.Outcome != OutcomeBreaks || got.StatementID != id {
			t.Fatalf("unexpected retry %d: %v %v", attempts, got, err)
		}
		finished = got
	}}
	// A ticking clock also gives separate scheduled attempts separate candidate
	// run IDs, so an accidental second commit cannot be hidden by idempotency.
	now := t0.AddDate(0, 0, 1)
	clock := func() time.Time { now = now.Add(time.Millisecond); return now }
	pub := &capturePublisher{}
	r, err := NewReconciler(observed, LedgerBookLoader(ledger.NewPostgres(pool), closeScope(t, true), testLogger()), pub, nil, nil, testLogger(), clock)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(r, SchedulerConfig{Pairs: []Subject{subject}, Interval: time.Hour, LagDays: 1}, testLogger(), clock)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(ctx)
	if attempts != 2 || finished.RunID == "" || len(finished.Breaks) != 1 || finished.Breaks[0].Key != "AAPL" || finished.Breaks[0].Diff.Cmp(big.NewRat(-1, 1)) != 0 || len(pub.events) != 1 || pub.events[0].CorrelationID != finished.RunID {
		t.Fatalf("attempts=%d comparison=%v published=%d", attempts, finished, len(pub.events))
	}
	var receipt, body []byte
	// Scan exactly one row: neither a failed run nor a second successful run/outbox
	// is acceptable after this single scheduler tick.
	var rows, facts int
	err = pool.QueryRow(ctx, `SELECT r.manual_receipt,o.payload,(SELECT count(*) FROM custody_runs),(SELECT count(*) FROM outbox WHERE event_type=$2)
   FROM custody_runs r JOIN outbox o ON o.tenant_id=r.tenant_id AND o.correlation_id=r.run_id
   WHERE r.run_id=$1 AND o.event_type=$2 AND o.published_at IS NOT NULL`, finished.RunID, SubjectRun).Scan(&receipt, &body, &rows, &facts)
	if err != nil || rows != 1 || facts != 1 || !bytes.Equal(receipt, body) {
		t.Fatalf("durable runs=%d FACTs=%d err=%v", rows, facts, err)
	}
	saved, err := runFromRecord(receipt)
	if err != nil || saved.RunID != finished.RunID || saved.StatementID != id || saved.Outcome != OutcomeBreaks || !saved.Subject.BusinessDate.Equal(subject.BusinessDate) {
		t.Fatalf("saved comparison=%v err=%v", saved, err)
	}
}

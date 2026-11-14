package custody

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

func TestPostgresNewestStatementAmbiguity(t *testing.T) {
	const tenant = "custody-statement-ambiguity"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	store := NewPostgres(pool)
	for _, sameValue := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("same_value=%t/reverse=%t", sameValue, reverse), func(t *testing.T) {
				first := statement("first-"+t.Name(), map[string]int64{"AAPL": 90}, nil)
				second := statement("second-"+t.Name(), map[string]int64{"AAPL": 100}, nil)
				first.PortfolioID, second.PortfolioID = t.Name(), t.Name()
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
				if got, err := store.LatestStatement(t.Context(), first.Subject()); !errors.Is(err, ErrAmbiguousStatement) || got.StatementID != "" {
					t.Fatalf("database invented precedence: %+v %v", got, err)
				}
				var rows int
				if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM custody_statements WHERE portfolio_id=$1`, first.PortfolioID).Scan(&rows); err != nil || rows != 2 {
					t.Fatalf("source history changed: rows=%d err=%v", rows, err)
				}
			})
		}
	}
	t.Run("stored_microsecond_collision", func(t *testing.T) {
		first := statement("submicro-first", map[string]int64{"AAPL": 90}, nil)
		second := statement("submicro-second", map[string]int64{"AAPL": 100}, nil)
		first.PortfolioID, second.PortfolioID = "SUBMICRO-PF", "SUBMICRO-PF"
		first.ReceivedAt, second.ReceivedAt = t0.Add(100*time.Nanosecond), t0.Add(900*time.Nanosecond)
		for _, s := range []Statement{first, second} {
			if err := store.SaveStatement(t.Context(), s); err != nil {
				t.Fatal(err)
			}
		}
		var receiptTimes int
		if err := pool.QueryRow(t.Context(), `SELECT count(DISTINCT received_at) FROM custody_statements WHERE portfolio_id=$1`, first.PortfolioID).Scan(&receiptTimes); err != nil || receiptTimes != 1 {
			t.Fatalf("fixture did not establish stored receipt collision: times=%d err=%v", receiptTimes, err)
		}
		if got, err := store.LatestStatement(t.Context(), first.Subject()); !errors.Is(err, ErrAmbiguousStatement) || got.StatementID != "" {
			t.Fatalf("stored precision loss became a proven newest receipt: %+v %v", got, err)
		}
	})
	assertStatementAmbiguityRecovery(t, store)
	pool.Close()
	reconnected := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, reconnected)
	if got, err := NewPostgres(reconnected).LatestStatement(t.Context(), Subject{PortfolioID: "SUBMICRO-PF", CustodianID: "CUST-A", BusinessDate: t0}); !errors.Is(err, ErrAmbiguousStatement) || got.StatementID != "" {
		t.Fatalf("restart resolved an unproven receipt order: %+v %v", got, err)
	}
	other := newCustodyPool(t, tenant+"-other")
	if _, err := NewPostgres(other).LatestStatement(t.Context(), subject()); !errors.Is(err, ErrNoStatement) {
		t.Fatalf("another tenant discovered statement history: %v", err)
	}
}

func TestPostgresAmbiguousStatementFailureKeepsLifecycleAndOutbox(t *testing.T) {
	const tenant = "custody-statement-failure"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	store := NewPostgres(pool)
	for _, id := range []string{"first", "second"} {
		if err := store.SaveStatement(ctx, statement(id, nil, nil)); err != nil {
			t.Fatal(err)
		}
	}
	prior := investigatedReferenceBreaks(t, ctx, store)
	r, err := NewReconciler(store, loaderFor(bookWith(nil, nil)), nil, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	failed, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, ErrAmbiguousStatement) || !errors.Is(err, ErrPublicationPending) || failed.Outcome != OutcomeFailed {
		t.Fatalf("ambiguous statement did not commit a pending failure: %+v %v", failed, err)
	}
	assertReferenceRunRows(t, ctx, store, 0)
	assertReferenceLifecycle(t, ctx, store, prior)
	pool.Close()
	reconnected := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, reconnected)
	restarted := NewPostgres(reconnected)
	pub := &capturePublisher{}
	r, err = NewReconciler(restarted, loaderFor(bookWith(nil, nil)), pub, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, ErrAmbiguousStatement) || errors.Is(err, ErrPublicationPending) || repeated.RunID != failed.RunID || repeated.FailureReason != failed.FailureReason {
		t.Fatalf("restart changed the refusal: %+v %v", repeated, err)
	}
	assertAmbiguousStatementFact(t, pub.lastRun(t), failed)
	assertReferenceRunRows(t, ctx, restarted, 1)
	assertReferenceLifecycle(t, ctx, restarted, prior)
	stored, err := restarted.LatestRun(ctx, "PF1", "CUST-A")
	if err != nil || stored.Outcome != OutcomeFailed || stored.FailureReason != failed.FailureReason {
		t.Fatalf("failed run did not survive restart: %+v %v", stored, err)
	}
}

package custody

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func TestCustodyRunBusinessDateOrdering(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		checkCustodyRunDates(t, NewMemoryStore(), testRunContext(t.Context()))
	})
	t.Run("postgres", func(t *testing.T) {
		pool := newCustodyPool(t, "custody-date")
		requireRestrictedCustodyRole(t, pool)
		applyCustodySchema(t, pool)
		checkCustodyRunDates(t, NewPostgres(pool), bus.WithTenantID(t.Context(), "custody-date"))
	})
}

func checkCustodyRunDates(t *testing.T, st Store, ctx context.Context) {
	t.Helper()
	commit := func(run Run) Run {
		t.Helper()
		got, err := st.CommitRun(ctx, run)
		if err != nil {
			t.Fatalf("commit %s: %v", run.RunID, err)
		}
		return got
	}
	first := Run{RunID: "first-clean", Subject: subject(), Outcome: OutcomeClean, CompletedAt: t0, Tolerance: new(big.Rat)}
	commit(first)
	current := first
	current.RunID, current.Outcome = "current-break", OutcomeBreaks
	current.Subject.BusinessDate = first.Subject.BusinessDate.AddDate(0, 0, 1)
	current.CompletedAt = t0.Add(24 * time.Hour)
	current.Breaks = detectedQuantityBreak(current.CompletedAt, 100, 90)
	commit(current)

	stale := first
	stale.RunID, stale.CompletedAt = "late-historical-clean", current.CompletedAt.Add(time.Hour)
	if _, err := st.CommitRun(ctx, stale); !errors.Is(err, ErrHistoricalLifecycle) {
		t.Fatalf("historical clean returned %v", err)
	}
	commit(first) // Replaying an original run may still read its immutable completion.
	b, err := st.LoadBreak(ctx, current.Breaks[0].BreakID)
	if err != nil || b.Status != BreakOpen || b.Revision != 1 || !b.LastSeenAt.Equal(current.CompletedAt) {
		t.Fatalf("historical clean changed the current break: %+v, %v", b, err)
	}
	latest, err := st.LatestRun(ctx, current.Subject.PortfolioID, current.Subject.CustodianID)
	if err != nil || latest.RunID != current.RunID {
		t.Fatalf("historical clean left a run: %+v, %v", latest, err)
	}
	rows, err := st.Outbox().Pending(ctx, pairKey(current.Subject.PortfolioID, current.Subject.CustodianID), 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("historical clean or replay appended a completion: %d, %v", len(rows), err)
	}

	missing := current
	missing.RunID, missing.Outcome, missing.Breaks = "later-no-statement", OutcomeNoStatement, nil
	missing.Subject.BusinessDate = current.Subject.BusinessDate.AddDate(0, 0, 1)
	commit(missing)
	correction := current
	correction.RunID, correction.Outcome, correction.Breaks = "same-date-clean", OutcomeClean, nil
	correction.CompletedAt = stale.CompletedAt.Add(time.Hour)
	commit(correction)
	stale.RunID, stale.Outcome, stale.Breaks = "late-historical-break", OutcomeBreaks, current.Breaks
	if _, err := st.CommitRun(ctx, stale); !errors.Is(err, ErrHistoricalLifecycle) {
		t.Fatalf("historical reopening returned %v", err)
	}
	b, err = st.LoadBreak(ctx, current.Breaks[0].BreakID)
	if err != nil || b.Status != BreakResolved {
		t.Fatalf("current correction lost: %+v, %v", b, err)
	}
	independent := first
	independent.RunID, independent.Subject.CustodianID = "independent-clean", "CUST-B"
	commit(independent)
}

func TestLateComparatorKeepsCorrectedCustodyStatement(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		checkDelayedComparison(t, NewMemoryStore(), testRunContext(t.Context()))
	})
	t.Run("postgres", func(t *testing.T) {
		pool := newCustodyPool(t, "custody-correction")
		requireRestrictedCustodyRole(t, pool)
		applyCustodySchema(t, pool)
		checkDelayedComparison(t, NewPostgres(pool), bus.WithTenantID(t.Context(), "custody-correction"))
	})
}

func checkDelayedComparison(t *testing.T, st Store, ctx context.Context) {
	t.Helper()
	observed := statement("statement-original", map[string]int64{"AAPL": 100}, nil)
	if err := st.SaveStatement(ctx, observed); err != nil {
		t.Fatal(err)
	}
	book := bookWith(map[string]int64{"AAPL": 100}, nil)
	makeReconciler := func(load BookLoader, now time.Time) *Reconciler {
		t.Helper()
		r, err := NewReconciler(st, load, &capturePublisher{}, nil, nil, testLogger(), func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Let the correction complete inside the first run's book load. This
	// establishes the exact interleaving without relying on sleep or scheduling.
	var correction Run
	older := makeReconciler(func(ctx context.Context, s Subject) (*ledger.Book, []ledger.Execution, error) {
		replacement := statement("statement-corrected", map[string]int64{"AAPL": 90}, nil)
		replacement.ReceivedAt = observed.ReceivedAt.Add(time.Minute)
		if err := st.SaveStatement(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		var err error
		correction, err = makeReconciler(loaderFor(book), t0.Add(2*time.Minute)).Reconcile(ctx, s)
		if err != nil || correction.Outcome != OutcomeBreaks || len(correction.Breaks) != 1 {
			t.Fatalf("correction: %+v, %v", correction, err)
		}
		return book, nil, nil
	}, t0)
	failed, err := older.Reconcile(ctx, observed.Subject())
	if !errors.Is(err, ErrStatementChanged) || failed.Outcome != OutcomeFailed {
		t.Fatalf("stale comparison: %+v, %v", failed, err)
	}
	b, err := st.LoadBreak(ctx, correction.Breaks[0].BreakID)
	if err != nil || b.Status != BreakOpen || b.Revision != 1 || !equalFigure(b.Diff, big.NewRat(10, 1)) {
		t.Fatalf("correction lost: %+v, %v", b, err)
	}
}

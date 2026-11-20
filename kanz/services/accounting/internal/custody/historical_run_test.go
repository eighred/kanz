package custody

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
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

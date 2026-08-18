package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
)

// THE MEMORY AND POSTGRES STORES MUST AGREE ABOUT WHAT LAPSED MEANS (#563).
//
// The seam exists so the double cannot accept what the database refuses. A
// memory Lapsed that ignored expiry, or a Purge that removed a live proposal,
// would pass every server test in this repository — the same shape fakeBus
// already taught this codebase to distrust.

func lapsedFixture(t *testing.T) (*MemoryProposals, context.Context, time.Time) {
	t.Helper()
	return NewMemoryProposals(), context.Background(), pgNow
}

// A LAPSED PROPOSAL IS THE ONE THAT EXPIRED WITHOUT A SIGNATURE. It is not
// merely "not pending": a CLAIMED proposal is not pending either, and it is not
// lapsed — it was decided, and Claim deleted it.
func TestMemoryLapsedIsExpiredAndUnclaimed(t *testing.T) {
	ps, ctx, now := lapsedFixture(t)
	if err := ps.Put(ctx, proposalFor(t, "p-live", "E1", "alice@kanz", "130")); err != nil {
		t.Fatal(err)
	}
	after := now.Add(dualcontrol.DefaultTTL + time.Hour)

	pending, err := ps.Pending(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	lapsed, err := ps.Lapsed(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("an expired proposal is still pending: %+v", pending)
	}
	if len(lapsed) != 1 || lapsed[0].ID != "p-live" {
		t.Fatalf("lapsed = %+v, want the expired proposal — an expired row that appears in NEITHER "+
			"list is the silent drop this change removes", lapsed)
	}

	// AND A CLAIMED ONE IS IN NEITHER LIST, because Claim deleted it. The durable
	// record of a decision is the exception_overrides row, not this table.
	if ok, err := ps.Claim(ctx, "p-live"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if lapsed, err = ps.Lapsed(ctx, after); err != nil || len(lapsed) != 0 {
		t.Fatalf("a claimed proposal is listed as lapsed (%+v, err=%v) — an approved override would "+
			"read as one nobody signed", lapsed, err)
	}
}

// PURGE REMOVES ONLY WHAT IS PAST RETENTION, and the boundary is the interesting
// part: a proposal that lapsed one minute ago is what the proposer came back to
// see.
func TestMemoryPurgeSparesWhatIsStillWithinRetention(t *testing.T) {
	ps, ctx, now := lapsedFixture(t)
	for _, id := range []string{"p-old", "p-recent"} {
		if err := ps.Put(ctx, proposalFor(t, id, "E1", "alice@kanz", "130")); err != nil {
			t.Fatal(err)
		}
	}
	// p-old lapsed long ago; both share a TTL, so age it by moving the cutoff.
	expiry := now.Add(dualcontrol.DefaultTTL)

	// A cutoff BEFORE both expiries removes nothing.
	n, err := ps.PurgeLapsed(ctx, expiry.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("purged %d proposals that had not lapsed yet — a purge that can take a LIVE "+
			"proposal deletes a second signature somebody was about to give", n)
	}
	if pending, _ := ps.Pending(ctx, now); len(pending) != 2 {
		t.Fatalf("pending = %d, want 2 — the purge took something", len(pending))
	}

	// A cutoff after them takes both.
	if n, err = ps.PurgeLapsed(ctx, expiry.Add(time.Hour)); err != nil || n != 2 {
		t.Fatalf("purged %d (err=%v), want 2", n, err)
	}
	if lapsed, _ := ps.Lapsed(ctx, expiry.Add(time.Hour)); len(lapsed) != 0 {
		t.Fatalf("lapsed = %+v after a purge past retention", lapsed)
	}
}

// EVERY STORED PROPOSAL IS IN EXACTLY ONE LIST. This is the invariant the whole
// change rests on, and it is the one a reader cannot check by inspection:
// Pending and Lapsed are separate predicates in separate statements — in
// Postgres, separate SQL — so nothing but a test stops them drifting into a gap
// or an overlap.
//
// A GAP IS THE ORIGINAL DEFECT RETURNING. A proposal in neither list is exactly
// the silent drop #563 exists to remove, and it would look like a fix that
// works: the lists would still be populated and correct-looking.
//
// AN OVERLAP IS WORSE THAN UNTIDY. The same proposal listed as pending AND
// lapsed sends an approver to sign something that can no longer be signed.
func TestPendingAndLapsedPartitionTheStore(t *testing.T) {
	ps, ctx, now := lapsedFixture(t)
	const n = 5
	for i := 0; i < n; i++ {
		if err := ps.Put(ctx, proposalFor(t, fmt.Sprintf("p-%d", i), "E1", "alice@kanz", "130")); err != nil {
			t.Fatal(err)
		}
	}

	// Sweep the clock across the expiry boundary: before, exactly on it, and
	// after. The boundary instant is where a > / >= mismatch between the two
	// predicates would show and nowhere else.
	expiry := now.Add(dualcontrol.DefaultTTL)
	for _, at := range []struct {
		name string
		when time.Time
	}{
		{"before any expiry", now},
		{"one tick before the boundary", expiry.Add(-time.Nanosecond)},
		{"exactly on the boundary", expiry},
		{"after every expiry", expiry.Add(time.Hour)},
	} {
		t.Run(at.name, func(t *testing.T) {
			pending, err := ps.Pending(ctx, at.when)
			if err != nil {
				t.Fatal(err)
			}
			lapsed, err := ps.Lapsed(ctx, at.when)
			if err != nil {
				t.Fatal(err)
			}

			seen := map[string]int{}
			for _, p := range pending {
				seen[p.ID]++
			}
			for _, p := range lapsed {
				seen[p.ID]++
			}
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("p-%d", i)
				switch seen[id] {
				case 1: // exactly right
				case 0:
					t.Errorf("%s is in NEITHER list at %v — that is the silent drop this change "+
						"removes, reintroduced between two predicates that must be inverses", id, at.when)
				default:
					t.Errorf("%s is in BOTH lists at %v — an approver would be sent to sign a "+
						"proposal that can no longer be signed", id, at.when)
				}
			}
		})
	}
}

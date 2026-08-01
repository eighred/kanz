package mark_test

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/mark"
)

// fold folds one trade at `at` and returns the source, advancing the clock via
// the pointer the caller holds.
func foldTrade(t *testing.T, s *mark.Source, instrument string, coeff int64, at time.Time) {
	t.Helper()
	if err := s.Handle(context.Background(), env("market.crypto.trade"),
		tradeEvent(t, instrument, dec(coeff, 0), at)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// AN EXPIRED MARK RELEASES ITS PRICE AND KEEPS ITS TIMESTAMP (#96).
//
// The fold held every instrument it had ever seen, forever: maxAge gated reads
// but never deleted, so a mark that expired an hour ago still owned a *big.Rat.
// Tombstoning releases the value while keeping asOf, which is the half anything
// actually reads.
func TestAnExpiredMarkIsTombstoned(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second)

	foldTrade(t, s, "BTC-USD", 100, base)
	if got := s.Mark("BTC-USD"); got == nil {
		t.Fatal("a fresh mark is not readable")
	}
	if held, live := s.Stats(); held != 1 || live != 1 {
		t.Fatalf("Stats = (held %d, live %d), want (1, 1)", held, live)
	}

	// Expire it, then fold something else so a sweep runs.
	now = base.Add(time.Hour)
	foldTrade(t, s, "ETH-USD", 50, now)

	price, asOf, seen := s.Lookup("BTC-USD")
	if !seen {
		t.Fatal("the tombstoned instrument reports NEVER SEEN — a stalled feed would now be " +
			"indistinguishable from a cold instrument, which is the outage signal this fold owes an operator")
	}
	if !asOf.Equal(base) {
		t.Fatalf("asOf = %v, want the original event time %v — the age of a stalled feed is the diagnosis", asOf, base)
	}
	if price != nil {
		t.Fatal("the expired mark still holds its price — the sweep released nothing")
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil for an expired mark", got)
	}

	held, live := s.Stats()
	if held != 2 {
		t.Fatalf("held = %d, want 2 — a tombstone is still a held instrument, and the gauge must say so", held)
	}
	if live != 1 {
		t.Fatalf("live = %d, want 1 — only the fresh mark is usable", live)
	}
}

// THE DISTINCTION THE OMS BRANCHES ON MUST SURVIVE TOMBSTONING.
//
// services/oms/cmd/oms/main.go emits kanz_compliance_unpriced_orders_total with
// reason="expired" vs reason="never_seen" from exactly this, and calls them "an
// outage" and "a warm-up". Plain deletion would have reported every stalled feed
// as a cold instrument. This is the regression test for that.
func TestTombstonedAndNeverSeenStayDistinguishable(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second)

	foldTrade(t, s, "STALLED", 100, base)
	now = base.Add(time.Hour)
	foldTrade(t, s, "FRESH", 50, now) // drives the sweep

	if _, _, seen := s.Lookup("STALLED"); !seen {
		t.Error(`a stalled feed reports as never_seen — the OMS would log "a cold pod warming up" for an outage`)
	}
	if _, _, seen := s.Lookup("NEVER-TRADED"); seen {
		t.Error("an instrument never folded reports as seen")
	}
}

// A CALLER THAT CHOSE NON-EXPIRING MARKS IS NEVER SWEPT.
//
// maxAge <= 0 means "never expires" — tv-sync passes it because a P&L display
// degrades gracefully on a stale mark. There is nothing to tombstone there, and
// releasing a price it still reads would be a silent data loss.
func TestNonExpiringMarksAreNeverTombstoned(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 0)

	foldTrade(t, s, "BTC-USD", 100, base)
	now = base.Add(9000 * time.Hour)
	foldTrade(t, s, "ETH-USD", 50, now)

	if got := s.Mark("BTC-USD"); got == nil {
		t.Fatal("a mark was expired under maxAge <= 0, which means NEVER expires")
	}
	price, _, seen := s.Lookup("BTC-USD")
	if !seen || price == nil {
		t.Fatal("a non-expiring mark was tombstoned — its value is still read")
	}
	if held, live := s.Stats(); held != 2 || live != 2 {
		t.Fatalf("Stats = (held %d, live %d), want (2, 2) — nothing expires here", held, live)
	}
}

// The sweep is amortised, not per-tick: it walks the map at most once per
// sweepInterval, under the write lock Handle already holds. A price spine
// delivering thousands of ticks a second must not walk every instrument on each
// one. Verified behaviourally — a second fold inside the interval must not
// release a mark that expired between them.
func TestTheSweepIsAmortisedNotPerFold(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, time.Second)

	foldTrade(t, s, "OLD", 100, base) // sweeps (first fold sets lastSweep)
	now = base.Add(2 * time.Second)   // OLD is now expired
	foldTrade(t, s, "NEW", 50, now)   // within sweepInterval of the first: no sweep

	if price, _, _ := s.Lookup("OLD"); price == nil {
		t.Fatal("the sweep ran on a fold inside sweepInterval — it is walking the map per tick")
	}
	// Mark still refuses it regardless: expiry is a read-time check, and the
	// tombstone is only about releasing memory.
	if got := s.Mark("OLD"); got != nil {
		t.Fatalf("Mark = %v, want nil — an unswept expired mark must still be refused", got)
	}
}

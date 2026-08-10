package ratelimit

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pinned returns a limiter with a controllable clock.
func pinned(t *testing.T, o Options) (*Limiter, func(time.Duration)) {
	t.Helper()
	o.Logger = quiet()
	l := New(o)
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return at }
	return l, func(d time.Duration) { at = at.Add(d) }
}

func TestBurstIsSpentThenRefused(t *testing.T) {
	l, _ := pinned(t, Options{Burst: 3, Refill: time.Second})
	for i := range 3 {
		if !l.Allow("s:alice") {
			t.Fatalf("attempt %d refused inside the burst", i+1)
		}
	}
	if l.Allow("s:alice") {
		t.Fatal("a fourth attempt was allowed — the burst is not a bound")
	}
}

func TestOneKeyDoesNotSpendAnother(t *testing.T) {
	l, _ := pinned(t, Options{Burst: 2, Refill: time.Second})
	l.Allow("s:alice")
	l.Allow("s:alice")
	if l.Allow("s:alice") {
		t.Fatal("alice's budget was not exhausted")
	}
	if !l.Allow("s:bob") {
		t.Fatal("bob was refused because alice guessed.\n\n" +
			"Shared buckets mean one account's attacker locks out every other operator, " +
			"which turns the limiter itself into the denial of service.")
	}
}

func TestTokensRefillOverTime(t *testing.T) {
	l, advance := pinned(t, Options{Burst: 2, Refill: 5 * time.Second})
	l.Allow("k")
	l.Allow("k")
	if l.Allow("k") {
		t.Fatal("budget not exhausted")
	}
	advance(5 * time.Second)
	if !l.Allow("k") {
		t.Fatal("no token after one refill interval")
	}
	if l.Allow("k") {
		t.Fatal("two tokens after ONE refill interval — the refill rate is wrong, " +
			"which makes sustained guessing cheaper than it is meant to be")
	}
}

// THE KEY SET IS BOUNDED, AND PAST THE CAP IT REFUSES.
//
// Keys come from the request, so an attacker chooses them. A limiter that grows
// a bucket per distinct guess is the cheapest way to exhaust the process, and a
// limiter that admits unknown keys once full has stopped limiting exactly when
// it matters.
func TestTheKeySetIsCappedAndRefusesPastIt(t *testing.T) {
	l, _ := pinned(t, Options{Burst: 1, Refill: time.Hour, MaxKeys: 10})
	for i := range 10 {
		if !l.Allow(string(rune('a'+i)) + ":first") {
			t.Fatalf("key %d refused below the cap", i)
		}
	}
	if l.Len() != 10 {
		t.Fatalf("tracked %d keys, want 10", l.Len())
	}
	if l.Allow("an-entirely-new-key") {
		t.Fatal("a new key was admitted past the cap — the key set is unbounded, so the " +
			"limiter is the cheapest denial-of-service in the estate")
	}
	if l.Len() != 10 {
		t.Fatalf("tracked %d keys after the refusal, want 10 — the refused key was stored anyway", l.Len())
	}
}

// A KEY ALREADY TRACKED STILL WORKS AT CAPACITY. The cap refuses NEW keys; an
// operator mid-attempt must not be cut off because someone else is spraying.
func TestAtCapacityAnExistingKeyIsStillServed(t *testing.T) {
	l, _ := pinned(t, Options{Burst: 5, Refill: time.Second, MaxKeys: 2})
	if !l.Allow("s:alice") || !l.Allow("s:bob") {
		t.Fatal("setup: both keys should be admitted")
	}
	if l.Allow("s:mallory") {
		t.Fatal("setup: the cap should refuse a third key")
	}
	if !l.Allow("s:alice") {
		t.Fatal("alice, already tracked and inside her burst, was refused because the table is full")
	}
}

// SWEEPING FORGETS ONLY WHAT CARRIES NO INFORMATION. A full, idle bucket is
// indistinguishable from one that never existed, so dropping it cannot weaken
// the limit — but dropping a PARTIALLY SPENT one would hand an attacker a reset.
func TestSweepDropsIdleFullBucketsAndKeepsSpentOnes(t *testing.T) {
	// Refill is 10m and the idle window is 10m, so 11m returns ~1.1 tokens:
	// enough to top "idle" (1 spent of 2) back to full, NOT enough to refill
	// "spent" (2 of 2) — which is exactly the distinction being asserted.
	l, advance := pinned(t, Options{Burst: 2, Refill: 10 * time.Minute, IdleAfter: 10 * time.Minute})
	l.Allow("idle")  // 1 of 2 spent → refills to full below
	l.Allow("spent") // both tokens gone → still partially spent below
	l.Allow("spent")

	advance(11 * time.Minute)
	l.Sweep()
	if l.Len() != 1 {
		t.Fatalf("tracked %d keys after sweep, want 1 (the idle full one dropped)", l.Len())
	}

	// And a sweep must never resurrect budget for a key still being guessed.
	l2, adv2 := pinned(t, Options{Burst: 2, Refill: time.Hour, IdleAfter: time.Minute})
	l2.Allow("victim")
	l2.Allow("victim")
	adv2(2 * time.Minute) // idle, but NOT refilled (refill is an hour)
	l2.Sweep()
	if l2.Allow("victim") {
		t.Fatal("a spent bucket was swept away and the key got a fresh burst.\n\n" +
			"That is a free reset on a timer: an attacker paces guesses to the sweep " +
			"interval and the limit never applies.")
	}
}

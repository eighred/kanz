package volprofile

import (
	"runtime"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pit"
)

// THE HORIZON RELEASES WHAT IT DROPS (#862).
//
// # Why a finalizer and not a length assertion
//
// TestRetentionDropsSessionsPastTheHorizon asserts on the retained COUNT, and a
// count cannot see this defect. Pruning by reslicing hands back a slice over the
// SAME backing array, and Go keeps an entire array alive while any slice
// references any part of it — so the dropped sessions are gone from the slice and
// still on the heap, released only when a later append happens to reallocate.
//
// That is exactly what #862 was, in the pricing stores, where every retention
// assertion was on len and every one of them was correct while the heap grew.
// Here each dropped element carries a session's worth of exact big.Rat volumes,
// one per intraday bucket, per instrument per venue — on market-ingest, whose
// reason for existing is to hold market state in memory.
//
// Reachability is the only thing that can state the property, so these tests
// attach a finalizer to the session a prune is about to drop, force collection
// cycles, and require the finalizer to run.

// gcUntil forces collection cycles until done is closed, or gives up.
//
// MORE THAN ONE CYCLE IS REQUIRED, not defensive padding: a finalizer runs on its
// own goroutine no earlier than the cycle after the one that found the object
// unreachable, so a single runtime.GC() proves nothing either way.
func gcUntil(done <-chan struct{}) bool {
	for i := 0; i < 8; i++ {
		runtime.GC()
		select {
		case <-done:
			return true
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}

// A SESSION DROPPED BY THE HORIZON IS RELEASED, NOT MERELY UNLINKED — AND IT IS
// RELEASED WHILE THE STORE IS STILL SMALL.
//
// The backing array is given capacity for the whole run up front, so append never
// needs to grow it and nothing but the prune itself can release the dropped
// session. An implementation that relies on a reallocation to free the prefix
// fails here while every count-based assertion in this package still passes.
func TestARetiredSessionIsReleasedWithoutWaitingForAReallocation(t *testing.T) {
	const horizonSessions = 3
	const runSessions = 200

	s, err := New(Config{MinSessions: 1, Horizon: horizonSessions * Session})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Open session 0 so the history exists, then give its completed-session list a
	// capacity larger than the run. Each prune reslices, so capacity falls by one
	// per drop; 512 against 200 sessions leaves it far from a reallocation.
	s.Observe(btcBinance, trade(day0.Add(time.Minute), 10))
	h := s.series[btcBinance]
	h.done = make([]pit.Version[*session], 0, 512)

	gone := make(chan struct{})
	func() {
		// Completing session 0 moves it onto h.done, and the store's slice is then
		// the only reference to it.
		s.Observe(btcBinance, trade(day0.Add(Session+time.Minute), 10))
		first := h.done[0]
		if !first.AsOf.Equal(day0) {
			t.Fatalf("h.done[0] starts at %s, want %s — the fixture is not holding the session "+
				"the prune will drop", first.AsOf, day0)
		}
		runtime.SetFinalizer(first.V, func(*session) { close(gone) })
	}()

	for d := 2; d <= runSessions; d++ {
		s.Observe(btcBinance, trade(day0.Add(time.Duration(d)*Session+time.Minute), 10))
	}

	// NON-VACUITY 1: the prune must actually have dropped the first session, or
	// this proves nothing about release. len is the right check here precisely
	// because it is the half that already worked.
	if len(h.done) > horizonSessions+1 {
		t.Fatalf("after %d sessions the store still holds %d — nothing was pruned, so this test "+
			"is not measuring release", runSessions, len(h.done))
	}
	if h.done[0].AsOf.Equal(day0) {
		t.Fatal("the first session is still the head of the store — it was not dropped")
	}
	// NON-VACUITY 2: the backing array must not have been replaced, or a passing
	// run cannot tell the prune apart from a reallocation.
	if cap(h.done) > 512 {
		t.Fatalf("the backing array was reallocated (cap %d) — this test needs it not to be",
			cap(h.done))
	}

	released := gcUntil(gone)
	// THE STORE MUST STAY LIVE ACROSS THE GC. Without this the compiler sees s is
	// never used again, the whole array becomes garbage, and the finalizer fires
	// whatever the prune did — the test would pass against the defect. A real
	// store is held by its owner for the life of the process; this is that
	// condition, stated.
	runtime.KeepAlive(s)
	if released {
		return
	}
	t.Fatal("a session dropped by the horizon survived every GC while the backing array was " +
		"never reallocated — the prune is unlinking the session without releasing it, so the " +
		"horizon bounds the session COUNT while the heap keeps growing. Every retention " +
		"assertion in this package is on len and all of them are blind to it (#862)")
}

// NON-VACUITY FOR THE FINALIZER HARNESS ITSELF. If SetFinalizer or the GC did not
// behave as the test above assumes, it would pass for the wrong reason. A session
// nothing retains must be collected; one the test still holds must not.
func TestTheFinalizerHarnessDistinguishesReachable(t *testing.T) {
	freed := make(chan struct{})
	func() {
		p := &session{start: day0}
		runtime.SetFinalizer(p, func(*session) { close(freed) })
	}()
	if !gcUntil(freed) {
		t.Fatal("an unreferenced session was not collected — the harness cannot detect a leak")
	}

	held := &session{start: day0}
	stillHeld := make(chan struct{})
	runtime.SetFinalizer(held, func(*session) { close(stillHeld) })
	runtime.GC()
	select {
	case <-stillHeld:
		t.Fatal("a session the test still references was collected — the harness would report " +
			"success whatever the prune did")
	case <-time.After(150 * time.Millisecond):
	}
	runtime.KeepAlive(held)
}

package bus_test

import (
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

func TestDedupWindowClaimIsExclusive(t *testing.T) {
	w := bus.NewDedupWindow(time.Minute, 100)
	if !w.Claim("k1") {
		t.Error("first Claim of k1 should succeed")
	}
	if w.Claim("k1") {
		t.Error("second Claim of a held k1 must fail")
	}
	if !w.Claim("k2") {
		t.Error("an unrelated key must still be claimable")
	}
	w.Commit("k1")
	if w.Claim("k1") {
		t.Error("a committed key must stay claimed for the dedup window")
	}
}

// A failed dispatch releases its claim, so the redelivery retries. Without this,
// claim-before-handle would turn every handler failure into a lost event.
func TestDedupWindowReleaseAllowsRetry(t *testing.T) {
	w := bus.NewDedupWindow(time.Minute, 100)
	if !w.Claim("k") {
		t.Fatal("first Claim should succeed")
	}
	w.Release("k")
	if !w.Claim("k") {
		t.Error("a released key must be claimable again")
	}
}

// A worker that claims and dies never commits or releases. The lease must expire
// so the event is reprocessed rather than silently dropped.
func TestDedupWindowLeaseExpiryRecoversAbandonedClaim(t *testing.T) {
	// ttl below the default 5s lease clamps the lease to the ttl, so a 50ms window
	// gives a 50ms lease.
	w := bus.NewDedupWindow(50*time.Millisecond, 100)
	if !w.Claim("k") {
		t.Fatal("first Claim should succeed")
	}
	if w.Claim("k") {
		t.Fatal("claim must be held inside its lease")
	}
	time.Sleep(80 * time.Millisecond)
	if !w.Claim("k") {
		t.Error("an abandoned claim must expire with its lease")
	}
}

func TestDedupWindowEmptyKey(t *testing.T) {
	w := bus.NewDedupWindow(time.Minute, 100)
	// Nothing to dedup on ⇒ the dispatch proceeds, every time.
	if !w.Claim("") {
		t.Error("an empty key must not block the dispatch")
	}
	if !w.Claim("") {
		t.Error("an empty key must never be treated as a duplicate")
	}
	w.Commit("")
	w.Release("")
}

func TestDedupWindowCommitTTLExpiry(t *testing.T) {
	w := bus.NewDedupWindow(50*time.Millisecond, 100)
	if !w.Claim("k") {
		t.Fatal("Claim should succeed")
	}
	w.Commit("k")
	if w.Claim("k") {
		t.Fatal("a committed key must be held inside its TTL")
	}
	time.Sleep(80 * time.Millisecond)
	if !w.Claim("k") {
		t.Error("a key past its dedup window must be claimable again")
	}
}

func TestDedupWindowCapacityEviction(t *testing.T) {
	w := bus.NewDedupWindow(time.Minute, 3)
	for _, k := range []string{"a", "b", "c"} {
		w.Claim(k)
		w.Commit(k)
		time.Sleep(time.Millisecond) // stagger expiries so "a" is the soonest
	}
	w.Claim("d") // forces eviction; "a" expires soonest
	w.Commit("d")

	// Probe the survivors FIRST. A failed Claim is read-only, but a successful one
	// inserts — so claiming the evicted key would itself evict the next survivor.
	for _, k := range []string{"b", "c", "d"} {
		if w.Claim(k) {
			t.Errorf("%s should still be held", k)
		}
	}
	if !w.Claim("a") {
		t.Error("a should have been evicted (expiring soonest) and be claimable again")
	}
}

func TestNewDedupWindowDisabledNilSafe(t *testing.T) {
	// ttl=0 or max=0 ⇒ nil. Methods must no-op on nil receiver so the
	// Consumer can use it unconditionally.
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		max  int
	}{
		{"ttl=0", 0, 100},
		{"max=0", time.Minute, 0},
		{"both", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := bus.NewDedupWindow(tc.ttl, tc.max)
			if w != nil {
				t.Fatalf("expected nil window for %s", tc.name)
			}
			// Dedup disabled ⇒ every delivery proceeds. A nil Claim returning false
			// would silently swallow EVERY event on a consumer with dedup turned off.
			if !w.Claim("k") {
				t.Error("nil Claim returned false — a disabled deduper would drop every event")
			}
			w.Commit("k")  // must not panic
			w.Release("k") // must not panic
		})
	}
}

// The in-instance race the old Seen/Record split could not close: concurrent
// deliveries of one key must yield exactly one winner. Run with -race.
func TestDedupWindowConcurrentClaimHasExactlyOneWinner(t *testing.T) {
	w := bus.NewDedupWindow(time.Minute, 1000)
	const racers = 64

	var mu sync.Mutex
	won := 0
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w.Claim("hot-key") {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if won != 1 {
		t.Fatalf("%d of %d concurrent claimants won, want exactly 1", won, racers)
	}
}

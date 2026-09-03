package arch

import (
	"strings"
	"testing"
)

// A RELEASED PORTFOLIO MUST LEAVE NOTHING BEHIND (#893).
//
// state.Store.Release prunes four maps — owned, portfolios, locks, dedup — when
// a ring rebalances. risk.Cache holds a fifth thing derived from that same
// state, the portfolio's last-known-good exposure and measure sets, written
// after every recompute. Nothing told it, so a replica on a rebalancing ring
// accumulated every portfolio it had EVER owned rather than the ones it owns.
//
// THE ASYMMETRY IS THE DEFECT, and it is invisible by construction: nothing
// errors, no probe fails, and the heap grows with cumulative ownership. The two
// prunings are separated by a package boundary — internal/risk/state sits BELOW
// internal/risk and must not import it — so they are joined by an observer the
// composition root wires. An unwired observer restores the leak exactly, with a
// green build and a green estate-wide map guard, because Release still prunes
// its own four maps.
//
// This holds the join. The map-eviction guard holds the other end: Cache.exposure
// and Cache.measures were carried there as deferredLeak exemptions naming this
// issue, and retiring them is what makes that guard demand an evictor at all.

const (
	handoffStoreRel = "../../internal/risk/state/ownership.go"
	handoffCacheRel = "../../internal/risk/degraded.go"
	handoffMainRel  = "../../services/risk-engine/cmd/risk-engine/main.go"
)

// TestReleaseNotifiesTheReleaseObserver holds the producer end.
func TestReleaseNotifiesTheReleaseObserver(t *testing.T) {
	src := readStripped(t, handoffStoreRel)
	body := funcBody(t, src, "func (s *Store) Release(")

	if !strings.Contains(body, "s.onRelease(") {
		t.Fatal("state.Store.Release no longer notifies its release observer. Release prunes this " +
			"package's four maps; everything DERIVED from that state — risk.Cache's exposure and " +
			"measure sets — is dropped through the observer, and without the call a replica keeps " +
			"the derived copy of every portfolio it has ever owned (#893).")
	}

	// It must fire only on a release that actually happened. Release returns
	// released=false for a portfolio this replica does not hold, because a
	// watcher re-delivering a revocation is normal; notifying there would make
	// the observer's calls stop meaning "a handoff occurred".
	drop := strings.Index(body, "delete(s.owned, id)")
	notify := strings.Index(body, "s.onRelease(")
	if drop < 0 {
		t.Fatal("Release no longer drops ownership — this guard's premise has moved")
	}
	if notify < drop {
		t.Fatal("state.Store.Release notifies the observer BEFORE it drops the portfolio. The " +
			"observer prunes state derived from a portfolio this replica still holds at that " +
			"moment, and every early return above the drop — an unowned portfolio, an empty id — " +
			"would fire it for a handoff that never happened (#893).")
	}
}

// TestTheCacheEvictsBothHalvesTogether holds the consumer end.
func TestTheCacheEvictsBothHalvesTogether(t *testing.T) {
	src := readStripped(t, handoffCacheRel)
	body := funcBody(t, src, "func (c *Cache) Evict(")

	for _, m := range []string{"delete(c.exposure, id)", "delete(c.measures, id)"} {
		if !strings.Contains(body, m) {
			t.Fatalf("Cache.Evict does not %s. A portfolio whose exposure was dropped and whose "+
				"measures were not is a book this replica reports half of, and the two halves are "+
				"read by different callers — nothing downstream would reconcile them (#893).", m)
		}
	}
	if !strings.Contains(body, "c.mu.Lock()") {
		t.Fatal("Cache.Evict does not take the cache's lock. The two deletes must be one critical " +
			"section, or a concurrent read sees a portfolio with measures and no exposure.")
	}
}

// TestTheRiskEngineWiresTheReleaseObserver holds the join.
//
// STRUCTURAL, because the failure is an absence. With the observer unwired
// everything still compiles, Release still prunes its own maps, and the leak is
// back — the exact state the two retired deferredLeak exemptions described.
func TestTheRiskEngineWiresTheReleaseObserver(t *testing.T) {
	src := readStripped(t, handoffMainRel)

	if !strings.Contains(src, "state.WithReleaseObserver(") {
		t.Fatal("the risk-engine composition root no longer wires state.WithReleaseObserver. The " +
			"state store and the cache are pruned by two different packages and only this wiring " +
			"joins them; without it an ownership handoff drops the state and keeps everything " +
			"derived from it, which is the leak #893 recorded and this change closed.")
	}
	if !strings.Contains(src, "cache.Evict(") {
		t.Fatal("the risk-engine composition root wires a release observer that does not call " +
			"cache.Evict. Note the observer is deliberately a CLOSURE calling the method rather " +
			"than the method value `cache.Evict`: the estate-wide map-eviction guard credits an " +
			"evictor only from a call expression, so the method-value form leaves Cache.Evict " +
			"looking like dead code and reports the cache maps as leaking (#893).")
	}

	// The cache must be constructed before the store, or the store cannot be
	// given an observer that closes over it.
	cacheAt := strings.Index(src, "risk.NewCache()")
	storeAt := strings.Index(src, "state.NewStore(")
	switch {
	case cacheAt < 0 || storeAt < 0:
		t.Fatal("the risk-engine composition root no longer builds both a risk.Cache and a " +
			"state.Store — this guard's premise has moved and it must be rewritten")
	case cacheAt > storeAt:
		t.Fatal("risk.NewCache is constructed AFTER state.NewStore, so the store cannot be wired " +
			"with an observer that prunes the cache. The ordering is the wiring (#893).")
	}
}

// TestTheCacheLeakExemptionsAreGone holds the retirement.
//
// An exemption that outlives its repair is a hole nobody is watching, and this
// pair specifically told the estate-wide guard to ignore the two maps this
// change fixed. Leaving them would mean the map guard never demands an evictor
// for the cache again.
func TestTheCacheLeakExemptionsAreGone(t *testing.T) {
	src := readStripped(t, "../../test/arch/long_lived_maps_are_evicted_test.go")
	for _, key := range []string{`"internal/risk: Cache.exposure"`, `"internal/risk: Cache.measures"`} {
		if strings.Contains(src, key) {
			t.Fatalf("the map-eviction guard still exempts %s. #893 is closed: the field now has an "+
				"evictor and the composition root calls it, so the exemption reads as a decision "+
				"somebody made rather than a repair somebody finished.", key)
		}
	}
}

package state_test

// The full handoff, against real Postgres (#893 / #110).
//
// The unit tests either side of this one prove halves: internal/risk proves
// Cache.Evict drops both maps, and release_observer_test.go proves Release fires
// the observer. Neither proves the thing the issue is actually about — that a
// replica which ACQUIRED a portfolio from durable state, computed against it,
// and then RELEASED it, is left holding nothing for that portfolio.
//
// It runs against a real Postgres because the acquire half is a durable read:
// a fake loader would prove the wiring and not that a portfolio genuinely
// arrives from the database and genuinely leaves memory afterwards. This is the
// shape #110's ruling calls locally verifiable — "load-on-demand against real
// Postgres is a test, not a cluster claim".

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

// exposureFor and measuresFor stand in for a completed recompute: the cache only
// ever holds what the orchestrator computed, and what is IN the sets does not
// matter to a test about whether they survive a handoff.
func exposureFor(id v1.PortfolioID) *domain.ExposureSet {
	return domain.NewExposureSet(id, time.Unix(0, 0).UTC(), nil)
}

func measuresFor(id v1.PortfolioID) *domain.MeasureSet {
	return domain.NewMeasureSet(id, time.Unix(0, 0).UTC(), nil)
}

// TestAnOwnershipHandoffLeavesNothingBehind is #893's "Verified when" in one
// test: releasing ownership removes the portfolio's exposure and measure
// entries, and the released portfolio is not readable from the degraded path.
func TestAnOwnershipHandoffLeavesNothingBehind(t *testing.T) {
	pool := pgPool(t, "__system__")
	applyStateSchema(t, pool)
	sink := persist.NewPostgres(pool)
	ctx := context.Background()

	const id v1.PortfolioID = "PORT-HANDOFF-893"
	if err := sink.Save(ctx, durableRecord(id)); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	// The composition root's shape: the cache exists first, and the store
	// releases into it.
	cache := risk.NewCache()
	store := state.NewStore(
		state.WithRuntimeOwnership(sink),
		state.WithReleaseObserver(cache.Evict),
	)

	if err := store.Acquire(ctx, id); err != nil {
		t.Fatalf("Acquire from Postgres: %v", err)
	}
	if _, err := store.SnapshotOwned(id); err != nil {
		t.Fatalf("the portfolio did not arrive from the durable store: %v", err)
	}

	// A recompute lands its last-known-good sets, exactly as the orchestrator
	// does after every successful compute.
	cache.StoreExposure(id, exposureFor(id))
	cache.StoreMeasures(id, measuresFor(id))
	if _, ok := cache.LookupExposure(id); !ok {
		t.Fatal("the cache did not take the computed exposure")
	}

	// THE RING REBALANCES.
	rec, released, err := store.Release(id)
	if err != nil || !released {
		t.Fatalf("Release = (%v, %v), want (nil, true)", err, released)
	}
	if rec.ID != id {
		t.Fatalf("Release handed back %q, want %q — the record the next owner needs", rec.ID, id)
	}

	// 1. NOT RESIDENT. This is the leak the issue names: before #893 the two
	//    entries stayed for the life of the process, so a replica on a
	//    rebalancing ring held every portfolio it had EVER owned.
	if _, ok := cache.LookupExposure(id); ok {
		t.Error("the released portfolio's exposure is still cached. A replica that hands a portfolio " +
			"off keeps its last-known-good set forever, so the heap grows with CUMULATIVE " +
			"ownership rather than current ownership (#893).")
	}
	if _, ok := cache.LookupMeasures(id); ok {
		t.Error("the released portfolio's measures are still cached")
	}

	// 2. NOT READABLE. The state store refuses it, which is the layer that makes
	//    the stale entry unreachable even before it is evicted — and is why this
	//    defect is a leak rather than a wrong number in the posture that would be
	//    releasing. Both halves are asserted so a change to either is visible.
	if _, err := store.SnapshotOwned(id); err == nil {
		t.Error("a released portfolio is still readable from the state store")
	}
	if store.Owns(id) {
		t.Error("the store still claims ownership of a released portfolio")
	}
}

// TestReacquiringAfterAHandoffStartsFromDurableStateNotTheCache proves the
// eviction is not merely cosmetic.
//
// A replica that releases and later re-acquires the same portfolio must rebuild
// from the durable record, not serve the sets it computed before the handoff.
// Between the two, another replica owned the book and moved it; a surviving
// cache entry would be a pre-handoff answer presented as current, which is the
// one way this leak becomes a wrong number rather than only memory.
func TestReacquiringAfterAHandoffStartsFromDurableStateNotTheCache(t *testing.T) {
	pool := pgPool(t, "__system__")
	applyStateSchema(t, pool)
	sink := persist.NewPostgres(pool)
	ctx := context.Background()

	const id v1.PortfolioID = "PORT-REACQ-893"
	if err := sink.Save(ctx, durableRecord(id)); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	cache := risk.NewCache()
	store := state.NewStore(
		state.WithRuntimeOwnership(sink),
		state.WithReleaseObserver(cache.Evict),
	)

	if err := store.Acquire(ctx, id); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	cache.StoreExposure(id, exposureFor(id))
	cache.StoreMeasures(id, measuresFor(id))

	if _, _, err := store.Release(id); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := store.Acquire(ctx, id); err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}

	if _, ok := cache.LookupExposure(id); ok {
		t.Error("a re-acquired portfolio still has its PRE-HANDOFF exposure cached. Another replica " +
			"owned this book in between and may have moved it, so serving that set is a stale " +
			"answer presented as current — the one way this leak becomes a wrong number (#893).")
	}
	if _, ok := cache.LookupMeasures(id); ok {
		t.Error("a re-acquired portfolio still has its pre-handoff measures cached")
	}
	// And the state itself did come back from Postgres.
	if _, err := store.SnapshotOwned(id); err != nil {
		t.Fatalf("re-acquired portfolio is not readable: %v", err)
	}
}

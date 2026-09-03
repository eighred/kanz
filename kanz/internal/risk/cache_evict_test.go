package risk

// Cache eviction on ownership handoff (#893).
//
// The cache is written after every successful recompute and, until #893, had no
// removal path at all. Its key space is estate-controlled — a real portfolio id
// — so this is not the caller-keyed shape of #814. What makes it a leak is
// OWNERSHIP: state.Store.Release already prunes the state store's four maps when
// a ring rebalances, and nothing told the cache, so a long-lived replica held
// every portfolio it had EVER owned rather than the ones it owns.

import (
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

const evictID v1.PortfolioID = "PORT-EVICT"

func seedCache(t *testing.T, id v1.PortfolioID) *Cache {
	t.Helper()
	c := NewCache()
	storeBoth(t, c, id)
	return c
}

func storeBoth(t *testing.T, c *Cache, id v1.PortfolioID) {
	t.Helper()
	c.StoreExposure(id, makeExposure(id, t0))
	c.StoreMeasures(id, domain.NewMeasureSet(id, t0, nil))

	if _, ok := c.LookupExposure(id); !ok {
		t.Fatalf("seed exposure for %q did not land", id)
	}
	if _, ok := c.LookupMeasures(id); !ok {
		t.Fatalf("seed measures for %q did not land", id)
	}
}

// EVICT DROPS BOTH HALVES.
//
// A portfolio whose exposure was dropped and whose measures were not is a book
// this replica reports half of — and the two are read by different callers, so
// nothing downstream would reconcile them.
func TestEvictDropsExposureAndMeasuresTogether(t *testing.T) {
	c := seedCache(t, evictID)
	c.Evict(evictID)

	if _, ok := c.LookupExposure(evictID); ok {
		t.Error("exposure survived Evict — a released portfolio's last-known-good set is still resident")
	}
	if _, ok := c.LookupMeasures(evictID); ok {
		t.Error("measures survived Evict. Dropping one half and not the other leaves this replica " +
			"reporting a book it half-remembers, and the two halves are read by different callers.")
	}
}

// EVICTING ONE PORTFOLIO LEAVES THE OTHERS. The whole point is to keep what this
// replica still owns; an Evict that cleared the maps would turn a handoff into a
// full cache flush, and every remaining portfolio would lose its degraded
// fallback at the moment the ring is already moving.
func TestEvictLeavesOtherPortfoliosAlone(t *testing.T) {
	c := seedCache(t, evictID)
	const keep v1.PortfolioID = "PORT-KEEP"
	storeBoth(t, c, keep)

	c.Evict(evictID)

	if _, ok := c.LookupExposure(keep); !ok {
		t.Error("evicting one portfolio dropped another's exposure")
	}
	if _, ok := c.LookupMeasures(keep); !ok {
		t.Error("evicting one portfolio dropped another's measures")
	}
}

// EVICTING AN UNKNOWN PORTFOLIO IS A NO-OP, not a panic. A membership watcher
// re-delivering a revocation is normal — state.Store.Release says so explicitly,
// and returns released=false rather than an error — so this is reached for
// portfolios this replica never computed.
func TestEvictingAnUnheldPortfolioIsHarmless(t *testing.T) {
	c := NewCache()
	c.Evict("never-seen")
	c.Evict("")
}

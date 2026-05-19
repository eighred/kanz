// Package risk holds the engine's degraded-mode primitives — the
// first .go file at the top-level kanz/internal/risk directory (all
// previous risk work lived in sub-packages). Establishes the `risk`
// package as the natural home for cross-cutting concerns that
// don't belong to any single sub-package (api/v1, domain, ingest,
// state, scenario, compute, publish).
//
// # RISK-11 scope
//
// Two pieces:
//
//   - Detector — staleness assessment. Pure function: given a
//     portfolio's AsOf and the engine's wall clock, return the
//     operating Mode plus the QualityFlags to attach to the
//     response. Used by the api/v1 Engine implementation
//     (orchestrator) to tag every query response.
//
//   - Cache — per-portfolio last-known-good ExposureSet and
//     MeasureSet. The orchestrator stores results after every
//     successful compute; reads serve the cached value when a live
//     compute fails (downstream dependency unavailable, panic
//     recovery, compute budget exceeded). The cached value carries
//     its own AsOf so Detector still tags it correctly.
//
// # Why staleness drives mode, not health checks
//
// The engine has no exotic dependencies whose health it could
// poll (no external risk service, no model server). The only
// observable that matters operationally is "how stale is the
// underlying state?" — measured as `now - portfolio.AsOf`. A
// healthy ingest pipeline keeps AsOf within a small budget; any
// stall (broker outage, applier crash, network partition) shows up
// as growing staleness. Tying mode to staleness directly makes the
// signal cheap and the cause locally diagnosable.
//
// # Default thresholds
//
// `DefaultFreshnessBudget = 30s` and `DefaultDegradedThreshold = 5m`
// are conservative starting points — tighten for high-frequency
// market state, loosen for slow-moving portfolios. Operationally
// they'll be tuned per deployment; the constants here are the
// out-of-the-box defaults a fresh engine starts with.
package risk

import (
	"sync"
	"time"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// Default thresholds — the gap between portfolio.AsOf and now()
// that triggers each level of degraded signalling.
const (
	// DefaultFreshnessBudget: above this gap, responses get
	// QualityFlagStale but the engine remains ModeNormal.
	DefaultFreshnessBudget = 30 * time.Second
	// DefaultDegradedThreshold: above this gap, the engine flips to
	// ModeDegraded and QualityFlagDegraded joins QualityFlagStale.
	DefaultDegradedThreshold = 5 * time.Minute
)

// Detector assesses staleness and returns the (Mode, QualityFlags)
// the api/v1 surface attaches to responses. Construct once per
// engine; safe for concurrent use (fields are immutable after
// construction).
type Detector struct {
	// FreshnessBudget: ≤0 falls back to DefaultFreshnessBudget.
	FreshnessBudget time.Duration
	// DegradedThreshold: ≤0 falls back to DefaultDegradedThreshold.
	DegradedThreshold time.Duration
	now               func() time.Time
}

// NewDetector returns a Detector with the default thresholds.
// Callers tighten/loosen via the public fields or via the
// constants for engine-wide overrides.
func NewDetector() *Detector {
	return &Detector{
		FreshnessBudget:   DefaultFreshnessBudget,
		DegradedThreshold: DefaultDegradedThreshold,
		now:               time.Now,
	}
}

// Assess returns the operating Mode and the QualityFlags for a
// response computed against state with the given AsOf. Zero AsOf
// (no state ever applied for this portfolio) returns ModeDegraded
// + QualityFlagDegraded — there is no fresh data, even a fallback
// to cache would be empty. The Detector itself doesn't decide what
// to DO with the mode (return empty? error? cache-only?) — that's
// the orchestrator's policy.
func (d *Detector) Assess(asOf time.Time) (v1.Mode, []v1.QualityFlag) {
	if asOf.IsZero() {
		return v1.ModeDegraded, []v1.QualityFlag{v1.QualityFlagDegraded}
	}
	fb := d.FreshnessBudget
	if fb <= 0 {
		fb = DefaultFreshnessBudget
	}
	dt := d.DegradedThreshold
	if dt <= 0 {
		dt = DefaultDegradedThreshold
	}
	staleness := d.now().Sub(asOf)
	switch {
	case staleness > dt:
		return v1.ModeDegraded, []v1.QualityFlag{v1.QualityFlagDegraded, v1.QualityFlagStale}
	case staleness > fb:
		return v1.ModeNormal, []v1.QualityFlag{v1.QualityFlagStale}
	default:
		return v1.ModeNormal, nil
	}
}

// Health composes a v1.Health value from the engine's latest
// applied state-event time. Use this from the api/v1 surface's
// Engine.Health implementation.
func (d *Detector) Health(latestAsOf time.Time) v1.Health {
	mode, _ := d.Assess(latestAsOf)
	staleness := time.Duration(0)
	if !latestAsOf.IsZero() {
		staleness = d.now().Sub(latestAsOf)
		if staleness < 0 {
			staleness = 0
		}
	}
	return v1.Health{
		Mode:      mode,
		AsOf:      latestAsOf,
		Staleness: staleness,
	}
}

// Cache holds the last-known-good ExposureSet and MeasureSet per
// portfolio. The orchestrator writes after every successful compute;
// reads serve the cached value when a live compute fails or to
// satisfy a caller's staleness budget without recomputing. The
// cache is best-effort — eviction is implicit (overwrite on next
// store), there is no TTL because the underlying ExposureSet /
// MeasureSet carries its own AsOf and Detector handles staleness.
type Cache struct {
	mu       sync.RWMutex
	exposure map[v1.PortfolioID]*domain.ExposureSet
	measures map[v1.PortfolioID]*domain.MeasureSet
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{
		exposure: make(map[v1.PortfolioID]*domain.ExposureSet),
		measures: make(map[v1.PortfolioID]*domain.MeasureSet),
	}
}

// StoreExposure records the latest computed ExposureSet for the
// portfolio. Replaces any previously cached value — older results
// are not retained (a chronological cache is a different feature;
// degraded fallback only needs the most recent good value).
func (c *Cache) StoreExposure(id v1.PortfolioID, es *domain.ExposureSet) {
	if es == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exposure[id] = es
}

// LookupExposure returns the cached ExposureSet and true, or nil
// and false. The returned ExposureSet's AsOf reports when the
// cached compute was performed — feed it to Detector.Assess to get
// the staleness flags.
func (c *Cache) LookupExposure(id v1.PortfolioID) (*domain.ExposureSet, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	es, ok := c.exposure[id]
	return es, ok
}

// StoreMeasures records the latest computed MeasureSet for the
// portfolio.
func (c *Cache) StoreMeasures(id v1.PortfolioID, ms *domain.MeasureSet) {
	if ms == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.measures[id] = ms
}

// LookupMeasures returns the cached MeasureSet and true, or nil
// and false.
func (c *Cache) LookupMeasures(id v1.PortfolioID) (*domain.MeasureSet, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ms, ok := c.measures[id]
	return ms, ok
}

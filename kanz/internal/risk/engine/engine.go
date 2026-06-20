// Package engine holds the concrete risk Engine — the production
// implementation of v1.Engine that ORCH-01c/01d drive from the
// risk-engine service. It composes the four pieces the risk module
// shipped as isolated units (RISK-04..11):
//
//	state.Store  → the applied state-of-the-world (reader side: Lookup)
//	compute      → pure ExposureSet / MeasureSet derivation
//	risk.Cache   → last-known-good fallback per portfolio
//	risk.Detector → staleness → (Mode, QualityFlags)
//
// # Lifted from RISK-13
//
// The composition logic here is the production form of
// `computeExposureWithFlags` from kanz/internal/risk/integration_test.go:
// try live (Store.Lookup → compute → Cache.Store on success), fall back
// to the cache on a store miss, then tag the result via the Detector
// against its own AsOf. RISK-13 inlined this in test code as documentation
// of the pattern the orchestrator "should implement"; ORCH-01b moves it
// into production. The integration tests can now be rewritten against
// these methods directly (a separate cleanup — they still pass as-is).
//
// # Read concurrency
//
// The engine serves many concurrent queries while ORCH-01c's ingest
// goroutines apply state. Reads go through Store.Snapshot, which clones
// the portfolio under the per-aggregate lock (ORCH-01d) — a consistent,
// race-free point-in-time view to compute against while applies continue
// on the live copy. This is the per-aggregate read boundary the RISK-13
// composition note deferred to the recompute trigger.
package engine

import (
	"context"
	"time"

	risk "github.com/kanz-eng/kanz/internal/risk"
	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/scenario"
	"github.com/kanz-eng/kanz/internal/risk/state"
)

// EngineImpl is the production v1.Engine. Safe for concurrent use:
// Store, Cache, and Detector are each concurrency-safe, and the
// Registry is immutable after construction.
type EngineImpl struct {
	store    *state.Store
	registry *compute.Registry
	cache    *risk.Cache
	detector *risk.Detector
	volModel compute.VolModel
}

// EngineOption customizes an EngineImpl at construction.
type EngineOption func(*EngineImpl)

// WithVolModel wires the MODEL-01g volatility model so each snapshot's
// positions are enriched with a real MarketValueUncertainty band before
// compute, feeding the RISK-08 propagation. nil leaves every band nil (the
// pre-MODEL-01g behavior).
func WithVolModel(vm compute.VolModel) EngineOption {
	return func(e *EngineImpl) { e.volModel = vm }
}

// New constructs an EngineImpl over the engine's collaborators. A nil
// registry delegates to compute.DefaultRegistry (parity with
// compute.ComputeMeasures); store, cache, and detector are required.
func New(store *state.Store, registry *compute.Registry, cache *risk.Cache, detector *risk.Detector, opts ...EngineOption) *EngineImpl {
	e := &EngineImpl{store: store, registry: registry, cache: cache, detector: detector}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Exposure implements v1.Engine. Computes live from the latest applied
// state, caches the result, and falls back to the last-known-good cached
// value on a store miss. The response carries the staleness flags the
// Detector derives from the result's AsOf.
func (e *EngineImpl) Exposure(ctx context.Context, req v1.ExposureRequest) (v1.ExposureResponse, error) {
	if err := ctx.Err(); err != nil {
		return v1.ExposureResponse{}, err
	}
	if req.PortfolioID == "" {
		return v1.ExposureResponse{}, v1.ErrInvalidRequest
	}

	es, ok := e.exposureSet(ctx, req.PortfolioID)
	if !ok {
		return v1.ExposureResponse{}, v1.ErrPortfolioNotFound
	}
	_, flags := e.detector.Assess(es.AsOf())
	return v1.ExposureResponse{
		PortfolioID:  req.PortfolioID,
		AsOf:         es.AsOf(),
		Set:          es,
		QualityFlags: flags,
	}, nil
}

// Measures implements v1.Engine. Always computes (and caches) the full
// measure set so a later degraded fallback can satisfy any filter, then
// narrows the response to req.Measures (empty ⇒ all).
func (e *EngineImpl) Measures(ctx context.Context, req v1.MeasuresRequest) (v1.MeasuresResponse, error) {
	if err := ctx.Err(); err != nil {
		return v1.MeasuresResponse{}, err
	}
	if req.PortfolioID == "" {
		return v1.MeasuresResponse{}, v1.ErrInvalidRequest
	}

	full, ok := e.measureSet(ctx, req.PortfolioID)
	if !ok {
		return v1.MeasuresResponse{}, v1.ErrPortfolioNotFound
	}
	_, flags := e.detector.Assess(full.AsOf())
	return v1.MeasuresResponse{
		PortfolioID:  req.PortfolioID,
		AsOf:         full.AsOf(),
		Set:          filterMeasures(full, req.Measures),
		QualityFlags: flags,
	}, nil
}

// EvaluateScenario implements v1.Engine. Scenarios are pure functions of
// the *current live* state — there is no cache fallback because the
// cache holds derived ExposureSet/MeasureSet values, not a Portfolio to
// shock. A portfolio with no applied state is ErrPortfolioNotFound.
func (e *EngineImpl) EvaluateScenario(ctx context.Context, req v1.ScenarioRequest) (v1.ScenarioResponse, error) {
	if err := ctx.Err(); err != nil {
		return v1.ScenarioResponse{}, err
	}
	if req.PortfolioID == "" {
		return v1.ScenarioResponse{}, v1.ErrInvalidRequest
	}

	p, found := e.store.Snapshot(req.PortfolioID)
	if !found {
		return v1.ScenarioResponse{}, v1.ErrPortfolioNotFound
	}
	compute.PopulateUncertainty(ctx, p, e.volModel)
	projected := scenario.Evaluate(p, req.Shocks, e.registry)
	// Flag against the underlying state's freshness: a scenario on stale
	// state is itself stale, and the caller must see that signal.
	_, flags := e.detector.Assess(p.AsOf())
	return v1.ScenarioResponse{
		PortfolioID:  req.PortfolioID,
		Projected:    projected,
		QualityFlags: flags,
	}, nil
}

// Health implements v1.Engine. The engine's AsOf is the latest applied
// state-event time across all aggregates (per v1.Health) — a single live
// portfolio keeps the engine healthy even when others are quiet.
func (e *EngineImpl) Health(ctx context.Context) (v1.Health, error) {
	if err := ctx.Err(); err != nil {
		return v1.Health{}, err
	}
	return e.detector.Health(e.latestAsOf()), nil
}

// exposureSet returns the live-computed-and-cached exposure, or the
// last-known-good cached value on a store miss. The bool is false only
// when neither the store nor the cache knows the portfolio.
func (e *EngineImpl) exposureSet(ctx context.Context, id v1.PortfolioID) (*domain.ExposureSet, bool) {
	if p, found := e.store.Snapshot(id); found {
		compute.PopulateUncertainty(ctx, p, e.volModel)
		es := compute.ComputeExposure(p)
		e.cache.StoreExposure(id, es)
		return es, true
	}
	return e.cache.LookupExposure(id)
}

// measureSet mirrors exposureSet for the full measure set.
func (e *EngineImpl) measureSet(ctx context.Context, id v1.PortfolioID) (*domain.MeasureSet, bool) {
	if p, found := e.store.Snapshot(id); found {
		compute.PopulateUncertainty(ctx, p, e.volModel)
		ms := compute.ComputeMeasures(p, e.registry, nil)
		e.cache.StoreMeasures(id, ms)
		return ms, true
	}
	return e.cache.LookupMeasures(id)
}

// latestAsOf is the max AsOf across all known portfolios, or the zero
// time when no state has been applied (Detector reports ModeDegraded).
func (e *EngineImpl) latestAsOf() time.Time {
	var latest time.Time
	for _, id := range e.store.IDs() {
		if p, ok := e.store.Snapshot(id); ok && p.AsOf().After(latest) {
			latest = p.AsOf()
		}
	}
	return latest
}

// filterMeasures narrows a full measure set to the named subset. Empty
// names ⇒ the full set unchanged. Unknown names are dropped, matching
// compute.ComputeMeasures' filter semantics and v1.MeasureSet.Lookup's
// miss contract.
func filterMeasures(full *domain.MeasureSet, names []v1.MeasureName) *domain.MeasureSet {
	if len(names) == 0 {
		return full
	}
	subset := make(map[v1.MeasureName]v1.Measure, len(names))
	for _, n := range names {
		if m, ok := full.Lookup(n); ok {
			subset[n] = m
		}
	}
	return domain.NewMeasureSet(full.PortfolioID(), full.AsOf(), subset)
}

// Compile-time assertion that EngineImpl satisfies the api/v1 contract.
var _ v1.Engine = (*EngineImpl)(nil)

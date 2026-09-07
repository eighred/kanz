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
	"fmt"
	"sort"
	"strings"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/scenario"
	"github.com/eighred/kanz/internal/risk/state"
)

// EngineImpl is the production v1.Engine. Safe for concurrent use:
// Store, Cache, and Detector are each concurrency-safe, and the
// Registry is immutable after construction.
type EngineImpl struct {
	store      *state.Store
	registry   *compute.Registry
	cache      *risk.Cache
	detector   *risk.Detector
	volModel   compute.VolModel
	classifier factor.Classifier
	ownership  Ownership
}

// Ownership is the query side of the shard ring (#110): which portfolios may
// THIS replica answer for, and which replica holds the rest.
//
// It is an interface here rather than a *shard.Assignment because the ring is a
// risk-engine service concern and this package is shared; the service satisfies
// it (services/risk-engine/internal/app.Sharding). A nil Ownership is the
// unsharded posture — every portfolio is answerable, exactly as before.
type Ownership interface {
	// Owns reports whether this replica holds the portfolio's state.
	Owns(id v1.PortfolioID) bool
	// OwnerOf names the replica that does, for the refusal message.
	OwnerOf(id v1.PortfolioID) string
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

// WithClassifier wires the MODEL-01f factor model so MODEL-01h SectorShocks in
// EvaluateScenario resolve each position's sector, and so exposureSet serves the
// SECTOR dimension.
//
// EVERY NAMED SCENARIO DEPENDS ON IT, and it had no caller at all until #640.
// The scenario library builds GFC_2008, COVID_2020 and the curve, factor,
// liquidity and climate stresses out of SectorCurve, which emits SectorShocks
// exclusively — so with this unset the whole catalog first returned the
// unshocked book, and then (once ErrScenarioUnresolvable landed) refused.
// services/risk-engine now passes refdata.Cache.Factor when a security master
// is configured.
//
// NIL REMAINS A LEGAL POSTURE and still means "this deployment has no
// reference-data source": scenarios carrying sector shocks refuse, and
// exposureSet serves the two dimensions compute.ComputeExposure produces rather
// than inventing a third. test/arch/no_nil_classifier_seam_test.go keeps every
// remaining nil tracked.
func WithClassifier(c factor.Classifier) EngineOption {
	return func(e *EngineImpl) { e.classifier = c }
}

// WithOwnership makes the query surface shard-aware: a request for a portfolio
// this replica does not own is REFUSED, naming the replica that does, rather
// than answered.
//
// # Why a refusal and not a miss
//
// Without this the read path does not consult the ring at all. It asks the
// store, and on a miss falls back to risk.Cache — the last-known-good value.
// Both halves of that are wrong on a sharded replica:
//
//   - The cache is populated by this replica's own recomputes. A portfolio it
//     used to own, or one it briefly held before a ring change, leaves a
//     plausible entry behind. Serving it is a real number computed from a book
//     this replica no longer has, with no staleness signal that says so — the
//     degraded fallback is designed for "the store lost it", not "somebody else
//     owns it".
//   - And when the cache is empty the answer is ErrPortfolioNotFound, which
//     says the portfolio does not exist. It does; it is on another pod. The
//     query Service round-robins across replicas, so the caller gets that answer
//     for two thirds of identical requests.
//
// nil ownership leaves both paths exactly as they were (the unsharded default).
func WithOwnership(o Ownership) EngineOption {
	return func(e *EngineImpl) { e.ownership = o }
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
	// A PINNED QUERY IS REFUSED, NOT QUIETLY ANSWERED FROM LIVE STATE (#859).
	//
	// exposureSet below reads store.Snapshot(id) — the latest applied state, and
	// the only state this engine holds. Answering a historical as_of from it
	// returns today's book stamped with today's timestamp, which is not a
	// slightly-wrong answer but a confidently wrong one: internally consistent,
	// unflagged, and indistinguishable from the real thing to a reconciliation
	// or a regulatory as-of report.
	//
	// REFUSED HERE RATHER THAN AT THE GATEWAY, because the gateway is not the
	// only caller. services/mcp and services/copilot build these requests
	// directly against the gRPC surface, and a boundary check would leave them
	// with the silent answer. EngineImpl is the single v1.Engine implementation,
	// so this is the one place every path converges.
	if !req.AsOf.IsZero() {
		return v1.ExposureResponse{}, v1.ErrAsOfNotSupported
	}
	if err := e.refuseIfNotOwned(req.PortfolioID); err != nil {
		return v1.ExposureResponse{}, err
	}

	es, pos, ok := e.exposureSet(ctx, req.PortfolioID)
	if !ok {
		return v1.ExposureResponse{}, v1.ErrPortfolioNotFound
	}
	_, flags := e.detector.Assess(es.AsOf())
	return v1.ExposureResponse{
		PortfolioID:    req.PortfolioID,
		AsOf:           es.AsOf(),
		Set:            es,
		QualityFlags:   flags,
		SourcePosition: pos,
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
	// Refused for the reason Exposure gives above (#859), and it matters more
	// here: a MeasureSet carries VaR and the sensitivities a desk hedges on, so
	// a pinned query answered from live state hands back today's risk numbers
	// under a historical label.
	if !req.AsOf.IsZero() {
		return v1.MeasuresResponse{}, v1.ErrAsOfNotSupported
	}
	if err := e.refuseIfNotOwned(req.PortfolioID); err != nil {
		return v1.MeasuresResponse{}, err
	}

	full, pos, ok := e.measureSet(ctx, req.PortfolioID)
	if !ok {
		return v1.MeasuresResponse{}, v1.ErrPortfolioNotFound
	}
	_, flags := e.detector.Assess(full.AsOf())
	// FLAGGED OFF THE SERVED SUBSET, NOT THE FULL SET. The two coverage records
	// behave differently under a filter and filterMeasures already encodes that:
	// the currency exclusions are copied onto the subset because they are a
	// property of the portfolio, while the per-measure input coverage travels
	// inside the measures and so narrows with them. Flagging off `full` would
	// therefore attach INPUTS_UNRESOLVED to a response containing only
	// GrossExposure because some FI measure the caller did not ask for could not
	// price — and a flag that describes numbers that are not on the response is
	// one every reader learns to ignore.
	served := filterMeasures(full, req.Measures)
	return v1.MeasuresResponse{
		PortfolioID:    req.PortfolioID,
		AsOf:           full.AsOf(),
		Set:            served,
		QualityFlags:   withCoverageFlags(flags, served),
		SourcePosition: pos,
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
	if err := e.refuseIfNotOwned(req.PortfolioID); err != nil {
		return v1.ScenarioResponse{}, err
	}

	p, found := e.store.Snapshot(req.PortfolioID)
	if !found {
		return v1.ScenarioResponse{}, v1.ErrPortfolioNotFound
	}
	compute.PopulateUncertainty(ctx, p, e.volModel)
	var opts []scenario.Option
	if e.classifier != nil {
		opts = append(opts, scenario.WithClassifier(e.classifier))
	}
	projected, cov := scenario.Evaluate(p, req.Shocks, e.registry, opts...)
	// A SHOCK THAT DID NOT LAND MAKES THE WHOLE PROJECTION A LIE, so it is
	// refused rather than annotated. See v1.ErrScenarioUnresolvable for why this
	// is the opposite call from QualityFlagInputsUnresolved, which the measures
	// path uses for a partial number: here there is no partial number, only the
	// current book wearing a scenario's name.
	if cov.ExcludedCount > 0 {
		return v1.ScenarioResponse{}, unresolvableScenario(cov)
	}
	// Flag against the underlying state's freshness: a scenario on stale
	// state is itself stale, and the caller must see that signal.
	_, flags := e.detector.Assess(p.AsOf())
	return v1.ScenarioResponse{
		PortfolioID:  req.PortfolioID,
		Projected:    projected,
		QualityFlags: withCoverageFlags(flags, projected),
	}, nil
}

// unresolvableScenario turns a shock-application coverage record into the
// refusal a caller acts on: the sentinel to branch on, plus the reason, the
// magnitude and a bounded sample of holdings in the message.
//
// THE REASON IS IN THE TEXT BECAUSE THE OPERATOR'S NEXT MOVE DEPENDS ON IT.
// SkipNoClassifier is a composition-root gap — nobody wired an instrument
// reference source and no per-instrument action will help. SkipUnclassified
// names holdings a wired classifier does not know, which is a reference-data
// load. SkipShockNamesNoSector is the caller's own malformed shock.
// SkipNoRevaluer is a composition-root gap too, but a DEEPER one: a vol stress
// needs an option pricer, which needs a calibrated vol surface, which needs
// observed option premiums nothing on this estate carries — so the answer to
// "when can I run this" is an issue (#509/#203), not a config change.
// SkipUnknownShockType means the caller and engine disagree on the v1 shock
// vocabulary, so neither data loading nor configuration can repair the request.
// One unhelpful message would send these cases to the same wrong place.
func unresolvableScenario(cov v1.InputCoverage) error {
	reasons := make([]string, 0, len(cov.Exclusions))
	seen := map[string]bool{}
	var sample []string
	for _, ex := range cov.Exclusions {
		if !seen[ex.Reason] {
			seen[ex.Reason] = true
			reasons = append(reasons, ex.Reason)
		}
		if ex.InstrumentID != "" && len(sample) < 8 {
			sample = append(sample, string(ex.InstrumentID))
		}
	}
	sort.Strings(reasons)
	msg := fmt.Sprintf("%d of %d resolved; reason(s): %s",
		cov.Contributed, cov.Contributed+cov.ExcludedCount, strings.Join(reasons, ","))
	if len(sample) > 0 {
		msg += "; e.g. " + strings.Join(sample, ",")
	}
	return fmt.Errorf("%w (%s)", v1.ErrScenarioUnresolvable, msg)
}

// withCoverageFlags appends the coverage signals the Detector cannot
// produce. Detector.Assess sees only a time.Time, so it can report that
// a number is OLD but never that it is PARTIAL — the gap that let
// currency-excluded positions vanish silently (#257). The set itself
// carries both records, so this works identically on the live path and
// on a degraded cache read where the portfolio is no longer in hand.
//
// TWO FLAGS, NOT ONE, and they are not interchangeable. CURRENCY_EXCLUDED
// says the engine deliberately declined to include positions it could
// see, because it has no FX layer. INPUTS_UNRESOLVED says data the engine
// expected was not there — bond terms nobody writes, a curve nobody
// calibrated, a return series the price store does not hold (#527). Only
// the second is somebody's bug, and merging them would hide the one
// that has a fix.
func withCoverageFlags(flags []v1.QualityFlag, ms *domain.MeasureSet) []v1.QualityFlag {
	if ms == nil {
		return flags
	}
	if len(ms.CurrencyExclusions()) > 0 {
		flags = append(flags, v1.QualityFlagCurrencyExcluded)
	}
	if len(ms.UnresolvedMeasures()) > 0 {
		flags = append(flags, v1.QualityFlagInputsUnresolved)
	}
	return flags
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

// ListPortfolios names the portfolios the store currently holds.
//
// IT READS THE STORE, NEVER THE CACHE. The exposure path deliberately falls
// back to the last-known-good cache on a store miss, so a query about a
// portfolio the store has dropped still answers. A LIST must not do that: it
// would offer a caller a portfolio the engine cannot then describe, and the
// difference between "here is a stale figure for something you asked about" and
// "here is a thing that no longer exists" is the difference between degraded
// and wrong.
//
// SORTED BY ID, because Store.IDs walks a map and Go randomises that. An
// unsorted list re-orders itself on every poll, which reads as the estate
// changing when nothing has.
//
// A portfolio dropped between IDs() and Snapshot() is skipped rather than
// returned empty: the two calls are not one atomic read, and a row with an id
// and nothing else is worse than a row that is absent.
func (e *EngineImpl) ListPortfolios(ctx context.Context) ([]v1.PortfolioSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := e.store.IDs()
	out := make([]v1.PortfolioSummary, 0, len(ids))
	for _, id := range ids {
		p, found := e.store.Snapshot(id)
		if !found {
			continue
		}
		out = append(out, v1.PortfolioSummary{
			ID:            id,
			DisplayName:   p.DisplayName(),
			BaseCurrency:  string(p.BaseCurrency()),
			AsOf:          p.AsOf(),
			PositionCount: p.PositionCount(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// refuseIfNotOwned is the query-side shard gate. It sits at the top of every
// per-portfolio read, BEFORE the store and before the cache fallback, so there
// is no path on which a non-owner produces a number. Unsharded engines (nil
// ownership) always return nil.
func (e *EngineImpl) refuseIfNotOwned(id v1.PortfolioID) error {
	if e.ownership == nil || e.ownership.Owns(id) {
		return nil
	}
	return fmt.Errorf("%w: %q is held by replica %q", v1.ErrPortfolioNotOwned, id, e.ownership.OwnerOf(id))
}

// exposureSet returns the live-computed-and-cached exposure plus the
// durable-log coordinate the live state was folded to (the portfolio's
// applied snapshot LogPosition, WIRE-03), or the last-known-good cached
// value on a store miss. The position is nil on the cache-fallback path: a
// cached value has no live portfolio in hand, so no log anchor. The bool is
// false only when neither the store nor the cache knows the portfolio.
func (e *EngineImpl) exposureSet(ctx context.Context, id v1.PortfolioID) (*domain.ExposureSet, *commonpb.LogPosition, bool) {
	if p, found := e.store.Snapshot(id); found {
		compute.PopulateUncertainty(ctx, p, e.volModel)
		// THE SECTOR DIMENSION IS SERVED ONLY WHEN IT CAN BE RESOLVED (#640).
		// factor.ComputeExposure layers ExposureBySector onto the instrument and
		// currency dimensions compute.ComputeExposure produces; this branch used
		// to be unreachable, because WithClassifier had no caller anywhere in the
		// module and the RISK-06 doc's "the engine uses this when a Classifier is
		// wired" described a condition that was never met.
		//
		// Positions the classifier cannot resolve land in factor.UnclassifiedSector
		// rather than being dropped, so the sector totals still reconcile with
		// gross — an operator reading a large UNCLASSIFIED bucket is being told
		// about a reference-data gap, which is the honest answer and the one a
		// silently absent dimension could not give.
		es := compute.ComputeExposure(p)
		if e.classifier != nil {
			es = factor.ComputeExposure(ctx, p, e.classifier)
		}
		e.cache.StoreExposure(id, es)
		return es, p.LogPosition(), true
	}
	es, ok := e.cache.LookupExposure(id)
	return es, nil, ok
}

// measureSet mirrors exposureSet for the full measure set, returning the
// same live snapshot LogPosition (nil on the cache-fallback path).
func (e *EngineImpl) measureSet(ctx context.Context, id v1.PortfolioID) (*domain.MeasureSet, *commonpb.LogPosition, bool) {
	if p, found := e.store.Snapshot(id); found {
		compute.PopulateUncertainty(ctx, p, e.volModel)
		ms := compute.ComputeMeasures(p, e.registry, nil)
		e.cache.StoreMeasures(id, ms)
		return ms, p.LogPosition(), true
	}
	ms, ok := e.cache.LookupMeasures(id)
	return ms, nil, ok
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
//
// The currency exclusions carry over to the subset: narrowing WHICH
// measures are returned does not change which positions went into them,
// and a subset that lost the coverage record would be a partial number
// that no longer says so (#257).
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
	return domain.NewMeasureSet(full.PortfolioID(), full.AsOf(), subset,
		domain.WithCurrencyExclusions(full.CurrencyExclusions()))
}

// Compile-time assertion that EngineImpl satisfies the api/v1 contract.
var _ v1.Engine = (*EngineImpl)(nil)

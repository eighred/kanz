// Package scenario implements the risk engine's what-if evaluator
// (RISK-09). Scenarios are PURE FUNCTIONS of (current state, shocks):
// the input Portfolio is never mutated, the engine deep-clones it
// into a transient working copy, applies the shocks, and recomputes
// measures against the shocked state.
//
// # Why the shock concrete types live in api/v1
//
// External callers (the orchestrator's HTTP API, scenario tooling)
// need to CONSTRUCT shocks to drive Engine.EvaluateScenario, but
// RISK-02's arch test forbids them from importing this package. So
// the value types (PriceShock, ParallelShift) live in api/v1 where
// outsiders can reach them; this package owns the DISPATCH + APPLY
// logic, type-asserting on the api/v1 types it knows. Unknown shock
// types (a future v1 addition the engine hasn't been rebuilt for,
// or a third-party shock from a downstream wrapper) are silent
// no-ops — same pattern as ComputeMeasures filter dropping unknown
// names.
//
// # Why a deep clone, not a shallow one
//
// Portfolio holds a positions map; SetPosition would mutate it in
// place. We need the original portfolio untouched (the engine is
// serving many concurrent queries plus its ingest goroutine —
// RISK-05's per-portfolio lock is held by the orchestrator for the
// duration of Evaluate, but mutation of the live portfolio under
// that lock would still be wrong because subsequent queries inside
// the same lock-window would see the shocked state). Cheap to
// clone in absolute terms — typical portfolios hold tens to
// hundreds of positions, value-copied.
package scenario

import (
	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// Evaluate applies shocks to a deep clone of p and returns the
// MeasureSet computed against the shocked state. p is not mutated.
// The registry parameter selects which measures to compute; nil
// falls back to compute.DefaultRegistry() for parity with
// compute.ComputeMeasures.
func Evaluate(p *domain.Portfolio, shocks []v1.ScenarioShock, registry *compute.Registry) *domain.MeasureSet {
	shocked := cloneWithShocks(p, shocks)
	return compute.ComputeMeasures(shocked, registry, nil)
}

// cloneWithShocks deep-copies p, applies every known shock, returns
// the working copy. Position is a plain value struct so the map
// copy is a true deep copy at the field level.
func cloneWithShocks(p *domain.Portfolio, shocks []v1.ScenarioShock) *domain.Portfolio {
	cp := domain.NewPortfolio(p.ID(), p.BaseCurrency())
	cp.SetAggregate(domain.AggregateUpdate{
		AsOf:             p.AsOf(),
		DisplayName:      p.DisplayName(),
		BaseCurrency:     p.BaseCurrency(),
		CashBalance:      p.CashBalance(),
		TotalMarketValue: p.TotalMarketValue(),
		PositionCount:    p.PositionCount(),
	})
	for _, pos := range p.Positions() {
		cp.SetPosition(pos)
	}
	for _, shock := range shocks {
		applyShock(cp, shock)
	}
	return cp
}

// applyShock dispatches one shock to its handler. Unknown shock
// types silently skip — the engine processes large scenario
// batches and a single unrecognised shock should not abort the run.
func applyShock(p *domain.Portfolio, shock v1.ScenarioShock) {
	switch s := shock.(type) {
	case v1.PriceShock:
		applyPriceShock(p, s)
	case v1.ParallelShift:
		applyParallelShift(p, s)
	}
}

// applyPriceShock changes one instrument's MarketValue. A shock
// targeting an instrument the portfolio does not hold is a no-op —
// scenario callers commonly fire watchlist-wide batches against
// multiple portfolios.
func applyPriceShock(p *domain.Portfolio, s v1.PriceShock) {
	pos, ok := p.Position(s.InstrumentID)
	if !ok {
		return
	}
	pos.MarketValue = compute.ShockMoney(pos.MarketValue, s.Pct)
	p.SetPosition(pos)
}

// applyParallelShift shocks every position's MarketValue by the
// same percentage. The textbook stress test.
func applyParallelShift(p *domain.Portfolio, s v1.ParallelShift) {
	for _, pos := range p.Positions() {
		pos.MarketValue = compute.ShockMoney(pos.MarketValue, s.Pct)
		p.SetPosition(pos)
	}
}

package scenario

import (
	"context"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/pricing/curve"
)

// Yield-curve-shift scenarios (FI-01e). EvaluateCurveShift is the fixed-income
// sibling of EvaluateReval: it reprices BOND positions off a shifted discount
// curve (parallel / steepener / butterfly, from the scenario library) and
// recomputes measures against the revalued state. Non-bond positions are
// untouched — a rate move does not reprice a cash equity. A portfolio with no
// bonds yields exactly what an unshocked ComputeMeasures would.
//
// It is a separate entry point, not a change to Evaluate, so the existing linear
// scenario path is unaffected — the same blast-radius discipline as DERIV-01e.

// BondRevaluer reprices a bond position under a curve shift. compute.BondRevaluer
// satisfies it; the interface lives here so scenario does not force a particular
// implementation. ok=false ⇒ not a bond / inputs unavailable (left unshocked).
type BondRevaluer interface {
	RevalueBond(ctx context.Context, instrumentID string, asOf time.Time, baseMV *commonpb.Money, shift curve.Shift) (*commonpb.Money, bool)
}

// EvaluateCurveShift applies a curve shift to a deep clone of p — repricing bond
// positions off the shifted curve — then computes measures against the revalued
// state. p is not mutated. A nil reval or nil shift falls back to an unshocked
// ComputeMeasures (the curve move has no other expression).
func EvaluateCurveShift(p *domain.Portfolio, shift curve.Shift, registry *compute.Registry, reval BondRevaluer) *domain.MeasureSet {
	if reval == nil || shift == nil {
		return compute.ComputeMeasures(cloneCarry(p), registry, nil)
	}
	cp := cloneCarry(p)
	ctx := context.Background()
	asOf := p.AsOf()
	for _, pos := range cp.Positions() {
		if mv, ok := reval.RevalueBond(ctx, string(pos.InstrumentID), asOf, pos.MarketValue, shift); ok {
			pos.MarketValue = mv
			cp.SetPosition(pos)
		}
	}
	return compute.ComputeMeasures(cp, registry, nil)
}

// cloneCarry deep-copies p (aggregate + positions) without applying any shocks —
// the shared clone step EvaluateCurveShift reuses from the linear path.
func cloneCarry(p *domain.Portfolio) *domain.Portfolio {
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
	return cp
}

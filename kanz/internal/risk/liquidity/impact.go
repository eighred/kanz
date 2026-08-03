package liquidity

import (
	"context"
	"math"

	"github.com/eighred/kanz/internal/risk/domain"
)

// Market impact / liquidation cost (LIQ-01c). The cost of unwinding a position is
// the half-spread paid to cross the book once, widened by the square root of the
// liquidation horizon — an Almgren-Chriss-style temporary-impact term: working a
// larger order over more days accrues more spread and impact cost. The horizon
// (DaysToLiquidate) already encodes size relative to ADV, so a bigger or less
// liquid position costs more through a longer horizon.
//
//	costFraction = ½·spread · (1 + impactCoeff·(√days − 1))
//
// At a one-day horizon this is exactly the half-spread (a single crossing); with
// the default impactCoeff=1 it is ½·spread·√days. The fraction is capped at 1 —
// you cannot lose more than the full notional to liquidation, which also bounds
// the cost of an illiquid (ADV-zero, +Inf-day) position at its notional rather
// than blowing up to infinity (LIQ-01b ADV-zero handling).

// CostFraction is the fraction of a position's notional lost to liquidation at a
// given horizon and spread (a fraction of price). Non-negative, capped at 1,
// monotonic increasing in both days and spread (for impactCoeff ≥ 0).
func (m Model) CostFraction(days, spread float64) float64 {
	if spread <= 0 {
		return 0
	}
	half := 0.5 * spread
	frac := half
	if days > 1 {
		coeff := m.ImpactCoeff
		if coeff < 0 {
			coeff = 0
		}
		frac = half * (1 + coeff*(math.Sqrt(days)-1))
	}
	if frac > 1 || math.IsInf(frac, 1) {
		return 1
	}
	return frac
}

// LiquidationCost is the total money cost of unwinding the whole book under the
// model — Σ |MarketValue_i| × CostFraction_i — in the portfolio base currency.
// Only base-currency positions with liquidity data contribute; an illiquid
// (ADV≤0) position contributes its full notional (cost fraction caps at 1).
// Always ≥ 0, so a VaR widened by it is never below the unadjusted VaR.
func (m Model) LiquidationCost(ctx context.Context, p *domain.Portfolio, provider Provider) float64 {
	base := p.BaseCurrency()
	asOf := p.AsOf()
	var cost float64
	for _, pos := range p.Positions() {
		if !pos.InBaseCurrency(base) {
			continue
		}
		spec, ok := provider.Liquidity(ctx, string(pos.InstrumentID), asOf)
		if !ok {
			continue
		}
		notional := math.Abs(decimalToFloat(pos.MarketValue.GetAmount()))
		days, _ := m.DaysToLiquidate(decimalToFloat(pos.Quantity), spec)
		cost += notional * m.CostFraction(days, spec.Spread)
	}
	return cost
}

package compute

import (
	"context"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/liquidity"
)

// LIQ-01d wires the liquidity-risk models into the RISK-07 measure registry —
// the liquidity sibling of the FI-01d rate-risk and DERIV-01d Greek measures. It
// registers the portfolio liquidation horizon and liquidity-adjusted VaR, each
// resolving a position's ADV/spread through the injected liquidity.Provider at
// p.AsOf(), the same point-in-time enrichment seam.
//
//	LiquidationHorizon = notional-weighted average days-to-liquidate (liquid names)
//	LVaR99             = VaR99 + Σ |MV_i| × liquidation-cost-fraction_i
//
// LVaR closes over the already-registered VaR measure (the RISK-07 placeholder or
// the MODEL-01d historical model), so it tracks whichever VaR the engine serves
// and is ≥ it by construction — the liquidation cost is never negative.

// Liquidity measure names (RISK-07 naming: short, CamelCase).
const (
	MeasureLiquidationHorizon v1.MeasureName = "LiquidationHorizon"
	MeasureLVaR99             v1.MeasureName = "LVaR99"
)

// liqHorizonExp is the precision the liquidation-horizon (a number of days) is
// emitted at, matching the dimensionless-ratio band (1e-4). liqVaRExp is the
// money scale LVaR is emitted at (cents), matching the VaR models.
const (
	liqHorizonExp int32 = -4
	liqVaRExp     int32 = -2
)

// RegisterLiquidityRisk registers LiquidationHorizon and LVaR99 on r, each
// closing over ctx + the provider + model. baseVaR is the VaR measure LVaR adds
// the liquidation cost to (nil ⇒ the registry's current VaR99, defaulting to the
// RISK-07 placeholder). Call at engine startup after DefaultRegistry / the VaR
// override; tests register against deterministic providers.
func RegisterLiquidityRisk(ctx context.Context, r *Registry, provider liquidity.Provider, model liquidity.Model, baseVaR MeasureFunc) {
	if baseVaR == nil {
		baseVaR = VaR99
	}
	r.Register(MeasureLiquidationHorizon, liquidationHorizonMeasure(ctx, provider, model))
	r.Register(MeasureLVaR99, lvarMeasure(ctx, provider, model, baseVaR))
}

// liquidationHorizonMeasure emits the notional-weighted average days-to-liquidate
// over the liquid, base-currency book. Illiquid (ADV-zero) names are flagged in
// the richer liquidity.Profile, not folded into this scalar (a +Inf horizon has
// no Decimal representation) — they surface as a quality concern a layer up.
func liquidationHorizonMeasure(ctx context.Context, provider liquidity.Provider, model liquidity.Model) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		prof := model.LiquidationProfile(ctx, p, provider)
		return v1.Measure{Name: MeasureLiquidationHorizon, Value: floatToDecimal(prof.WeightedDays, liqHorizonExp)}
	}
}

// lvarMeasure emits VaR99 widened by the book's liquidation cost. The value
// inherits VaR's money scale (cents); the cost is added in the same units.
func lvarMeasure(ctx context.Context, provider liquidity.Provider, model liquidity.Model, baseVaR MeasureFunc) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		base := baseVaR(p)
		varFloat := decimalToFloat(base.Value)
		lvar := model.LiquidityAdjustedVaR(ctx, varFloat, p, provider)
		return v1.Measure{Name: MeasureLVaR99, Value: floatToDecimal(lvar, liqVaRExp)}
	}
}

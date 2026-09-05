package varmodel

import (
	"context"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
)

// drawdownExponent is the Decimal scale of the FRACTION drawdown: a ratio in
// [0,1], so a fractional scale. The AMOUNT drawdown uses varExponent (cents).
const drawdownExponent int32 = -4

// maxDrawdown walks the cumulative value path V_t = v0 + Σ_{s≤t} pnl_s in TIME
// order, tracking the running peak, and returns the maximum peak-to-trough
// decline as a FRACTION of peak and as an absolute AMOUNT. The two are maximized
// INDEPENDENTLY and may fall at different peaks when the path has several peaks
// of different heights — the worst percentage decline (from a lower peak) and
// the worst dollar decline (from a higher peak) are distinct questions with
// distinct answers, so there is no fixed amount = fraction × peak relationship.
// The fraction is only updated where peak > 0 (a non-positive peak — a net-flat
// or net-short book — has no meaningful percentage drawdown); the amount is
// always well-defined.
func maxDrawdown(pnl []float64, v0 float64) (fraction, amount float64) {
	peak := v0
	v := v0
	for _, x := range pnl {
		v += x
		if v > peak {
			peak = v
		}
		dd := peak - v
		if dd > amount {
			amount = dd
		}
		if peak > 0 {
			if f := dd / peak; f > fraction {
				fraction = f
			}
		}
	}
	return fraction, amount
}

// MaxDrawdownFraction builds the MaxDrawdown measure — worst peak-to-trough
// decline as a fraction of peak (0–1), the relative-severity / benchmark layer.
// Reuses portfolioPnL, so it is consistent with VaR/ES over the same window.
func MaxDrawdownFraction(cfg Config) compute.ReturnsMeasure {
	window := cfg.Window
	prov := historicalProvenance(cfg.confidence(), window)
	return func(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider) v1.Measure {
		var cov compute.Coverage
		pnl, v0, ok := portfolioPnL(ctx, p, rp, window, &cov)
		if !ok {
			return zeroNamed(compute.MeasureMaxDrawdown, prov, cov)
		}
		frac, _ := maxDrawdown(pnl, v0)
		return v1.Measure{
			Name:       compute.MeasureMaxDrawdown,
			Value:      floatToDecimal(frac, drawdownExponent),
			Coverage:   cov.Result(),
			Provenance: prov,
		}
	}
}

// MaxDrawdownAmount builds the MaxDrawdownAmount measure — worst peak-to-trough
// decline as an absolute base-currency loss (cents), the capital / margin layer.
func MaxDrawdownAmount(cfg Config) compute.ReturnsMeasure {
	window := cfg.Window
	prov := historicalProvenance(cfg.confidence(), window)
	return func(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider) v1.Measure {
		var cov compute.Coverage
		pnl, v0, ok := portfolioPnL(ctx, p, rp, window, &cov)
		if !ok {
			return zeroNamed(compute.MeasureMaxDrawdownAmount, prov, cov)
		}
		_, amount := maxDrawdown(pnl, v0)
		return v1.Measure{
			Name:       compute.MeasureMaxDrawdownAmount,
			Value:      floatToDecimal(amount, varExponent),
			Coverage:   cov.Result(),
			Provenance: prov,
		}
	}
}

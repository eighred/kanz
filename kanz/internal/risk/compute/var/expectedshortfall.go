package varmodel

import (
	"context"
	"math"
	"sort"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// ExpectedShortfall builds the ES99 (CVaR) measure: the mean loss in the tail
// AT OR BEYOND the 99% VaR quantile, on the same empirical P&L distribution as
// Historical VaR (via portfolioPnL) — so ES99 ≥ VaR99 by construction and the
// two cannot drift. Estimator: ES = −mean(worst k P&Ls), k = ⌈n·(1−α)⌉ (the
// empirical ES estimator — the mean of the ⌈n·(1−α)⌉ worst scenarios; e.g.
// n=250, α=0.99 ⇒ k=3), k floored at 1 so a small window still yields the single
// worst loss. Emitted as a money loss in base-currency cents, like VaR99.
// Insufficient data ⇒ zero measure; a non-loss tail floors at zero.
func ExpectedShortfall(cfg Config) compute.ReturnsMeasure {
	conf := cfg.confidence()
	window := cfg.Window
	return func(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider) v1.Measure {
		pnl, _, ok := portfolioPnL(ctx, p, rp, window)
		if !ok {
			return zeroNamed(compute.MeasureES99)
		}
		sorted := append([]float64(nil), pnl...)
		sort.Float64s(sorted) // ascending: worst (most negative) first
		n := len(sorted)
		k := int(math.Ceil(float64(n) * (1 - conf)))
		if k < 1 {
			k = 1
		}
		if k > n {
			k = n
		}
		var sum float64
		for i := 0; i < k; i++ {
			sum += sorted[i]
		}
		es := -(sum / float64(k))
		if es < 0 {
			es = 0
		}
		return v1.Measure{
			Name:  compute.MeasureES99,
			Value: floatToDecimal(es, varExponent),
		}
	}
}

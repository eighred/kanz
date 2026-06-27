package liquidity

import (
	"context"
	"time"
)

// Liquidity-stress scenario (LIQ-01d). A liquidity crisis does not move prices
// the way the GICS/curve scenarios do — it widens spreads and dries up volume.
// A Stress is therefore a transform on the liquidity Provider, not a price shock:
// it multiplies spreads up and ADV down, so days-to-liquidate lengthen and the
// liquidation cost rises (and with it LVaR). Wrapping a provider lets every
// liquidity measure recompute under stress with no other code change — the
// EvaluateReval blast-radius discipline, one axis over.
type Stress struct {
	// SpreadMult scales spreads (≥1 widens). ≤0 ⇒ 1 (no change).
	SpreadMult float64
	// ADVMult scales average daily volume (≤1 dries up liquidity, lengthening
	// the horizon). ≤0 ⇒ 1 (no change).
	ADVMult float64
}

// Wrap returns a Provider that applies the stress to every spec p resolves. A
// position with no base-line liquidity stays unavailable (the stress cannot
// invent data it does not have).
func (s Stress) Wrap(p Provider) Provider {
	return stressedProvider{inner: p, s: s.withDefaults()}
}

func (s Stress) withDefaults() Stress {
	if s.SpreadMult <= 0 {
		s.SpreadMult = 1
	}
	if s.ADVMult <= 0 {
		s.ADVMult = 1
	}
	return s
}

type stressedProvider struct {
	inner Provider
	s     Stress
}

func (sp stressedProvider) Liquidity(ctx context.Context, instrumentID string, asOf time.Time) (LiquiditySpec, bool) {
	spec, ok := sp.inner.Liquidity(ctx, instrumentID, asOf)
	if !ok {
		return spec, false
	}
	spec.Spread *= sp.s.SpreadMult
	spec.ADV *= sp.s.ADVMult
	return spec, true
}

// Compile-time assertion that stressedProvider satisfies Provider.
var _ Provider = stressedProvider{}

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

// ServesSpread FORWARDS THE INNER PROVIDER'S CLAIM, and it exists because
// wrapping silently dropped it.
//
// compute decides whether to register LVaR99 by asking the provider whether it
// can ever serve a non-zero spread, and it treats "does not implement" as "no
// claim, assume yes" — the compatible default for providers written before that
// question existed. A stressed provider is a different type, so it answered "no
// claim" however emphatically the provider underneath it had said no, and
// LVaR99 came back onto the wire on the stress path.
//
// Stressing a zero spread does not help: the multiplier is applied to whatever
// the inner provider returned, and any multiple of zero is zero. So the measure
// that returns is the degenerate one — LVaR99 identically equal to VaR99 — under
// exactly the configuration that was chosen to keep it off.
//
// Declared structurally rather than against compute's interface, because compute
// imports this package and the dependency cannot run both ways.
func (p stressedProvider) ServesSpread() bool {
	type spreadServing interface{ ServesSpread() bool }
	if s, ok := p.inner.(spreadServing); ok {
		return s.ServesSpread()
	}
	// The inner provider makes no claim, so neither does this one — wrapping must
	// not manufacture an answer the thing it wraps declined to give.
	return true
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

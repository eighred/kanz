package compute

import (
	"context"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/pricing"
)

// Full-revaluation of option positions for scenario analysis (DERIV-01e). Where
// a linear scenario scales a position's MarketValue by the price shock, full
// reval reprices the option through Black-Scholes/binomial under the shocked
// spot AND shocked vol — capturing the gamma/vega effects a linear shock cannot.
// The Revaluer is wired with the same GreeksProviders the Greek measures use, so
// scenario repricing and risk reporting price off one consistent source.

// revalMoneyExp is the precision a revalued MarketValue is emitted at (cents).
const revalMoneyExp int32 = -2

// RevalShocks are the shocks applied to one option position under full reval.
// PriceFrac is the fractional underlying move (−0.10 = −10%); GlobalVolBump an
// absolute vol change applied to every underlying; VolByUnderlying an additional
// per-underlying absolute bump.
type RevalShocks struct {
	PriceFrac       float64
	GlobalVolBump   float64
	VolByUnderlying map[string]float64
}

// Revaluer reprices option positions under shocks. It is wired with the option
// terms / spot / vol / curve providers; a non-option position (or one missing
// pricing inputs) returns ok=false so the scenario keeps the linear shock.
type Revaluer struct{ providers GreeksProviders }

// NewRevaluer wraps the providers (curve defaults to flat zero).
func NewRevaluer(p GreeksProviders) *Revaluer {
	if p.Curve == nil {
		p.Curve = pricing.FlatCurve(0)
	}
	return &Revaluer{providers: p}
}

// RevalueOption returns baseMV repriced under shocks. baseMV is the position's
// current MarketValue (price × quantity × multiplier), so the revalued value is
// baseMV × (shockedPrice / basePrice) — scaling preserves the position size and
// sign (a short option keeps its negative value). ok=false ⇒ leave to the linear
// path.
func (rv *Revaluer) RevalueOption(ctx context.Context, instrumentID string, asOf time.Time, baseMV *commonpb.Money, shocks RevalShocks) (*commonpb.Money, bool) {
	if rv.providers.Terms == nil || baseMV == nil {
		return nil, false
	}
	spec, ok := rv.providers.Terms.OptionTerms(ctx, instrumentID, asOf)
	if !ok {
		return nil, false
	}
	spot, ok := rv.providers.Spot.Spot(ctx, spec.UnderlyingID, asOf)
	if !ok || spot <= 0 {
		return nil, false
	}
	ttm := spec.Expiry.Sub(asOf).Hours() / 24 / 365
	if ttm <= 0 {
		return nil, false
	}
	vol, ok := rv.providers.Vol.Vol(ctx, spec.UnderlyingID, spec.Strike, ttm, asOf)
	if !ok || vol <= 0 {
		return nil, false
	}
	r := rv.providers.Curve.Rate(ttm)
	basePrice := pricing.Price(spec.Type, spec.Exercise, spot, spec.Strike, ttm, r, 0, vol)
	if basePrice <= 0 {
		return nil, false // cannot scale off a zero base
	}

	shockedSpot := spot * (1 + shocks.PriceFrac)
	shockedVol := vol + shocks.GlobalVolBump + shocks.VolByUnderlying[spec.UnderlyingID]
	if shockedVol <= 0 {
		shockedVol = 1e-6 // a shock cannot drive vol non-positive
	}
	shockedPrice := pricing.Price(spec.Type, spec.Exercise, shockedSpot, spec.Strike, ttm, r, 0, shockedVol)

	ratio := shockedPrice / basePrice
	newAmount := decimalToFloat(baseMV.GetAmount()) * ratio
	return &commonpb.Money{Amount: floatToDecimal(newAmount, revalMoneyExp), CurrencyCode: baseMV.GetCurrencyCode()}, true
}

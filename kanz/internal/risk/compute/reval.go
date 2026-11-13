package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/pricing"
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

// RevalStatus separates a successfully priced option, an authoritatively known
// non-option, and an unassessed position. The zero value refuses classification.
type RevalStatus int

const (
	RevalUnassessed RevalStatus = iota
	RevalPriced
	RevalKnownLinear
)

// RevalResult carries the disposition and bounded reason into scenario coverage.
type RevalResult struct {
	Value  *commonpb.Money
	Status RevalStatus
	Reason string
}

// Revaluer reprices option positions under shocks using point-in-time inputs.
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
// sign (a short option keeps its negative value). Only RevalKnownLinear licenses
// a clean linear result. An unassessed position may be retained in a degraded
// linear approximation, but the caller must record the returned exclusion.
func (rv *Revaluer) RevalueOption(ctx context.Context, instrumentID string, asOf time.Time, baseMV *commonpb.Money, shocks RevalShocks) RevalResult {
	unassessed := func(reason string) RevalResult {
		skipOption(rv.providers, instrumentID, reason)
		return RevalResult{Status: RevalUnassessed, Reason: reason}
	}
	spec, resolution, reason := resolveOptionTerms(ctx, rv.providers.Terms, instrumentID, asOf)
	if resolution == TermsOtherVariant {
		return RevalResult{Status: RevalKnownLinear}
	}
	if resolution != TermsResolved {
		return unassessed(reason)
	}
	if baseMV == nil {
		return unassessed(SkipInvalidPricing)
	}
	baseAmount, readable := decutil.Float64(baseMV.GetAmount())
	if !readable {
		return unassessed(SkipInvalidPricing)
	}
	// NIL-GUARDED, like greeks.go. These two were dereferenced unconditionally
	// and a GreeksProviders with no Spot or Vol panicked on the first option —
	// the same defect, on the same struct, reached from the scenario path instead
	// of the measure path.
	var spot float64
	var ok bool
	if rv.providers.Spot != nil {
		spot, ok = rv.providers.Spot.Spot(ctx, spec.UnderlyingID, asOf)
	} else {
		ok = false
	}
	if !ok || spot <= 0 {
		return unassessed(SkipNoSpot)
	}
	ttm := spec.Expiry.Sub(asOf).Hours() / 24 / 365
	if ttm <= 0 {
		return unassessed(SkipExpired)
	}
	var vol float64
	if rv.providers.Vol != nil {
		vol, ok = rv.providers.Vol.Vol(ctx, spec.UnderlyingID, spec.Strike, ttm, asOf)
	} else {
		ok = false
	}
	if !ok || vol <= 0 || math.IsNaN(vol) || math.IsInf(vol, 0) {
		return unassessed(SkipNoVol)
	}
	r := rv.providers.Curve.Rate(ttm)
	basePrice := pricing.Price(spec.Type, spec.Exercise, spot, spec.Strike, ttm, r, 0, vol)
	if math.IsNaN(basePrice) || math.IsInf(basePrice, 0) {
		return unassessed(SkipInvalidPricing)
	}
	if basePrice <= 0 {
		return unassessed(SkipInvalidPricing) // cannot scale off a zero base
	}

	shockedSpot := spot * (1 + shocks.PriceFrac)
	shockedVol := vol + shocks.GlobalVolBump + shocks.VolByUnderlying[spec.UnderlyingID]
	if shockedVol <= 0 {
		shockedVol = 1e-6 // a shock cannot drive vol non-positive
	}
	shockedPrice := pricing.Price(spec.Type, spec.Exercise, shockedSpot, spec.Strike, ttm, r, 0, shockedVol)

	ratio := shockedPrice / basePrice
	newAmount := baseAmount * ratio
	if !representablePricingAmount(newAmount, revalMoneyExp) {
		return unassessed(SkipInvalidPricing)
	}
	return RevalResult{Status: RevalPriced, Value: &commonpb.Money{Amount: floatToDecimal(newAmount, revalMoneyExp), CurrencyCode: baseMV.GetCurrencyCode()}}
}

package compute

import (
	"context"
	"time"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/pricing"
)

// DERIV-01d wires real option Greeks into the RISK-07 measure registry,
// replacing the placeholder Delta with per-position pricing-derived
// sensitivities aggregated to the portfolio.
//
// # The seam mirrors the volatility / classifier enrichment
//
// Greeks need data a Position does not carry — the option's contract terms, the
// underlying spot, an implied vol, a discount rate. Rather than widen
// domain.Position, the measures close over four providers (the reference mirror
// of compute.ReturnsProvider / factor.Classifier) and resolve each per position
// point-in-time at p.AsOf(). The pure MeasureFunc contract is unchanged: the
// providers enter through the closure, exactly as BindReturns captures its ctx +
// ReturnsProvider. Registering them is one RegisterGreeks call.
//
// # Dollar Greeks
//
// Measure values are common.v1.Decimal in the portfolio base currency, so the
// Greeks are reported as dollar (cash) sensitivities of the portfolio value:
//
//	Delta = Σ Δ·S·n     Gamma = Σ Γ·S²·n     Vega = Σ vega·n
//	Theta = Σ θ·n        Rho   = Σ ρ·n
//
// where n = quantity × contract_multiplier. A non-option position (no contract
// terms) is linear in its underlying: it contributes its signed MarketValue to
// Delta (Δ≡1) and nothing to the higher-order Greeks — so a cash-equity book's
// Delta equals its net exposure, matching the placeholder it replaces.

// Greek measure names (RISK-07 naming: short, CamelCase). Delta reuses the
// existing constant — RegisterGreeks overwrites its placeholder implementation.
const (
	MeasureGamma v1.MeasureName = "Gamma"
	MeasureVega  v1.MeasureName = "Vega"
	MeasureTheta v1.MeasureName = "Theta"
	MeasureRho   v1.MeasureName = "Rho"
)

// greekExp is the precision Greek measure values are emitted at (1e-4). Greeks
// are float-derived analytics, not values of record, so a fixed sub-cent
// exponent is the right tradeoff (matches the volatility band's stance).
const greekExp int32 = -4

// OptionSpec is the contract terms the Greeks layer needs — the float working
// shape behind reference.v1.OptionTerms, resolved by a TermsProvider.
type OptionSpec struct {
	UnderlyingID string
	Strike       float64
	Expiry       time.Time
	Type         pricing.OptionType
	Exercise     pricing.Exercise
	Multiplier   float64 // contract multiplier (shares per contract)
}

// TermsProvider resolves an instrument's option contract terms as of a point in
// time. ok=false ⇒ the instrument is not an option (priced linearly).
type TermsProvider interface {
	OptionTerms(ctx context.Context, instrumentID string, asOf time.Time) (OptionSpec, bool)
}

// SpotProvider supplies an underlying's spot price as of a point in time.
type SpotProvider interface {
	Spot(ctx context.Context, instrumentID string, asOf time.Time) (float64, bool)
}

// VolProvider supplies an implied vol for an underlying at a strike/expiry as of
// a point in time — satisfied by a volsurface.Surface adapter.
type VolProvider interface {
	Vol(ctx context.Context, underlyingID string, strike, ttmYears float64, asOf time.Time) (float64, bool)
}

// GreeksProviders bundles the four seams the Greek measures resolve through.
// Curve defaults to a flat zero rate when nil.
type GreeksProviders struct {
	Terms TermsProvider
	Spot  SpotProvider
	Vol   VolProvider
	Curve pricing.DiscountCurve
}

// RegisterGreeks registers Delta/Gamma/Vega/Theta/Rho on r, each closing over
// ctx + providers. Delta overwrites the RISK-07 placeholder. Call at engine
// startup after DefaultRegistry; tests register against deterministic providers.
func RegisterGreeks(ctx context.Context, r *Registry, providers GreeksProviders) {
	if providers.Curve == nil {
		providers.Curve = pricing.FlatCurve(0)
	}
	r.Register(MeasureDelta, greekMeasure(ctx, MeasureDelta, providers, func(g pricing.Greeks, S, n float64) float64 { return g.Delta * S * n }))
	r.Register(MeasureGamma, greekMeasure(ctx, MeasureGamma, providers, func(g pricing.Greeks, S, n float64) float64 { return g.Gamma * S * S * n }))
	r.Register(MeasureVega, greekMeasure(ctx, MeasureVega, providers, func(g pricing.Greeks, _, n float64) float64 { return g.Vega * n }))
	r.Register(MeasureTheta, greekMeasure(ctx, MeasureTheta, providers, func(g pricing.Greeks, _, n float64) float64 { return g.Theta * n }))
	r.Register(MeasureRho, greekMeasure(ctx, MeasureRho, providers, func(g pricing.Greeks, _, n float64) float64 { return g.Rho * n }))
}

// greekMeasure builds a MeasureFunc that sums one selected dollar-Greek across
// positions. sel maps a position's per-unit Greeks + spot + position size to its
// dollar contribution.
func greekMeasure(ctx context.Context, name v1.MeasureName, p GreeksProviders, sel func(g pricing.Greeks, S, n float64) float64) MeasureFunc {
	return func(port *domain.Portfolio) v1.Measure {
		asOf := port.AsOf()
		var sum float64
		for _, pos := range port.Positions() {
			contrib, isOption := positionGreekContribution(ctx, p, pos, asOf, name, sel)
			if !isOption {
				// Linear (non-option) position: Δ≡1 ⇒ contributes its signed
				// MarketValue to Delta, nothing to higher-order Greeks.
				if name == MeasureDelta {
					sum += decimalToFloat(pos.MarketValue.GetAmount())
				}
				continue
			}
			sum += contrib
		}
		return v1.Measure{Name: name, Value: floatToDecimal(sum, greekExp)}
	}
}

// positionGreekContribution prices one option position and returns its selected
// dollar-Greek. isOption=false when the position is not an option or its pricing
// inputs are unavailable (the caller falls back to the linear treatment).
func positionGreekContribution(ctx context.Context, p GreeksProviders, pos domain.Position, asOf time.Time, name v1.MeasureName, sel func(g pricing.Greeks, S, n float64) float64) (float64, bool) {
	if p.Terms == nil {
		return 0, false
	}
	spec, ok := p.Terms.OptionTerms(ctx, string(pos.InstrumentID), asOf)
	if !ok {
		return 0, false
	}
	spot, ok := p.Spot.Spot(ctx, spec.UnderlyingID, asOf)
	if !ok || spot <= 0 {
		return 0, false
	}
	ttm := spec.Expiry.Sub(asOf).Hours() / 24 / 365
	if ttm <= 0 {
		return 0, true // expired ⇒ zero Greeks, but still an option (no linear fallback)
	}
	vol, ok := p.Vol.Vol(ctx, spec.UnderlyingID, spec.Strike, ttm, asOf)
	if !ok || vol <= 0 {
		return 0, true
	}
	r := p.Curve.Rate(ttm)
	_, g := pricing.PriceGreeks(spec.Type, spec.Exercise, spot, spec.Strike, ttm, r, 0 /*q*/, vol)
	mult := spec.Multiplier
	if mult == 0 {
		mult = 1
	}
	n := decimalToFloat(pos.Quantity) * mult
	return sel(g, spot, n), true
}

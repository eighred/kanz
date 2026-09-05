package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing"
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
//
// That linear fallback is reserved for a position with NO contract terms. An
// option whose terms resolve but whose spot or vol does not contributes zero to
// every Greek and is reported through GreeksProviders.OnSkip; it is never folded
// into the linear sum, which would report a contract as Δ≡1 stock.

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

	// OnSkip is called when a position that IS an option contributes zero to
	// every Greek because a pricing input was unavailable. Optional; nil disables
	// it.
	//
	// WITHOUT IT THE DEGRADATION IS INVISIBLE, and it is invisible in the
	// flattering direction. An option that cannot be priced contributes 0 to
	// Delta and 0 to Gamma/Vega/Theta/Rho, so the book's measured convexity
	// SHRINKS — and a Gamma of zero is indistinguishable from a portfolio holding
	// no options at all. That is the #257 shape (ten copies of the base-currency
	// filter each dropped positions and not one recorded it, so a USD book
	// holding only EUR reported GrossExposure = 0) and the #509 shape (a bond
	// dropped for want of a curve) again, one layer over.
	//
	// reason is one of SkipNoSpot / SkipNoVol / SkipExpired — a small closed set
	// rather than free text, because the caller counts by it and a metric label
	// must not be whatever a future edit writes.
	//
	// NOT CALLED FOR A NON-OPTION. A share has no contract terms and is correctly
	// priced linearly (Δ≡1) rather than skipped; firing here would drown the
	// signal in every equity position on the book.
	//
	// FIRES ONCE PER MEASURE, NOT ONCE PER POSITION. RegisterGreeks installs five
	// measures over the same per-position path, so one unpriceable option in one
	// ComputeMeasures call reports five times with the same (instrumentID,
	// reason). Count distinct pairs, or read the counter as "skip events", never
	// as "positions dropped".
	OnSkip func(instrumentID, reason string)
}

// The reasons an option contributes zero to every Greek. Each of these means the
// position is an option and was NOT priced; none of them is the linear path.
const (
	// SkipNoSpot: the option's terms resolved and no usable underlying spot did —
	// no SpotProvider is wired at all, or it has no mark for the underlying as of
	// the valuation time, or the mark is non-positive. Those are one reason and
	// not three because the consequence is one thing: with no spot there is no
	// price and therefore no sensitivity. Which of the three it was is knowable
	// only inside the provider, and that is where it is distinguished.
	SkipNoSpot = "no_spot"
	// SkipNoVol: spot resolved and no usable implied vol did — no VolProvider, no
	// point on the surface at that strike/expiry, or a non-positive vol. This is
	// the calibration gap rather than a data gap: the option is known, its
	// underlying is marked, and the surface cannot quote it.
	SkipNoVol = "no_vol"
	// SkipExpired: the option's expiry is at or before the valuation time. This is
	// the one skip whose zero is arithmetically CORRECT — an expired option really
	// does have no Greeks — and it is still reported, because an expired contract
	// sitting on the book at valuation time is a settlement that has not been
	// processed or a stale reference-data record, and the measures are where that
	// first becomes visible to anyone.
	SkipExpired = "expired"
)

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
					sum += decutil.Float64Or(pos.MarketValue.GetAmount(), 0)
				}
				continue
			}
			sum += contrib
		}
		// THE MODEL, NOT THE MEASURE NAME, IS WHAT SEPARATES THIS FROM THE
		// PLACEHOLDER. MeasureDelta is served by this function when
		// RegisterGreeks has run and by compute.Delta (net exposure) when it has
		// not, and nothing on the wire told the two apart before #1037.
		return v1.Measure{
			Name:       name,
			Value:      floatToDecimal(sum, greekExp),
			Provenance: v1.MeasureProvenance{Method: v1.MethodOptionPricingGreeks},
		}
	}
}

// positionGreekContribution prices one option position and returns its selected
// dollar-Greek.
//
// isOption=false means ONLY "this instrument has no option contract terms" — the
// caller's linear Δ≡1 fallback is correct for exactly that case and for nothing
// else. Every failure AFTER the terms resolve returns isOption=true with a zero
// contribution, because by then the position is known to be an option and the
// linear fallback would be a lie about its shape.
//
// It used to be a lie. A spot lookup that failed returned isOption=false, so an
// option whose underlying was unmarked was added to Delta at its full signed
// MarketValue — priced as Δ≡1 stock, with Gamma zero — silently. Returning
// isOption=true instead swaps a wild OVERSTATEMENT of Delta for an understated
// zero, which is the safer of the two and still wrong; OnSkip is what makes
// either one visible, and is why the reason is reported rather than logged.
//
// A nil Spot or Vol provider is handled here rather than refused at registration.
// That follows positionBondRisk, which likewise absorbs a nil provider per
// position instead of failing RegisterFIRisk: the misconfiguration surfaces on
// the first event either way, and only here does the instrument ID exist to name
// in the report. Refusing in RegisterGreeks would need it to return an error or
// panic, which is a wider change than the defect warrants.
func positionGreekContribution(ctx context.Context, p GreeksProviders, pos domain.Position, asOf time.Time, name v1.MeasureName, sel func(g pricing.Greeks, S, n float64) float64) (float64, bool) {
	if p.Terms == nil {
		return 0, false
	}
	spec, ok := p.Terms.OptionTerms(ctx, string(pos.InstrumentID), asOf)
	if !ok {
		// NOT REPORTED. Most positions on any book are not options, and
		// OptionTerms answers ok=false for every one of them — the provider's own
		// observer is where a MISSING load is distinguished from a share, because
		// only it can see which. Counting here would make the signal the noise.
		return 0, false
	}
	spot, spotOK := 0.0, false
	if p.Spot != nil {
		spot, spotOK = p.Spot.Spot(ctx, spec.UnderlyingID, asOf)
	}
	if !spotOK || spot <= 0 {
		skipOption(p, string(pos.InstrumentID), SkipNoSpot)
		return 0, true
	}
	ttm := spec.Expiry.Sub(asOf).Hours() / 24 / 365
	if ttm <= 0 {
		skipOption(p, string(pos.InstrumentID), SkipExpired)
		return 0, true
	}
	vol, volOK := 0.0, false
	if p.Vol != nil {
		vol, volOK = p.Vol.Vol(ctx, spec.UnderlyingID, spec.Strike, ttm, asOf)
	}
	if !volOK || vol <= 0 {
		skipOption(p, string(pos.InstrumentID), SkipNoVol)
		return 0, true
	}
	r := p.Curve.Rate(ttm)
	_, g := pricing.PriceGreeks(spec.Type, spec.Exercise, spot, spec.Strike, ttm, r, 0 /*q*/, vol)
	mult := spec.Multiplier
	if mult == 0 {
		mult = 1
	}
	n := decutil.Float64Or(pos.Quantity, 0) * mult
	return sel(g, spot, n), true
}

// skipOption reports an option that priced to nothing, if the caller asked to
// hear about them.
func skipOption(p GreeksProviders, instrumentID, reason string) {
	if p.OnSkip != nil {
		p.OnSkip(instrumentID, reason)
	}
}

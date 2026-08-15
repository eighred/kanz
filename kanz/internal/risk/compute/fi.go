package compute

import (
	"context"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// FI-01d wires real fixed-income rate risk into the RISK-07 measure registry —
// the bond sibling of the DERIV-01d Greeks. It registers DV01, (effective)
// Duration, Convexity, and SpreadDuration as portfolio measures, each pricing
// every bond position off the FI-01b discount curve resolved point-in-time.
//
// # The seam mirrors the Greeks enrichment
//
// Bond risk needs data a Position does not carry — the bond's contract terms and
// the discount curve. As with the Greek measures, the measures close over two
// providers (BondTermsProvider + CurveProvider, the FI mirror of Terms/Spot/Vol/
// Curve) resolved per position at p.AsOf(); the pure MeasureFunc contract is
// unchanged. Register them with one RegisterFIRisk call after DefaultRegistry.
//
// # Aggregation
//
//	DV01     = Σ dv01_bond · qty                 (dollar, base currency)
//	Duration = Σ |MV_i|·effDur_i / Σ |MV_i|      (MV-weighted years)
//	Convexity= Σ |MV_i|·convexity_i / Σ |MV_i|   (MV-weighted)
//	SpreadDuration = same as Duration for a bullet bond — a parallel credit-
//	  spread shift is a parallel discount-curve shift; they diverge only once
//	  optionality/OAS lands (STRUCT-01d). Registered distinctly so the seam and
//	  the dashboard wiring exist now.
//
// A non-bond position (no terms) contributes nothing to any FI measure.

// FI measure names (RISK-07 naming: short, CamelCase).
const (
	MeasureDV01           v1.MeasureName = "DV01"
	MeasureDuration       v1.MeasureName = "Duration"
	MeasureConvexity      v1.MeasureName = "Convexity"
	MeasureSpreadDuration v1.MeasureName = "SpreadDuration"
)

// fiDV01Exp is the precision DV01 (a dollar amount) is emitted at: cents.
const fiDV01Exp int32 = -2

// fiRatioExp is the precision the dimensionless duration/convexity measures are
// emitted at (1e-4), matching the Greek band's stance for float-derived ratios.
const fiRatioExp int32 = -4

// BondSpec is the bond contract terms the FI risk layer needs — the float
// working shape behind reference.v1.BondTerms, resolved by a BondTermsProvider.
type BondSpec struct {
	Face       float64
	CouponRate float64
	Frequency  int
	Issue      time.Time
	Maturity   time.Time
	DayCount   pricing.DayCount
	Currency   string
	IssuerID   string
}

// BondTermsProvider resolves an instrument's bond terms as of a point in time.
// ok=false ⇒ the instrument is not a bond (excluded from the FI measures).
type BondTermsProvider interface {
	BondTerms(ctx context.Context, instrumentID string, asOf time.Time) (BondSpec, bool)
}

// CurveProvider supplies the discount curve for a currency as of a point in
// time — the bootstrapped FI-01b curve.Curve.
type CurveProvider interface {
	Curve(ctx context.Context, currency string, asOf time.Time) (*curve.Curve, bool)
}

// FIProviders bundles the two seams the FI measures resolve through.
type FIProviders struct {
	Terms BondTermsProvider
	Curve CurveProvider

	// OnSkip is called when a position is EXCLUDED from the FI measures for a
	// reason that is not "it is not a bond". Optional; nil disables it.
	//
	// WITHOUT IT THE EXCLUSION IS INVISIBLE, and invisible in the flattering
	// direction. A bond dropped for want of a discount curve contributes 0 to
	// DV01 and nothing to the duration average, so the book's measured rate risk
	// SHRINKS — and a DV01 of zero is indistinguishable from a portfolio holding
	// no bonds at all. That is the #257 shape exactly: ten copies of the
	// base-currency filter each dropped positions and not one recorded it, so a
	// USD book holding only EUR reported GrossExposure = 0.
	//
	// reason is one of SkipNoTerms / SkipNoCurve — a small closed set rather than
	// free text, because the caller counts by it and a metric label must not be
	// whatever a future edit writes.
	//
	// NOT CALLED FOR A NON-BOND. A share has no terms and is correctly absent
	// from a bond measure; firing here would drown the signal in every equity
	// position on the book.
	//
	// FIRES ONCE PER MEASURE, NOT ONCE PER POSITION. RegisterFIRisk installs four
	// measures over the same per-position path, so one unpriceable bond in one
	// ComputeMeasures call reports four times with the same (instrumentID,
	// reason). Read the counter as "skip events", never as "positions dropped" —
	// a gauge built on the latter reading overstates by the measure count, and
	// silently changes meaning if a fifth FI measure is ever added.
	OnSkip func(instrumentID, reason string)
}

// The reasons a position is excluded from the FI measures despite being a bond.
const (
	// SkipNoTerms: the instrument resolved no usable bond terms. Either they were
	// never loaded, or they are present and unusable (an unspecified day count, a
	// maturity at or before issue). The terms provider distinguishes those two in
	// its own observer; here they are one thing, because the consequence is one
	// thing.
	SkipNoTerms = "no_terms"
	// SkipNoCurve: the bond's terms resolved, and no discount curve exists for
	// its currency as of the valuation time. This is the calibration gap rather
	// than a data gap — the bond is known and cannot be priced.
	SkipNoCurve = "no_curve"
)

// RegisterFIRisk registers DV01/Duration/Convexity/SpreadDuration on r, each
// closing over ctx + providers. Call at engine startup after DefaultRegistry;
// tests register against deterministic providers.
func RegisterFIRisk(ctx context.Context, r *Registry, providers FIProviders) {
	r.Register(MeasureDV01, fiMeasure(ctx, MeasureDV01, providers))
	r.Register(MeasureDuration, fiMeasure(ctx, MeasureDuration, providers))
	r.Register(MeasureConvexity, fiMeasure(ctx, MeasureConvexity, providers))
	r.Register(MeasureSpreadDuration, fiMeasure(ctx, MeasureSpreadDuration, providers))
}

// fiMeasure builds the MeasureFunc for one FI risk measure. DV01 is a dollar
// sum; the others are |MarketValue|-weighted averages across bond positions.
func fiMeasure(ctx context.Context, name v1.MeasureName, p FIProviders) MeasureFunc {
	return func(port *domain.Portfolio) v1.Measure {
		asOf := port.AsOf()
		var dollar, weighted, weight float64
		for _, pos := range port.Positions() {
			cr, qty, mv, ok := positionBondRisk(ctx, p, pos, asOf)
			if !ok {
				continue
			}
			switch name {
			case MeasureDV01:
				dollar += cr.DV01 * qty
			case MeasureConvexity:
				weighted += mv * cr.Convexity
				weight += mv
			default: // Duration, SpreadDuration
				weighted += mv * cr.EffectiveDuration
				weight += mv
			}
		}
		if name == MeasureDV01 {
			return v1.Measure{Name: name, Value: floatToDecimal(dollar, fiDV01Exp)}
		}
		ratio := 0.0
		if weight != 0 {
			ratio = weighted / weight
		}
		return v1.Measure{Name: name, Value: floatToDecimal(ratio, fiRatioExp)}
	}
}

// positionBondRisk prices one bond position off the discount curve and returns
// its curve risk, quantity, and |MarketValue| weight. ok=false when the position
// is not a bond or its pricing inputs (terms / curve) are unavailable.
func positionBondRisk(ctx context.Context, p FIProviders, pos domain.Position, asOf time.Time) (pricing.CurveRisk, float64, float64, bool) {
	if p.Terms == nil || p.Curve == nil {
		return pricing.CurveRisk{}, 0, 0, false
	}
	spec, ok := p.Terms.BondTerms(ctx, string(pos.InstrumentID), asOf)
	if !ok {
		// NOT REPORTED. The overwhelming majority of positions on any book are
		// not bonds, and BondTerms answers ok=false for every one of them — the
		// provider's own observer is where a MISSING load is distinguished from a
		// share, because only it can see which. Counting here would make the
		// signal the noise.
		return pricing.CurveRisk{}, 0, 0, false
	}
	c, ok := p.Curve.Curve(ctx, spec.Currency, asOf)
	if !ok || c == nil {
		// REPORTED, because this one is unambiguous: the terms resolved, so this
		// IS a bond, and it is about to leave the book's measured rate risk
		// without appearing anywhere as a gap.
		skip(p, pos, SkipNoCurve)
		return pricing.CurveRisk{}, 0, 0, false
	}
	bond := pricing.Bond{
		Face:       spec.Face,
		CouponRate: spec.CouponRate,
		Frequency:  spec.Frequency,
		Issue:      spec.Issue,
		Maturity:   spec.Maturity,
		DayCount:   spec.DayCount,
	}
	cr := bond.CurveRisk(asOf, c)
	qty := decimalToFloat(pos.Quantity)
	mv := decimalToFloat(pos.MarketValue.GetAmount())
	if mv < 0 {
		mv = -mv
	}
	return cr, qty, mv, true
}

// skip reports an exclusion, if the caller asked to hear about them.
func skip(p FIProviders, pos domain.Position, reason string) {
	if p.OnSkip != nil {
		p.OnSkip(string(pos.InstrumentID), reason)
	}
}

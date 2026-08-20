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
// A position a terms record identifies as something other than a bond
// contributes nothing to any FI measure, and that absence is an answer. A
// position the terms store has NEVER HEARD OF is a different thing entirely and
// is reported as an exclusion — see TermsResolution and #527.

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

// TermsResolution is a terms provider's answer about one instrument.
//
// IT REPLACES A BOOL, AND THE BOOL IS THE DEFECT (#527). `(BondSpec, bool)`
// collapsed two answers whose consequences are opposite: "there is a terms
// record and it describes something that is not a bond" — correctly absent from
// a rate measure, and the engine can say so — and "the store holds no record of
// this instrument at all", where the engine knows NOTHING about it and must not
// pretend it has certified it as a non-bond. Because both were false, a book of
// bonds nobody had loaded terms for produced exactly the response of a book of
// shares: DV01 = 0, no exclusions, no flag.
//
// SHARED WITH THE STRUCTURED FAMILY (#572), which is why TermsOtherVariant is
// spelled the way it is rather than "TermsNotABond". The four answers are
// properties of the ContractTerms RECORD — absent, usable, a different oneof
// variant, present-and-unusable — and not of any one instrument family. #572
// gave the structured family a schema, a Kind and a store row, which is what
// made its own bool widenable at all: before that, "a record exists and says
// this is positively not a securitization" was true of no deployment and
// reachable by no test, so the enum could not honestly be reused.
type TermsResolution int

const (
	// TermsUnknown: the store holds no record for the instrument, so its asset
	// class is unknown and it may be a bond.
	//
	// THE ZERO VALUE, ON PURPOSE. A provider that falls through without deciding
	// says "I do not know" — which flags the response — rather than "not a
	// bond", which would silently restore the confident zero this type exists to
	// prevent.
	TermsUnknown TermsResolution = iota
	// TermsResolved: usable bond terms. The only value that prices.
	TermsResolved
	// TermsOtherVariant: a terms record exists and carries a DIFFERENT oneof
	// variant than the one asked for — an option, a swap, a future, a bond or a
	// securitization. The ONLY value that licenses a confident absence: a share
	// is correctly missing from DV01, and reporting it would bury the bonds that
	// really were dropped under every equity on the book. Read by the structured
	// family the same way (#572).
	TermsOtherVariant
	// TermsUnusable: a record of the RIGHT variant exists and cannot be priced
	// from — for a bond, an unspecified day count, a maturity at or before issue,
	// no currency to look a curve up by; for a securitization, a held_tranche
	// naming no tranche in the deal, a prepayment model whose parameters the
	// schema does not carry, an absent quoted OAS. Definitely of this family,
	// definitely excluded.
	TermsUnusable
)

// BondTermsProvider resolves an instrument's bond terms as of a point in time.
// See TermsResolution for what each answer licenses; only TermsResolved carries
// a usable BondSpec.
type BondTermsProvider interface {
	BondTerms(ctx context.Context, instrumentID string, asOf time.Time) (BondSpec, TermsResolution)
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
	// WITHOUT IT THE EXCLUSION IS INVISIBLE. A bond dropped for want of a
	// discount curve contributes 0 to DV01 and nothing to the duration average,
	// and a DV01 of zero is indistinguishable from a portfolio holding no bonds
	// at all. That is the #257 shape exactly: ten copies of the base-currency
	// filter each dropped positions and not one recorded it, so a USD book
	// holding only EUR reported GrossExposure = 0.
	//
	// THE DIRECTION IS NOT UNIFORM ACROSS THESE FOUR MEASURES, and an earlier
	// version of this comment said it was ("the book's measured rate risk
	// SHRINKS"). It shrinks for DV01, which is a sum. Duration, Convexity and
	// SpreadDuration are |MV|-WEIGHTED AVERAGES, so dropping a short-dated bond
	// RAISES them — three bonds of duration 8/2/5 average 5.0, and without the 2
	// the same measure reports 6.5. Neither direction is safe to assume; see
	// v1.InputCoverage.
	//
	// reason is one of SkipNoTerms / SkipNoCurve — a small closed set rather than
	// free text, because the caller counts by it and a metric label must not be
	// whatever a future edit writes.
	//
	// NOT CALLED FOR A NON-BOND, nor for an instrument the store has no record
	// of. A share is correctly absent from a bond measure; firing here would
	// drown the signal in every equity position on the book. An instrument with
	// NO terms record at all is a real gap and is counted already — by the terms
	// provider's own missing-terms observer, which is the only layer that can
	// see the difference between "no record" and "a record for something else"
	// (kanz_risk_fi_terms_missing_total). Counting it here too would put one
	// fact in two metrics and let them disagree.
	//
	// THIS IS A METRIC, AND IT DOES NOT REPLACE THE RESPONSE ANNOTATION — nor
	// the reverse. They answer different questions for different people at
	// different times. OnSkip is a counter an operator watches across all
	// portfolios and only sees if they are looking; the InputCoverage this
	// measure attaches to its own value travels WITH the number, to the one
	// caller acting on that one portfolio, at the moment they act. #527 is
	// precisely the case where the counter existed and was not enough: the skips
	// WERE counted, and the DV01 on the wire was still a confident zero.
	//
	// FIRES ONCE PER MEASURE, NOT ONCE PER POSITION. RegisterFIRisk installs four
	// measures over the same per-position path, so one unpriceable bond in one
	// ComputeMeasures call reports four times with the same (instrumentID,
	// reason). Read the counter as "skip events", never as "positions dropped" —
	// a gauge built on the latter reading overstates by the measure count, and
	// silently changes meaning if a fifth FI measure is ever added.
	OnSkip func(instrumentID, reason string)
}

// The reasons a position is excluded from the FI measures.
const (
	// SkipNoTerms: a bond terms record exists and cannot be used — an unspecified
	// day count, a maturity at or before issue, no currency to price in. This is
	// definitely a bond and it is definitely out of every FI measure.
	//
	// IT USED TO MEAN "never loaded OR unusable", on the argument that "here they
	// are one thing, because the consequence is one thing". The consequence is
	// one thing; the REPAIR is not, and that is what the reason label is read
	// for. It also meant the constant was never actually emitted — nothing in
	// this file passed it to skip() — so the series sat at zero while every bond
	// on the estate fell out (#527).
	SkipNoTerms = "no_terms"
	// SkipNoCurve: the bond's terms resolved, and no discount curve exists for
	// its currency as of the valuation time. This is the calibration gap rather
	// than a data gap — the bond is known and cannot be priced.
	SkipNoCurve = "no_curve"
	// SkipUnknownInstrument: the terms store holds NO record for the instrument,
	// so the engine cannot say it is not a bond.
	//
	// NOT REPORTED THROUGH OnSkip — see FIProviders.OnSkip — but recorded as
	// response evidence, because on a book whose reference data was never loaded
	// this is every position, and a DV01 computed over none of them is the #527
	// zero. Its presence in the evidence is what separates "measured nothing"
	// from "measured a book with no bonds in it".
	//
	// SHARED WITH THE STRUCTURED FAMILY (#572), which is the one reason constant
	// that crosses families. The fact it states is not about bonds — the engine
	// holds no record saying what this instrument is — and structured.go reaches
	// it for the stronger version of the same gap: no ContractTerms variant, no
	// terms.Kind and no store can describe a structured product at all, so its
	// provider's ok=false certifies nothing either. One string, because an
	// operator groups and alerts on the string.
	SkipUnknownInstrument = "unknown_instrument"
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
//
// EVERY RETURN CARRIES ITS COVERAGE. The value alone cannot distinguish a book
// that priced no bonds from a book that holds none, and until #527 it did not
// try: the accumulator was returned regardless of whether anything had reached
// it. The zero is still emitted — see the flag-versus-omission argument on
// QualityFlagInputsUnresolved — but it now travels with a count of what it could
// not see.
func fiMeasure(ctx context.Context, name v1.MeasureName, p FIProviders) MeasureFunc {
	return func(port *domain.Portfolio) v1.Measure {
		asOf := port.AsOf()
		var dollar, weighted, weight float64
		var cov Coverage
		for _, pos := range port.Positions() {
			cr, qty, mv, reason := positionBondRisk(ctx, p, pos, asOf)
			if reason != "" {
				cov.Exclude(pos.InstrumentID, reason)
				continue
			}
			if !cr.priceable {
				continue // a terms record says this is not a bond: correctly absent
			}
			cov.Contributed++
			switch name {
			case MeasureDV01:
				dollar += cr.risk.DV01 * qty
			case MeasureConvexity:
				weighted += mv * cr.risk.Convexity
				weight += mv
			default: // Duration, SpreadDuration
				weighted += mv * cr.risk.EffectiveDuration
				weight += mv
			}
		}
		if name == MeasureDV01 {
			return v1.Measure{Name: name, Value: floatToDecimal(dollar, fiDV01Exp), Coverage: cov.Result()}
		}
		ratio := 0.0
		if weight != 0 {
			ratio = weighted / weight
		}
		return v1.Measure{Name: name, Value: floatToDecimal(ratio, fiRatioExp), Coverage: cov.Result()}
	}
}

// bondRisk is one position's curve risk plus whether it priced at all.
// priceable=false with an empty exclusion reason is the ONE benign case: a terms
// record exists and says this is not a bond.
type bondRisk struct {
	risk      pricing.CurveRisk
	priceable bool
}

// positionBondRisk prices one bond position off the discount curve and returns
// its curve risk, quantity, and |MarketValue| weight. The reason is "" when
// nothing needs reporting — the position priced, or a terms record positively
// identified it as something other than a bond — and one of the Skip* constants
// when the position could not be assessed.
func positionBondRisk(ctx context.Context, p FIProviders, pos domain.Position, asOf time.Time) (bondRisk, float64, float64, string) {
	if p.Terms == nil || p.Curve == nil {
		// A MISSING SEAM IS AN EXCLUSION, NOT A NON-BOND. Absorbed here rather
		// than refused at registration (matching positionGreekContribution), but
		// absorbed loudly: with no provider the engine has certified nothing, and
		// reporting the whole book unassessed is what makes that visible on the
		// first query instead of on a dashboard nobody opened.
		return bondRisk{}, 0, 0, SkipUnknownInstrument
	}
	spec, res := p.Terms.BondTerms(ctx, string(pos.InstrumentID), asOf)
	switch res {
	case TermsOtherVariant:
		// THE ONLY CONFIDENT ABSENCE. A record exists and describes an option, a
		// swap, a future or a securitization, so this position carries no bond rate risk
		// and its absence from DV01 is an answer rather than a gap. Not reported
		// to OnSkip and not recorded as an exclusion — counting every equity would
		// make the signal the noise, and would flag every response on the estate.
		return bondRisk{}, 0, 0, ""
	case TermsUnusable:
		return bondRisk{}, 0, 0, skipFI(p, pos, SkipNoTerms)
	case TermsResolved:
		// fall through to pricing
	default: // TermsUnknown
		// NOT A SHARE — AN INSTRUMENT NOBODY HAS TOLD THIS ENGINE ABOUT. The store
		// has no record, so "not a bond" is a guess, and it is the guess that
		// produced #527: with no production writer for the contract-terms store,
		// every position on every book takes this branch and the four FI measures
		// were reporting an unqualified zero off it.
		return bondRisk{}, 0, 0, SkipUnknownInstrument
	}
	c, ok := p.Curve.Curve(ctx, spec.Currency, asOf)
	if !ok || c == nil {
		// REPORTED, because this one is unambiguous: the terms resolved, so this
		// IS a bond, and it is about to leave the book's measured rate risk
		// without appearing anywhere as a gap.
		return bondRisk{}, 0, 0, skipFI(p, pos, SkipNoCurve)
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
	return bondRisk{risk: cr, priceable: true}, qty, mv, ""
}

// skipFI reports an exclusion to the observer, if the caller asked to hear about
// them, and returns the reason so the call site records it on the response too.
// Returning it is what stops the two surfaces drifting: there is no way to fire
// the counter without also carrying the evidence.
func skipFI(p FIProviders, pos domain.Position, reason string) string {
	if p.OnSkip != nil {
		p.OnSkip(string(pos.InstrumentID), reason)
	}
	return reason
}

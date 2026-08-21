package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"math"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing/structured"
)

// STRUCT-01d wires structured-product rate risk into the RISK-07 measure registry
// — the securitized sibling of the FI-01d bond measures. It registers effective
// duration, effective convexity, and weighted-average life, each repricing a
// structured position through the STRUCT-01b waterfall + STRUCT-01c prepay model
// resolved point-in-time, the same enrichment seam as FI/Greeks.
//
//	StructDuration  = Σ |MV_i|·effDur_i / Σ |MV_i|     (MV-weighted years)
//	StructConvexity = Σ |MV_i|·effConv_i / Σ |MV_i|    (negative for callable MBS)
//	StructWAL       = Σ |MV_i|·WAL_i / Σ |MV_i|        (MV-weighted years)
//
// A position the provider does not resolve contributes nothing AND IS RECORDED
// AS AN EXCLUSION. It used to be dropped silently, which is the same statement
// as "measured, and it carries no rate risk".
//
// # What this family can and cannot be asked about (#572)
//
// SECURITIZED STRUCTURES ONLY — MBS, ABS, CMO, tranched credit: a collateral
// pool, a capital structure, and prepayment behaviour, which is what
// reference.v1.StructuredTerms describes and what the waterfall/OAS machinery
// prices. STRUCTURED NOTES ARE NOT EXPRESSIBLE and that is the ruling, not an
// omission: barriers, memory coupons, autocall observation schedules and
// participation legs say WHETHER OR WHEN a cashflow happens along a path, which
// is a payoff program rather than a deal description. A note carrying one
// resolves to no StructuredTerms record, so it is refused with coverage rather
// than priced as if the option leg were not there.

// Structured measure names (RISK-07 naming: short, CamelCase).
const (
	MeasureStructDuration  v1.MeasureName = "StructDuration"
	MeasureStructConvexity v1.MeasureName = "StructConvexity"
	MeasureStructWAL       v1.MeasureName = "StructWAL"
)

// structRiskExp is the precision the dimensionless structured measures emit at.
const structRiskExp int32 = -4

// structDurationBumpBp is the parallel shift the effective duration/convexity
// reprice under.
const structDurationBumpBp = 25.0

// StructuredSpec is the per-instrument structured-product pricing context — the
// deal, the prepay model, the tranche THIS instrument is, the currency its
// discount curve is keyed by, and the OAS it is carried at. The float working
// shape behind reference.v1.StructuredTerms, resolved by a StructuredProvider.
//
// IT NO LONGER CARRIES THE RATE ENVIRONMENT (#572). The curve is resolved by
// StructuredProviders.Curve at measurement time from Currency, exactly as the FI
// band resolves a bond's curve from BondSpec.Currency — one curve source for
// both families, and "this deal has no calibrated curve" becomes SkipNoCurve
// (the calibration gap) instead of arriving here as a spec with a nil curve that
// the pricer can only report as a malformed input.
type StructuredSpec struct {
	Deal   structured.Deal
	Prepay structured.PrepayModel
	// Currency is the ISO 4217 code the deal pays in — the discount curve's key.
	Currency string
	// TrancheIndex is the position of the HELD tranche in Deal.Tranches.
	TrancheIndex int
	// OAS is the option-adjusted spread the tranche is carried at. A provider
	// must never default this: reference.v1.StructuredTerms.quoted_oas is
	// explicitly optional so that "flat to the curve" and "nobody said" are
	// different records, and a record that did not say is refused.
	OAS float64
}

// StructuredProvider resolves an instrument's structured-product spec as of a
// point in time.
//
// # This seam IS the widened one now, and #572 is why it could be
//
// It used to answer `(StructuredSpec, bool)`, and the comment here said the bool
// could not mean "positively not a structured product" because nothing on the
// estate could describe one: no ContractTerms variant, no terms.Kind, and
// reference.v1.StructuredTerms read and written by no Go code. So every equity
// on the book took the same branch as an unloaded MBS, and the only safe reading
// was "the engine does not know what this is" — which flagged every response.
//
// That premise is what #572 removed. The oneof carries a `structured` case,
// terms.KindStructured labels the row, and termsource.Provider reads it, so a
// ContractTerms record carrying an OPTION variant now positively certifies that
// the instrument is not a securitization. TermsOtherVariant is that answer and
// it is the one that stops the refusal firing on every share — the exact
// widening fi.go took for the identical defect in #527, taken here on the same
// evidence rather than in advance of it.
//
// TermsUnknown is still the ZERO VALUE, so a provider that falls through
// without deciding says "I do not know" and flags the response, rather than
// silently restoring the confident zero this family's refusal path exists to
// prevent (#585).
type StructuredProvider interface {
	Structured(ctx context.Context, instrumentID string, asOf time.Time) (StructuredSpec, TermsResolution)
}

// StructuredProviders bundles the two seams the structured measures resolve
// through, mirroring FIProviders — deliberately, because they are the same two
// facts (what is this instrument, and what curve does it discount on) and a
// second shape for them would be a second thing to keep right.
type StructuredProviders struct {
	Terms StructuredProvider
	Curve CurveProvider

	// OnSkip is called when a position is EXCLUDED from the structured measures
	// for a reason that is not "it is not a securitization". Optional; nil
	// disables it. See FIProviders.OnSkip for why the counter and the response
	// coverage are both needed and neither replaces the other — and for why it
	// fires once per MEASURE rather than once per position (three measures here,
	// so read the counter as skip EVENTS).
	//
	// NOT CALLED FOR TermsUnknown. An instrument with no terms record at all is
	// counted by the terms provider's own missing-terms observer
	// (kanz_risk_fi_terms_missing_total), which is the only layer that can tell
	// "no record" from "a record for something else". Counting it here too would
	// put one fact in two metrics and let them disagree.
	OnSkip func(instrumentID, reason string)
}

// The reasons a position is excluded from the structured measures.
//
// SkipUnknownInstrument and SkipNoCurve are REUSED FROM fi.go rather than
// respelled. Both already state exactly this fact — the engine holds no record
// saying what the instrument is; the instrument is known and its currency has no
// calibrated discount curve — and the reason string is operator-facing
// vocabulary they group and alert on. Two spellings of one fact is the
// duplication that turns a fix in one family into a fix missing in another.
const (
	// SkipUnpriceableTranche: a spec RESOLVED, a curve was found, and the
	// structured pricer still refused it (structured.ErrUnpriceable) — a tranche
	// written down to nothing (so a relative sensitivity is 0/0), or one that
	// receives no principal at all (so it has no weighted-average life).
	//
	// SEPARATE FROM SkipUnusableDeal BECAUSE THE REPAIR IS DIFFERENT. An unusable
	// deal is a reference record that cannot be turned into a spec at all, and it
	// is fixed by loading it correctly; this one is a spec that priced to a
	// degenerate answer, which is a fact about the deal's state.
	SkipUnpriceableTranche = "unpriceable_tranche"
	// SkipUnusableDeal: a STRUCTURED terms record exists and cannot be turned
	// into a spec — a held_tranche naming no tranche in the deal, a prepayment
	// model whose parameters reference.v1.PrepaymentAssumption does not carry, an
	// absent quoted OAS, a pool with no balance or no term. Definitely a
	// securitization, definitely out of all three measures.
	SkipUnusableDeal = "unusable_deal"
	// SkipNoMarketValue: the position IS structured and carries no mark, so it
	// can neither be weighted nor weigh anything else. Excluded rather than
	// skipped: these three measures are |MV|-weighted averages, and a holding
	// dropped from a weighted average moves it in whichever direction its own
	// value sat relative to the mean (v1.InputCoverage says why no direction can
	// be assumed).
	SkipNoMarketValue = "no_market_value"
)

// RegisterStructuredRisk registers StructDuration/StructConvexity/StructWAL on r,
// each closing over ctx + the providers. Call at engine startup after
// DefaultRegistry; tests register against deterministic providers.
//
// IT HAS A PRODUCTION CALLER SINCE #572 — services/risk-engine, inside the same
// calibration gate the FI band sits behind, because both need a discount curve
// and neither is honest without one. Its entry in
// test/arch/no_dark_measure_seam_test.go is deleted on the same commit; that
// guard's dead-entry arm insists once a seam has a caller, which is what keeps
// an exemption from outliving its repair.
func RegisterStructuredRisk(ctx context.Context, r *Registry, providers StructuredProviders) {
	r.Register(MeasureStructDuration, structMeasure(ctx, MeasureStructDuration, providers))
	r.Register(MeasureStructConvexity, structMeasure(ctx, MeasureStructConvexity, providers))
	r.Register(MeasureStructWAL, structMeasure(ctx, MeasureStructWAL, providers))
}

// structMeasure builds the MeasureFunc for one structured measure — a |MV|-
// weighted average across structured positions.
//
// EVERY RETURN CARRIES ITS COVERAGE, and for this family that is the ONLY thing
// standing between a dark seam and #565's zero-capital risk class (#572). The
// weighted average emits 0 when nothing reached it, exactly as the FI measures
// did before #527, and a StructDuration of 0.0000 is an active claim that a book
// of mortgage tranches does not move when rates move.
//
// # Why coverage here and a returned error one layer down
//
// The pricing layer refuses: structured.EffectiveRisk and TrancheCashflow.WAL
// return ErrUnpriceable rather than zero, which is #565's shape, and it fits
// there because those functions have one kind of caller and an error is
// expressible in their signature.
//
// It is NOT expressible here. MeasureFunc is func(*domain.Portfolio) v1.Measure
// — twenty-six measures across eight families, one registry shared by the query
// path, the scenario path and the degraded cache. Widening that signature to
// carry an error would be the largest blast radius in the risk module bought for
// a channel the platform ALREADY HAS: coverage travels with the value through
// Lookup, through a measure filter and off the cache, engine.go raises
// QualityFlagInputsUnresolved from it, and riskview declines to fold any measure
// whose coverage reports exclusions (#527/#566).
//
// So the boundary is the rule: REFUSE WHERE AN ERROR CAN BE RETURNED, RECORD AN
// EXCLUSION WHERE IT CANNOT. What must never happen is the third option this
// code took before — folding the refusal in at zero, with full market-value
// weight, into an average somebody sizes a position against. WIRING THE SEAM IN
// #572 DID NOT WEAKEN THAT: every arm below either contributes a priced tranche
// or records why it did not, and the only silent one is the arm where a terms
// record positively says the instrument is something else.
func structMeasure(ctx context.Context, name v1.MeasureName, p StructuredProviders) MeasureFunc {
	return func(port *domain.Portfolio) v1.Measure {
		asOf := port.AsOf()
		var weighted, weight float64
		var cov Coverage
		for _, pos := range port.Positions() {
			// A MISSING SEAM IS AN EXCLUSION, NOT A NON-STRUCTURED POSITION —
			// positionBondRisk's stance for the same case, and it also removes
			// the nil-interface panic a registration with no provider used to
			// take on its first position.
			if p.Terms == nil || p.Curve == nil {
				cov.Exclude(pos.InstrumentID, SkipUnknownInstrument)
				continue
			}
			spec, res := p.Terms.Structured(ctx, string(pos.InstrumentID), asOf)
			switch res {
			case TermsOtherVariant:
				// THE ONLY CONFIDENT ABSENCE. A record exists and carries an
				// option, a swap, a future or a bond, so this position genuinely
				// holds no tranche and its absence is an answer rather than a
				// gap. Not recorded — counting every equity would flag every
				// response on the estate, which is the noise #585's refusal was
				// careful not to become.
				continue
			case TermsUnusable:
				cov.Exclude(pos.InstrumentID, skipStructured(p, pos, SkipUnusableDeal))
				continue
			case TermsResolved:
				// fall through to pricing
			default: // TermsUnknown
				// NOT A SHARE — AN INSTRUMENT NOBODY HAS TOLD THIS ENGINE ABOUT.
				// The store holds no record, so "not a securitization" is a
				// guess, and it is the guess that produced #527 one family over.
				cov.Exclude(pos.InstrumentID, SkipUnknownInstrument)
				continue
			}
			if pos.MarketValue == nil {
				cov.Exclude(pos.InstrumentID, skipStructured(p, pos, SkipNoMarketValue))
				continue
			}
			crv, ok := p.Curve.Curve(ctx, spec.Currency, asOf)
			if !ok {
				cov.Exclude(pos.InstrumentID, skipStructured(p, pos, SkipNoCurve))
				continue
			}
			val, err := structValue(name, spec, structured.RateEnv{Curve: crv})
			if err != nil {
				cov.Exclude(pos.InstrumentID, skipStructured(p, pos, SkipUnpriceableTranche))
				continue
			}
			mv := math.Abs(decutil.Float64Or(pos.MarketValue.GetAmount(), 0))
			cov.Contributed++
			weighted += mv * val
			weight += mv
		}
		ratio := 0.0
		if weight != 0 {
			ratio = weighted / weight
		}
		return v1.Measure{Name: name, Value: floatToDecimal(ratio, structRiskExp), Coverage: cov.Result()}
	}
}

// skipStructured reports an exclusion to the OnSkip observer and returns the
// reason, so the counter and the response coverage cannot disagree about what
// was dropped.
func skipStructured(p StructuredProviders, pos domain.Position, reason string) string {
	if p.OnSkip != nil {
		p.OnSkip(string(pos.InstrumentID), reason)
	}
	return reason
}

// structValue is one position's contribution to one structured measure, or the
// pricer's refusal. Split out so all three measures take the same refusal path:
// StructWAL used to index Projection.Tranches directly and PANIC on a tranche
// index the deal does not hold, while StructDuration returned 0.0 for the same
// malformed spec — one fault, two behaviours, and the quieter one was the
// dangerous one.
func structValue(name v1.MeasureName, spec StructuredSpec, env structured.RateEnv) (float64, error) {
	switch name {
	case MeasureStructWAL:
		t, err := spec.Deal.Project(spec.Prepay, env).Tranche(spec.TrancheIndex)
		if err != nil {
			return 0, err
		}
		return t.WAL()
	case MeasureStructConvexity:
		_, conv, err := structured.EffectiveRisk(spec.Deal, spec.Prepay, env, spec.TrancheIndex, spec.OAS, structDurationBumpBp)
		return conv, err
	default: // MeasureStructDuration
		dur, _, err := structured.EffectiveRisk(spec.Deal, spec.Prepay, env, spec.TrancheIndex, spec.OAS, structDurationBumpBp)
		return dur, err
	}
}

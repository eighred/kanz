package compute

import (
	"context"
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
// as "measured, and it carries no rate risk" — see StructuredProvider for why
// the engine cannot tell those two apart today, and #572 for the schema ruling
// that would let it.

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
// deal, the prepay model, the rate environment, the held tranche, and the OAS the
// position is marked at. The float working shape behind reference.v1.
// StructuredTerms, resolved by a StructuredProvider.
type StructuredSpec struct {
	Deal         structured.Deal
	Prepay       structured.PrepayModel
	Env          structured.RateEnv
	TrancheIndex int
	OAS          float64
}

// StructuredProvider resolves an instrument's structured-product spec as of a
// point in time.
//
// ok=false MEANS "THIS ENGINE DOES NOT KNOW WHAT THIS INSTRUMENT IS", NOT "IT IS
// NOT STRUCTURED", and that reading is the fix in #572. The bool cannot carry
// the second meaning: reference.v1.ContractTerms' oneof carries option, swap,
// future and bond and NOTHING structured, terms.Kinds() has no structured label
// to store a row under, and reference.v1.StructuredTerms — which exists in the
// schema — is read and written by no Go code in this module. So there is no
// store that could certify a position as "positively not a structured product",
// and a provider answering false has certified nothing. Read the other way, as
// this file used to, every equity on the book silently licenses the measures to
// report a weighted average over the empty set.
//
// # The seam is still NOT widened into a resolution, and that is still deliberate
//
// fi.go's repair for the identical defect was to replace the bool with
// TermsResolution, separating "a record exists and it is not a bond" from "no
// record at all". That widening cannot be verified here: with no wire type, no
// store and no producer for a StructuredSpec, the "a record exists and says
// something else" arm would be reachable by no test and true of no deployment —
// a contract invented from nothing. #572 is where the schema question is
// decided (a structured.v1 message, a payoff DSL, or retiring the family), and
// nothing in this file makes that decision or presumes an outcome.
//
// WHAT THIS FILE DOES INSTEAD is make the family fail closed until that ruling
// lands: every position this provider declines is recorded as an INPUT
// EXCLUSION, so the three measures arrive with Contributed=0 and a non-zero
// ExcludedCount — v1.QualityFlagInputsUnresolved on the response, and a measure
// riskview refuses to fold into any limit. Wiring RegisterStructuredRisk today
// therefore yields a refusal rather than the plausible number over a book it
// priced nothing of. The day a real StructuredProvider exists, the way to stop
// the refusal firing on every equity is to widen this seam to match fi.go —
// which is only possible once the schema exists, which is the order #572 needs.
type StructuredProvider interface {
	Structured(ctx context.Context, instrumentID string, asOf time.Time) (StructuredSpec, bool)
}

// The reasons a position is excluded from the structured measures.
//
// SkipUnknownInstrument is REUSED FROM fi.go rather than respelled. It already
// means exactly this — the engine holds no record that says what the instrument
// is, so it cannot certify the absence — and the reason string is operator-
// facing vocabulary they group and alert on. Two spellings of one fact is the
// duplication that turns a fix in one family into a fix missing in another.
const (
	// SkipUnpriceableTranche: a spec RESOLVED and the structured pricer refused
	// it (structured.ErrUnpriceable) — no discount curve in the rate
	// environment, a tranche index the deal does not contain, a tranche that
	// prices to zero or receives no principal.
	//
	// SEPARATE FROM SkipUnknownInstrument BECAUSE THE REPAIR IS DIFFERENT. An
	// unknown instrument is missing reference data; this one is a spec that
	// arrived malformed, which is a producer bug, and merging them would send
	// whoever reads the exclusion to the wrong place.
	SkipUnpriceableTranche = "unpriceable_tranche"
	// SkipNoMarketValue: the position IS structured and carries no mark, so it
	// can neither be weighted nor weigh anything else. Excluded rather than
	// skipped: these three measures are |MV|-weighted averages, and a holding
	// dropped from a weighted average moves it in whichever direction its own
	// value sat relative to the mean (v1.InputCoverage says why no direction can
	// be assumed).
	SkipNoMarketValue = "no_market_value"
)

// RegisterStructuredRisk registers StructDuration/StructConvexity/StructWAL on r,
// each closing over ctx + the provider. Call at engine startup after
// DefaultRegistry; tests register against a deterministic provider.
//
// IT STILL HAS NO PRODUCTION CALLER, and #572 is where that is decided — this
// seam is exempted in test/arch/no_dark_measure_seam_test.go with its blocker
// named. What changed in #572 is what happens IF it is wired: the three measures
// now report a refusal (Contributed=0, every position excluded) instead of a
// weighted average over the empty set. Wiring it no longer needs to wait for the
// measures to be made safe; it waits only for a StructuredSpec that a store can
// hold.
func RegisterStructuredRisk(ctx context.Context, r *Registry, provider StructuredProvider) {
	r.Register(MeasureStructDuration, structMeasure(ctx, MeasureStructDuration, provider))
	r.Register(MeasureStructConvexity, structMeasure(ctx, MeasureStructConvexity, provider))
	r.Register(MeasureStructWAL, structMeasure(ctx, MeasureStructWAL, provider))
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
// whose coverage reports exclusions (#527/#566). A second refusal channel beside
// that one would be a second answer competing with the first.
//
// So the boundary is the rule: REFUSE WHERE AN ERROR CAN BE RETURNED, RECORD AN
// EXCLUSION WHERE IT CANNOT. What must never happen is the third option this
// code took before — folding the refusal in at zero, with full market-value
// weight, into an average somebody sizes a position against.
func structMeasure(ctx context.Context, name v1.MeasureName, provider StructuredProvider) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		asOf := p.AsOf()
		var weighted, weight float64
		var cov Coverage
		for _, pos := range p.Positions() {
			// A MISSING SEAM IS AN EXCLUSION, NOT A NON-STRUCTURED POSITION —
			// positionBondRisk's stance for the same case, and it also removes
			// the nil-interface panic a registration with no provider used to
			// take on its first position.
			if provider == nil {
				cov.Exclude(pos.InstrumentID, SkipUnknownInstrument)
				continue
			}
			spec, ok := provider.Structured(ctx, string(pos.InstrumentID), asOf)
			if !ok {
				cov.Exclude(pos.InstrumentID, SkipUnknownInstrument)
				continue
			}
			if pos.MarketValue == nil {
				cov.Exclude(pos.InstrumentID, SkipNoMarketValue)
				continue
			}
			val, err := structValue(name, spec)
			if err != nil {
				cov.Exclude(pos.InstrumentID, SkipUnpriceableTranche)
				continue
			}
			mv := math.Abs(decimalToFloat(pos.MarketValue.GetAmount()))
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

// structValue is one position's contribution to one structured measure, or the
// pricer's refusal. Split out so all three measures take the same refusal path:
// StructWAL used to index Projection.Tranches directly and PANIC on a tranche
// index the deal does not hold, while StructDuration returned 0.0 for the same
// malformed spec — one fault, two behaviours, and the quieter one was the
// dangerous one.
func structValue(name v1.MeasureName, spec StructuredSpec) (float64, error) {
	switch name {
	case MeasureStructWAL:
		t, err := spec.Deal.Project(spec.Prepay, spec.Env).Tranche(spec.TrancheIndex)
		if err != nil {
			return 0, err
		}
		return t.WAL()
	case MeasureStructConvexity:
		_, conv, err := structured.EffectiveRisk(spec.Deal, spec.Prepay, spec.Env, spec.TrancheIndex, spec.OAS, structDurationBumpBp)
		return conv, err
	default: // MeasureStructDuration
		dur, _, err := structured.EffectiveRisk(spec.Deal, spec.Prepay, spec.Env, spec.TrancheIndex, spec.OAS, structDurationBumpBp)
		return dur, err
	}
}

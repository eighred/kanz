package compute

import (
	"context"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
)

// FACTOR-01e wires the multi-factor risk model into the RISK-07 measure registry
// — the factor sibling of the FI/Greek/liquidity enrichment. It registers
// factor VaR and the systematic/specific risk split, each resolving the
// portfolio's factor model at p.AsOf() through FactorProviders.Model (the model is a
// universe-wide object rebuilt point-in-time, the per-snapshot analog of the
// per-instrument providers). Marginal/component risk contributions are vectors
// (one per instrument/factor), richer than a scalar measure — they are delivered
// through factormodel.Model.Decompose, not folded into the scalar registry.
//
//	FactorVaR99   = z₀.₉₉ · √(vᵀΣv)     (Σ = BFBᵀ + D, the factor covariance)
//	SystematicRisk = √(eᵀFe)            (factor/common risk, e = Bᵀv)
//	SpecificRisk   = √(Σ vᵢ²dᵢ)         (idiosyncratic/diversifiable risk)

// Factor measure names (RISK-07 naming: short, CamelCase).
const (
	MeasureFactorVaR99    v1.MeasureName = "FactorVaR99"
	MeasureSystematicRisk v1.MeasureName = "SystematicRisk"
	MeasureSpecificRisk   v1.MeasureName = "SpecificRisk"
)

// factorVaRExp / factorRiskExp are the money scales the factor measures emit at
// (cents) — they are dollar P&L figures like VaR.
const (
	factorVaRExp  int32 = -2
	factorRiskExp int32 = -2
)

// factorVaRConfidence is the confidence the FactorVaR99 measure uses, matching
// the MeasureVaR99 name.
const factorVaRConfidence = 0.99

// ModelProvider resolves the portfolio's factor model as of a knowledge horizon
// — the per-snapshot enrichment seam (the universe-wide analog of the FI/Greek
// per-instrument providers). ok=false ⇒ no model available.
//
// The factor measures then report zero rather than failing the recompute — one
// unfittable universe must not take down a snapshot that still has exposure,
// VaR and concentration to serve. That zero is reported through
// FactorProviders.OnSkip as SkipNoModel, which is the only thing that keeps it
// from being a lie.
type ModelProvider interface {
	Model(ctx context.Context, asOf time.Time) (*factormodel.Model, bool)
}

// FactorProviders bundles the seam the factor measures resolve through and the
// observer that makes their zeros honest.
//
// A STRUCT RATHER THAN A BARE ModelProvider ARGUMENT, matching FIProviders and
// GreeksProviders. The observer is not the last field this seam will ever grow
// (a benchmark for ex-ante tracking error is the obvious next one), and the two
// alternatives both cost more than they save: a variadic option would make this
// package hold THREE spellings of one concept — the exact "one implementation
// per concept" failure the #257 base-currency filter was — and a fourth
// positional argument puts the observer where nothing names it at the call site.
// The change is safe to make now precisely because this seam is still dark
// (test/arch/no_dark_measure_seam_test.go): the only callers are this package's
// tests, so the migration is two lines rather than a production edit.
type FactorProviders struct {
	// Model resolves the portfolio's factor model at the snapshot's asOf.
	Model ModelProvider

	// OnSkip is called when a factor measure evaluates to zero because the model
	// could not cover what was asked of it. Optional; nil disables it.
	//
	// WITHOUT IT THE DEGRADATION IS INVISIBLE, and invisible in the flattering
	// direction. A FactorVaR99 of zero is indistinguishable from a portfolio with
	// no factor risk, and the paths that produce it — an empty universe, a fit
	// that failed, too little return history, an instrument outside the universe
	// — all produce it SILENTLY. That is the #257 shape (ten copies of the
	// base-currency filter each dropped positions and not one recorded it, so a
	// USD book holding only EUR reported GrossExposure = 0) and the #509 shape (a
	// bond dropped for want of a curve) again, one layer up: here the whole book's
	// factor risk can vanish at once.
	//
	// reason is one of SkipNoModel / SkipNotInModel — a small closed set rather
	// than free text, because the caller counts by it and a metric label must not
	// be whatever a future edit writes.
	//
	// instrumentID IS EMPTY FOR SkipNoModel. That skip is a property of the
	// (portfolio, asOf) evaluation, not of any instrument — there is no model, so
	// there is nothing to attribute it to. The signature still matches
	// FIProviders.OnSkip and GreeksProviders.OnSkip so one observer can serve all
	// three seams; a caller labelling a metric by instrument must expect the empty
	// label and must not drop it, because the empty one is the whole-book event.
	//
	// FIRES ONCE PER MEASURE, NOT ONCE PER PORTFOLIO. RegisterFactorRisk installs
	// three measures over the same path, so one missing model in one
	// ComputeMeasures call reports SkipNoModel three times with an empty
	// instrumentID. Read the counter as "measure evaluations with no model", NEVER
	// as "portfolios affected": the fan-out is not even a fixed 3 to divide by,
	// because ComputeMeasures takes a name filter and a client asking only for
	// FactorVaR99 produces exactly one report. A "books with no factor model"
	// gauge cannot be built from this at all — build it beside the ModelProvider,
	// which sees each snapshot once.
	OnSkip func(instrumentID, reason string)
}

// The reasons a factor measure reports zero for something that has risk. Both
// mean "the arithmetic ran and the model had nothing to say", never "the book is
// flat".
const (
	// SkipNoModel: no factor model resolved for this snapshot — the provider
	// answered ok=false, returned nil, or no provider is wired at all. All three
	// are one reason because the consequence is one thing: every factor measure on
	// this evaluation is zero and NONE of them is a statement about the book.
	// Which of the three it was is knowable inside the provider
	// (LiveModelProvider distinguishes an empty universe from a failed fit), and
	// that is where it is distinguished.
	SkipNoModel = "no_model"
	// SkipNotInModel: a model resolved and one base-currency position is outside
	// its instrument universe, so it has no loadings and no specific variance.
	// Such a position contributes zero to BOTH halves of the split —
	// factormodel.FactorExposures skips an unknown id, and the specific term reads
	// a zero-valued SpecificVar — so the book's measured factor risk shrinks by
	// exactly the risk of the holding nobody modelled, in the direction that makes
	// a limit pass. factormodel.Model.FactorExposures says this is "surfaced as a
	// coverage concern a layer up, never silently treated as zero-risk"; THIS is
	// that layer, and until this constant existed the promise was unkept.
	//
	// Not the FI/Greeks "not a bond" / "not an option" case, which is deliberately
	// NOT reported. A non-bond is correctly absent from a bond measure, whereas
	// every base-currency position on the book is supposed to be in the factor
	// universe — the model is a universe-wide object fitted for this book, so an
	// absence is a coverage gap and not the common case. A book that reports this
	// for most of its positions is a book whose factor risk is mostly unmodelled,
	// which is the thing worth knowing.
	SkipNotInModel = "not_in_model"
)

// RegisterFactorRisk registers FactorVaR99 / SystematicRisk / SpecificRisk on r,
// each closing over ctx + providers. Call at engine startup after
// DefaultRegistry; tests register against a static model provider.
func RegisterFactorRisk(ctx context.Context, r *Registry, providers FactorProviders) {
	r.Register(MeasureFactorVaR99, factorMeasure(ctx, providers, MeasureFactorVaR99))
	r.Register(MeasureSystematicRisk, factorMeasure(ctx, providers, MeasureSystematicRisk))
	r.Register(MeasureSpecificRisk, factorMeasure(ctx, providers, MeasureSpecificRisk))
}

// factorMeasure builds the MeasureFunc for one factor measure. It resolves the
// model at p.AsOf(), folds base-currency positions into the dollar exposure map
// the model decomposes, and emits the requested scalar.
//
// A nil Model provider is absorbed here rather than refused at registration,
// following positionGreekContribution: the misconfiguration surfaces on the
// first event either way, and refusing in RegisterFactorRisk would need it to
// return an error or panic — a wider change than this warrants. It also used to
// panic outright on a nil interface, which is a worse first event than a
// reported zero.
func factorMeasure(ctx context.Context, providers FactorProviders, name v1.MeasureName) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		// NO MODEL IS ONE WHOLE-BOOK EXCLUSION, not one per position. The measure
		// never reached the book, so counting each holding would make an outage
		// look like a per-instrument data gap — see Coverage.ExcludeWhole.
		var cov Coverage
		zero := func() v1.Measure {
			cov.ExcludeWhole(SkipNoModel)
			skipFactor(providers, "", SkipNoModel)
			return v1.Measure{Name: name, Value: floatToDecimal(0, factorRiskExp), Coverage: cov.Result()}
		}
		if providers.Model == nil {
			return zero()
		}
		model, ok := providers.Model.Model(ctx, p.AsOf())
		if !ok || model == nil {
			return zero()
		}
		values := factorValues(p, model, providers, &cov)
		switch name {
		case MeasureFactorVaR99:
			return v1.Measure{Name: name, Value: floatToDecimal(model.VaR(values, factorVaRConfidence), factorVaRExp), Coverage: cov.Result()}
		case MeasureSystematicRisk:
			return v1.Measure{Name: name, Value: floatToDecimal(model.Risk(values).Systematic, factorRiskExp), Coverage: cov.Result()}
		default: // MeasureSpecificRisk
			return v1.Measure{Name: name, Value: floatToDecimal(model.Risk(values).Specific, factorRiskExp), Coverage: cov.Result()}
		}
	}
}

// factorValues folds the portfolio's base-currency positions into the
// instrument→signed-market-value map the factor model decomposes (the RISK-07
// same-currency convention), and reports every one of them the model does not
// cover. Other-currency positions are excluded first via the one shared rule
// (domain.Position.InBaseCurrency) and reported to the caller as
// v1.QualityFlagCurrencyExcluded (#257) — they are NOT reported as
// SkipNotInModel, which would double-count one exclusion under two names.
//
// An uncovered position is still put in the map. The model already ignores an id
// it does not know, so including it is arithmetically identical to dropping it
// here — and this function's job is to make the drop visible, not to move where
// it happens. Changing the arithmetic inside an observability fix is how a
// "reporting-only" change ships a silent number change.
func factorValues(p *domain.Portfolio, model *factormodel.Model, providers FactorProviders, cov *Coverage) map[string]float64 {
	base := p.BaseCurrency()
	values := make(map[string]float64)
	for _, pos := range p.Positions() {
		if !pos.InBaseCurrency(base) {
			continue
		}
		id := string(pos.InstrumentID)
		// Loading is the membership oracle deliberately: it consults the same
		// id→row index FactorExposures does, so this cannot report coverage the
		// arithmetic disagrees with. The returned copy is discarded — a K-float
		// allocation per uncovered position, paid only on the gap.
		if _, covered := model.Loading(id); !covered {
			skipFactor(providers, id, SkipNotInModel)
			cov.Exclude(pos.InstrumentID, SkipNotInModel)
		} else {
			cov.Contributed++
		}
		values[id] = decimalToFloat(pos.MarketValue.GetAmount())
	}
	return values
}

// skipFactor reports a factor measure that resolved to nothing, if the caller
// asked to hear about them.
func skipFactor(providers FactorProviders, instrumentID, reason string) {
	if providers.OnSkip != nil {
		providers.OnSkip(instrumentID, reason)
	}
}

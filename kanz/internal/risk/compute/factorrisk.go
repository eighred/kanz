package compute

import (
	"context"
	"time"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/factormodel"
)

// FACTOR-01e wires the multi-factor risk model into the RISK-07 measure registry
// — the factor sibling of the FI/Greek/liquidity enrichment. It registers
// factor VaR and the systematic/specific risk split, each resolving the
// portfolio's factor model at p.AsOf() through a ModelProvider (the model is a
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
// per-instrument providers). ok=false ⇒ no model available (the factor measures
// report zero rather than failing the recompute).
type ModelProvider interface {
	Model(ctx context.Context, asOf time.Time) (*factormodel.Model, bool)
}

// RegisterFactorRisk registers FactorVaR99 / SystematicRisk / SpecificRisk on r,
// each closing over ctx + the model provider. Call at engine startup after
// DefaultRegistry; tests register against a static model provider.
func RegisterFactorRisk(ctx context.Context, r *Registry, provider ModelProvider) {
	r.Register(MeasureFactorVaR99, factorMeasure(ctx, provider, MeasureFactorVaR99))
	r.Register(MeasureSystematicRisk, factorMeasure(ctx, provider, MeasureSystematicRisk))
	r.Register(MeasureSpecificRisk, factorMeasure(ctx, provider, MeasureSpecificRisk))
}

// factorMeasure builds the MeasureFunc for one factor measure. It resolves the
// model at p.AsOf(), folds base-currency positions into the dollar exposure map
// the model decomposes, and emits the requested scalar.
func factorMeasure(ctx context.Context, provider ModelProvider, name v1.MeasureName) MeasureFunc {
	return func(p *domain.Portfolio) v1.Measure {
		model, ok := provider.Model(ctx, p.AsOf())
		if !ok || model == nil {
			return v1.Measure{Name: name, Value: floatToDecimal(0, factorRiskExp)}
		}
		values := baseCurrencyValues(p)
		switch name {
		case MeasureFactorVaR99:
			return v1.Measure{Name: name, Value: floatToDecimal(model.VaR(values, factorVaRConfidence), factorVaRExp)}
		case MeasureSystematicRisk:
			return v1.Measure{Name: name, Value: floatToDecimal(model.Risk(values).Systematic, factorRiskExp)}
		default: // MeasureSpecificRisk
			return v1.Measure{Name: name, Value: floatToDecimal(model.Risk(values).Specific, factorRiskExp)}
		}
	}
}

// baseCurrencyValues folds the portfolio's base-currency positions into the
// instrument→signed-market-value map the factor model decomposes (the RISK-07
// same-currency convention; other-currency positions are skipped).
func baseCurrencyValues(p *domain.Portfolio) map[string]float64 {
	base := string(p.BaseCurrency())
	values := make(map[string]float64)
	for _, pos := range p.Positions() {
		if pos.MarketValue == nil || pos.MarketValue.CurrencyCode != base {
			continue
		}
		values[string(pos.InstrumentID)] = decimalToFloat(pos.MarketValue.GetAmount())
	}
	return values
}

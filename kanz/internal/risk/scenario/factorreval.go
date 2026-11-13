package scenario

import (
	"math"

	"github.com/eighred/kanz/internal/dec"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
)

// Factor-shock scenarios (FACTOR-01e). EvaluateFactorShock is the factor-space
// sibling of EvaluateReval / EvaluateCurveShift: it reprices every position by
// its model-implied return under a factor shock (factor name → factor return),
// then recomputes measures against the revalued state. An unassessed input
// refuses the factor evaluation with coverage. The existing linear/curve paths
// are unaffected (the DERIV-01e blast-radius discipline, one
// axis over: a factor shock expresses "momentum crashes 10%" the way no single
// price/sector shift can).

// EvaluateFactorShock applies a factor shock to a deep clone of p — repricing
// each position MVᵢ ← MVᵢ·(1 + Σ_k Bᵢₖ·shock_k) — then computes measures against
// the revalued state. p is not mutated. Empty shocks are an unshocked no-op;
// nonempty requests must be supported in full, including on an empty book.
func EvaluateFactorShock(p *domain.Portfolio, model *factormodel.Model, shocks map[string]float64, registry *compute.Registry) (*domain.MeasureSet, v1.InputCoverage) {
	var cov compute.Coverage
	if len(shocks) == 0 {
		return compute.ComputeMeasures(cloneCarry(p), registry, nil), cov.Result()
	}
	if model == nil {
		cov.ExcludeWhole(compute.SkipNoModel)
		return nil, cov.Result()
	}
	if !model.SupportsShocks(shocks) {
		cov.ExcludeWhole(SkipUnsupportedFactorShock)
		return nil, cov.Result()
	}
	cp := cloneCarry(p)
	for _, pos := range cp.Positions() {
		if pos.MarketValue == nil || pos.MarketValue.Amount == nil || !dec.InDomain(pos.MarketValue.Amount) {
			cov.Exclude(pos.InstrumentID, SkipUnassessedFactorInstrument)
			continue
		}
		r, ok := model.ShockReturn(string(pos.InstrumentID), shocks)
		if !ok {
			cov.Exclude(pos.InstrumentID, SkipUnassessedFactorInstrument)
			continue
		}
		// Retain the existing 1e-6 fraction convention, but refuse a finite
		// result outside that coefficient range before converting to int64.
		scaled := r * 1e6
		if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled < -0x1p63 || scaled >= 0x1p63 {
			cov.Exclude(pos.InstrumentID, SkipInvalidFactorShockResult)
			continue
		}
		if r != 0 {
			pos.MarketValue = compute.ShockMoney(pos.MarketValue, fracDecimal(r))
			if pos.MarketValue.Amount == nil {
				cov.Exclude(pos.InstrumentID, SkipInvalidFactorShockResult)
				continue
			}
			cp.SetPosition(pos)
		}
		cov.Contributed++
	}
	coverage := cov.Result()
	if coverage.ExcludedCount > 0 {
		return nil, coverage
	}
	return compute.ComputeMeasures(cp, registry, nil), coverage
}

// Factor-family refusal reasons are bounded coverage labels, never input names.
const (
	SkipUnsupportedFactorShock     = "unsupported_factor_shock"
	SkipUnassessedFactorInstrument = "unassessed_factor_instrument"
	SkipInvalidFactorShockResult   = "invalid_factor_shock_result"
)

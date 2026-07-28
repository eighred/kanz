package scenario

import (
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
)

// Factor-shock scenarios (FACTOR-01e). EvaluateFactorShock is the factor-space
// sibling of EvaluateReval / EvaluateCurveShift: it reprices every position by
// its model-implied return under a factor shock (factor name → factor return),
// then recomputes measures against the revalued state. A position outside the
// model universe is left unshocked. It is a separate entry point — the existing
// linear/curve paths are unaffected (the DERIV-01e blast-radius discipline, one
// axis over: a factor shock expresses "momentum crashes 10%" the way no single
// price/sector shift can).

// EvaluateFactorShock applies a factor shock to a deep clone of p — repricing
// each position MVᵢ ← MVᵢ·(1 + Σ_k Bᵢₖ·shock_k) — then computes measures against
// the revalued state. p is not mutated. A nil model or empty shock falls back to
// an unshocked ComputeMeasures.
func EvaluateFactorShock(p *domain.Portfolio, model *factormodel.Model, shocks map[string]float64, registry *compute.Registry) *domain.MeasureSet {
	if model == nil || len(shocks) == 0 {
		return compute.ComputeMeasures(cloneCarry(p), registry, nil)
	}
	cp := cloneCarry(p)
	for _, pos := range cp.Positions() {
		if pos.MarketValue == nil {
			continue
		}
		r, ok := model.ShockReturn(string(pos.InstrumentID), shocks)
		if !ok || r == 0 {
			continue
		}
		pos.MarketValue = compute.ShockMoney(pos.MarketValue, fracDecimal(r))
		cp.SetPosition(pos)
	}
	return compute.ComputeMeasures(cp, registry, nil)
}

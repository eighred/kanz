package factormodel

// FACTOR-01d — factor risk decomposition. Given a portfolio's per-instrument
// dollar exposures and a model, it splits total risk into factor exposures, a
// factor-vs-specific risk breakdown, per-factor and per-instrument risk
// contributions (each summing back to total risk by Euler's theorem), and the
// ex-ante tracking error against a benchmark.

// Decomposition is the full factor risk report for a portfolio.
type Decomposition struct {
	// FactorExposure is the portfolio's exposure to each factor (by name).
	FactorExposure map[string]float64
	// Systematic / Specific / Total are the risk (standard-deviation) split.
	Systematic float64
	Specific   float64
	Total      float64
	// FactorRiskContribution is each factor's contribution to Total risk; the
	// factor contributions plus SpecificRiskContribution sum to Total (Euler
	// decomposition of the risk into additive parts).
	FactorRiskContribution   map[string]float64
	SpecificRiskContribution float64
	// MarginalRisk / ComponentRisk are per-instrument: ComponentRisk sums to
	// Total (the instrument-level risk budget); MarginalRisk_i = ∂Total/∂v_i.
	MarginalRisk  map[string]float64
	ComponentRisk map[string]float64
}

// Decompose computes the full risk decomposition for a book whose per-instrument
// signed dollar exposures are `values`. Risk contributions use the standard
// Euler identity: with total risk σ = √(vᵀΣv), the marginal contribution of
// instrument i is (Σv)_i/σ and its component contribution v_i·(Σv)_i/σ, which
// sum to σ. The factor contributions e_k·(Fe)_k/σ plus the specific contribution
// (Σ v_i²d_i)/σ likewise sum to σ.
func (m *Model) Decompose(values map[string]float64) Decomposition {
	rb := m.Risk(values)
	total := rb.Total

	d := Decomposition{
		FactorExposure:         make(map[string]float64, len(m.Factors)),
		Systematic:             rb.Systematic,
		Specific:               rb.Specific,
		Total:                  total,
		FactorRiskContribution: make(map[string]float64, len(m.Factors)),
		MarginalRisk:           make(map[string]float64, len(values)),
		ComponentRisk:          make(map[string]float64, len(values)),
	}
	for k := range m.Factors {
		d.FactorExposure[m.factorName(k)] = rb.FactorExposure[k]
	}
	if total == 0 {
		return d // a flat / riskless book — every contribution is zero
	}

	// Per-factor contribution: e_k·(F e)_k / σ.
	fe := matVec(m.FactorCov, rb.FactorExposure)
	for k := range m.Factors {
		d.FactorRiskContribution[m.factorName(k)] = rb.FactorExposure[k] * fe[k] / total
	}
	d.SpecificRiskContribution = (rb.Specific * rb.Specific) / total

	// Per-instrument marginal/component contribution via (Σv)_i.
	perInstrument, _ := m.covApply(values)
	for id, v := range values {
		sigmaV := perInstrument[id]
		d.MarginalRisk[id] = sigmaV / total
		d.ComponentRisk[id] = v * sigmaV / total
	}
	return d
}

// TrackingError is the ex-ante (model-implied) tracking error of the portfolio
// against a benchmark: the total risk of the ACTIVE position (portfolio minus
// benchmark exposures). benchmarkValues are the benchmark's per-instrument dollar
// exposures (the PERF-01c BenchmarkDefinition constituent weights scaled to the
// portfolio's value), an input — factormodel does not reach into the performance
// module. TE = √(aᵀΣa) where a = v_p − v_b.
func (m *Model) TrackingError(portfolioValues, benchmarkValues map[string]float64) float64 {
	active := make(map[string]float64, len(portfolioValues)+len(benchmarkValues))
	for id, v := range portfolioValues {
		active[id] += v
	}
	for id, v := range benchmarkValues {
		active[id] -= v
	}
	return m.Risk(active).Total
}

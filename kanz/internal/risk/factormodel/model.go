// Package factormodel is the FACTOR-01 multi-factor risk model — the analytical
// core an Aladdin-class platform is built on. It expresses an instrument's return
// as a linear combination of common factor returns plus an idiosyncratic
// (specific) term:
//
//	r_i = Σ_k B_{i,k} · f_k + ε_i
//
// from which portfolio risk decomposes into a FACTOR (systematic) part and a
// SPECIFIC (diversifiable) part — so VaR, ex-ante tracking error, and risk
// attribution decompose by factor, not just by name. It ships both flavors: a
// FUNDAMENTAL model (observed style/industry/country loadings, factor returns by
// cross-sectional regression, FACTOR-01b) and a STATISTICAL model (PCA on the
// return covariance, FACTOR-01c), selectable/blendable per config.
//
// # Boundary
//
// factormodel is INSIDE kanz/internal/risk, so it composes directly with the
// compute / scenario layers (no RISK-02 edge). It reuses the MODEL-01c
// compute.ReturnsProvider for the return series both estimators read, the same
// point-in-time data source VaR uses — so factor VaR and historical VaR are
// estimated off one consistent history and reconcile (FACTOR-01f). A benchmark
// for ex-ante tracking error enters as a weight map (the PERF-01c
// BenchmarkDefinition constituents), an INPUT — not a reach into the performance
// module — the same "covariance is an input" stance OPT-01 takes.
package factormodel

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// FactorType is the category a factor belongs to — the dimension a risk report
// groups exposures by.
type FactorType int

const (
	// FactorStyle is a continuous cross-sectional characteristic (value,
	// momentum, size, quality, low-volatility).
	FactorStyle FactorType = iota
	// FactorIndustry is an industry/sector membership factor (0/1 loading).
	FactorIndustry
	// FactorCountry is a country/market membership factor (0/1 loading).
	FactorCountry
	// FactorMacro is a macroeconomic factor (rates, credit, oil) — loadings are
	// estimated sensitivities. FactorStatistical denotes a PCA-derived factor.
	FactorMacro
	// FactorStatistical is a latent principal-component factor (FACTOR-01c).
	FactorStatistical
)

// Factor identifies one factor by name and type. Order in Model.Factors is the
// column order of the loading matrix and the factor covariance.
type Factor struct {
	Name string
	Type FactorType
}

// Model is an assembled multi-factor risk model over a fixed instrument
// universe. It is a plain data holder (the estimators in fundamental.go /
// statistical.go build it; decompose.go and the compute layer consume it).
type Model struct {
	// Factors are the K factors, in loading-column order.
	Factors []Factor
	// Instruments is the N-instrument universe, in loading-row order.
	Instruments []string
	// Loadings is the N×K exposure matrix B: Loadings[i][k] is instrument i's
	// loading on factor k.
	Loadings [][]float64
	// FactorCov is the K×K factor-return covariance F (one-period).
	FactorCov [][]float64
	// SpecificVar is the per-instrument idiosyncratic variance d_i (one-period),
	// keyed by instrument id — the diagonal of the residual covariance.
	SpecificVar map[string]float64

	// ModelID and AsOf identify this fitted INSTANCE, and they are the reason a
	// number produced from it can be reproduced (#1039).
	//
	// Before they existed, a Model was an anonymous bag of matrices: the loadings
	// and covariance behind a published FactorVaR99 lived for the duration of one
	// call and nothing on the wire named them, so "reproduce last Tuesday's VaR"
	// had no referent to resolve. They are the same pair factor.v1 keys every one
	// of its messages by (model_id + as_of) and the same pair
	// domain.v1.MeasureProvenance carries onto each measure, so an output joins to
	// its inputs by equality rather than by inference.
	//
	// SET BY Fit AND NOWHERE ELSE. An estimator assembles the matrices; only Fit
	// knows the configuration and the as-of the caller asked for, which is
	// precisely what the pair has to name. Zero values mean the model was
	// assembled directly (a test fixture, blend's intermediate passes) and is not
	// a citable instance — absence is "this names no instance", never "the
	// instance is unnamed".
	ModelID string
	AsOf    time.Time

	// index memoizes instrument id → row for O(1) lookup.
	index map[string]int
}

// DefaultModelID derives a model's canonical id from the configuration that
// produced it — "STATISTICAL-3F-250D", "FUNDAMENTAL-250D", "BLEND-3F-250D".
//
// THE ESTIMATION PARAMETERS ARE IN THE ID ON PURPOSE, and it is the whole
// difference between an id that identifies a model and one that identifies a
// slot. factor.v1 keys every artifact by model_id + as_of and calls model_id
// "the canonical id of the model", which invites a bare "STATISTICAL" — and then
// a deployment that widens the lookback from 250 to 500 days publishes a
// materially different model under the id yesterday's number cited. The join
// still resolves, the numbers no longer reconcile, and nothing anywhere reports
// a change. Putting the window and the factor count in the id makes that a NEW
// model rather than a quiet redefinition of an old one.
//
// Config.ModelID overrides it, for a desk that names its models itself; the
// override is then that desk's promise that the name still identifies the fit.
func DefaultModelID(cfg Config) string {
	parts := []string{cfg.Type.String()}
	if cfg.Type != Fundamental {
		parts = append(parts, fmt.Sprintf("%dF", cfg.statFactors()))
	}
	parts = append(parts, fmt.Sprintf("%dD", cfg.window()))
	return strings.Join(parts, "-")
}

// String names the model type for DefaultModelID and for logs. An unrecognised
// value renders as UNKNOWN rather than a number: an id is read by a human
// reconstructing a number, and "3" tells them nothing.
func (t ModelType) String() string {
	switch t {
	case Statistical:
		return "STATISTICAL"
	case Blend:
		return "BLEND"
	case Fundamental:
		return "FUNDAMENTAL"
	default:
		return "UNKNOWN"
	}
}

// newModel wires the id→row index. Estimators call it once assembled.
func newModel(factors []Factor, instruments []string, loadings, factorCov [][]float64, specific map[string]float64) *Model {
	idx := make(map[string]int, len(instruments))
	for i, id := range instruments {
		idx[id] = i
	}
	return &Model{
		Factors:     factors,
		Instruments: instruments,
		Loadings:    loadings,
		FactorCov:   factorCov,
		SpecificVar: specific,
		index:       idx,
	}
}

// Loading returns instrument id's loading vector (a copy) and true, or nil/false
// when the instrument is outside the model universe.
func (m *Model) Loading(id string) ([]float64, bool) {
	i, ok := m.index[id]
	if !ok {
		return nil, false
	}
	return append([]float64(nil), m.Loadings[i]...), true
}

// FactorExposures returns the portfolio's factor exposure vector Bᵀv (per
// factor), where values maps instrument id → signed exposure (dollar market
// value). Instruments outside the universe are ignored — surfaced as a coverage
// concern a layer up, never silently treated as zero-risk.
func (m *Model) FactorExposures(values map[string]float64) []float64 {
	k := len(m.Factors)
	e := make([]float64, k)
	for id, v := range values {
		i, ok := m.index[id]
		if !ok {
			continue
		}
		row := m.Loadings[i]
		for f := 0; f < k; f++ {
			e[f] += v * row[f]
		}
	}
	return e
}

// RiskBreakdown is a portfolio's one-period risk split into its systematic
// (factor) and specific (idiosyncratic) standard deviations, in the units of the
// input exposures (dollar P&L when exposures are market values).
type RiskBreakdown struct {
	// FactorExposure is the per-factor exposure Bᵀv (factor order matches
	// Model.Factors).
	FactorExposure []float64
	// Systematic is √(eᵀ F e); Specific is √(Σ v_i² d_i); Total is √(sys²+spec²).
	Systematic float64
	Specific   float64
	Total      float64
}

// Risk computes the systematic/specific/total risk of a book whose per-
// instrument signed exposures (dollar market values) are `values`. The factor
// and specific variances are independent by construction (ε ⟂ f), so they add:
// Total² = eᵀFe + Σ v_i² d_i.
func (m *Model) Risk(values map[string]float64) RiskBreakdown {
	e := m.FactorExposures(values)
	sysVar := quadForm(m.FactorCov, e)
	if sysVar < 0 {
		sysVar = 0 // guard tiny negative round-off
	}
	var specVar float64
	for id, v := range values {
		specVar += v * v * m.SpecificVar[id]
	}
	return RiskBreakdown{
		FactorExposure: e,
		Systematic:     math.Sqrt(sysVar),
		Specific:       math.Sqrt(specVar),
		Total:          math.Sqrt(sysVar + specVar),
	}
}

// VaR returns the parametric one-period Value-at-Risk of a book at the given
// confidence: z_conf · total-risk, a money loss in the units of `values`
// (dollars when values are market values). Because the total risk is built from
// the SAME return covariance the historical/Monte-Carlo models resample, a
// statistical model fit on those returns reconciles factor VaR with MODEL-01 VaR
// (FACTOR-01f). confidence outside (0,1) falls back to 0.99.
func (m *Model) VaR(values map[string]float64, confidence float64) float64 {
	if confidence <= 0 || confidence >= 1 {
		confidence = 0.99
	}
	return normalQuantile(confidence) * m.Risk(values).Total
}

// ShockReturn returns the model-implied return of instrument id under a factor
// shock (factor name → factor-return shock): Σ_k loading_{i,k}·shock_k over the
// named factors. ok=false when the instrument is outside the universe (the
// scenario engine leaves it unshocked). The factor-space analog of the
// price/curve/liquidity shocks.
func (m *Model) ShockReturn(id string, shocks map[string]float64) (float64, bool) {
	i, ok := m.index[id]
	if !ok {
		return 0, false
	}
	row := m.Loadings[i]
	var r float64
	for k, f := range m.Factors {
		if s, ok := shocks[f.Name]; ok {
			r += row[k] * s
		}
	}
	return r, true
}

// instrumentCovRow returns (Σv)_i = B_i·(F·Bᵀv) + d_i·v_i for every instrument —
// the instrument-space covariance applied to the exposure vector, the building
// block of marginal/component risk contributions. fe is F·(Bᵀv), precomputed once.
func (m *Model) covApply(values map[string]float64) (perInstrument map[string]float64, total float64) {
	e := m.FactorExposures(values)
	fe := matVec(m.FactorCov, e) // F·e, a K-vector
	perInstrument = make(map[string]float64, len(values))
	for id, v := range values {
		i, ok := m.index[id]
		if !ok {
			continue
		}
		row := m.Loadings[i]
		var s float64
		for f := range fe {
			s += row[f] * fe[f]
		}
		s += m.SpecificVar[id] * v
		perInstrument[id] = s
		total += v * s
	}
	if total < 0 {
		total = 0
	}
	return perInstrument, math.Sqrt(total)
}

// sortedFactorNames returns the factor names in declaration order — a small
// helper for deterministic map-keyed output.
func (m *Model) factorName(k int) string { return m.Factors[k].Name }

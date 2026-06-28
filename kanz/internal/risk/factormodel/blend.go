package factormodel

import "math"

// ALT-01d (risk side) — blending illiquid NAVs into the factor risk view. A
// private fund has no model loadings of its own, so the alternatives module
// (internal/alternatives, OUTSIDE the risk boundary) maps its NAV onto PUBLIC
// factors via a proxy and hands the result here as a plain factor→dollar
// exposure map. This file folds that exogenous exposure into the model's factor
// risk so a whole-portfolio view spans the liquid book AND the alternatives
// sleeve on ONE factor model.
//
// Keeping the input a plain map (not an alternatives type) is deliberate: the
// risk module does not depend on the alternatives module, and any source of
// proxy exposure (a manual overlay, a different proxy library) blends the same
// way.

// BlendedFactorExposure returns the portfolio's named factor exposures with an
// exogenous proxy exposure added in: Bᵀv (over the liquid `values`) plus the
// proxy factor→dollar map. A proxy factor outside the model's factor set is kept
// (it is a real exposure the model simply does not price), so the caller can see
// full coverage rather than silently dropping it.
func (m *Model) BlendedFactorExposure(values map[string]float64, proxyExposure map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m.Factors)+len(proxyExposure))
	e := m.FactorExposures(values)
	for k := range m.Factors {
		out[m.factorName(k)] = e[k]
	}
	for factor, v := range proxyExposure {
		out[factor] += v
	}
	return out
}

// BlendedRisk computes the systematic/specific/total risk of the liquid book
// blended with proxy positions. Each proxy position carries its own factor
// loadings (its proxy betas) and a dollar NAV; its factor-space contribution is
// loading·NAV and its specific risk is taken as zero (the proxy is, by
// construction, fully explained by the public factors it maps to). The factor
// exposure is therefore Bᵀv + Σ_proxy NAV·beta, and total risk is √(eᵀFe + Σ
// specific) over the liquid names only.
//
// proxies maps a proxy id → (betas, nav). Only factors in the model's factor set
// contribute to risk (an unpriced proxy factor adds exposure but no modelled
// variance — surfaced via BlendedFactorExposure, not here).
func (m *Model) BlendedRisk(values map[string]float64, proxies map[string]ProxyPosition) RiskBreakdown {
	e := m.FactorExposures(values)
	for _, p := range proxies {
		for k, f := range m.Factors {
			if beta, ok := p.Betas[f.Name]; ok {
				e[k] += beta * p.NAV
			}
		}
	}
	sysVar := quadForm(m.FactorCov, e)
	if sysVar < 0 {
		sysVar = 0
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

// ProxyPosition is an illiquid position mapped onto public factors: its proxy
// betas (factor name → loading) and its dollar NAV. It mirrors the shape
// internal/alternatives.ProxyMapping produces, passed as plain data across the
// risk boundary.
type ProxyPosition struct {
	Betas map[string]float64
	NAV   float64
}

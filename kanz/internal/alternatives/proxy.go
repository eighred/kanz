package alternatives

// ALT-01d (alternatives side) — proxy-beta mapping. A private fund has no
// observable market price, so it carries no public-factor loadings of its own.
// The standard whole-portfolio treatment maps an illiquid NAV onto PUBLIC factors
// via a proxy: a private-equity buyout fund behaves like a levered small-cap
// equity exposure, real estate like a REIT/rates blend, private credit like a
// high-yield exposure. This module turns a NAV + a proxy mapping into a public-
// factor exposure vector the FACTOR-01 risk view can blend in (the risk-side
// blend lives in internal/risk/factormodel, which consumes the plain
// factor→dollar map this produces — alternatives never imports the risk module,
// RISK-02: the loadings are an INPUT).

// ProxyMapping maps a private strategy onto public-factor betas — the loadings a
// liquid proxy for this fund would carry. Betas is factor name → sensitivity
// (e.g. {"equity": 1.3, "size": 0.4} for a levered small-cap-like buyout proxy).
type ProxyMapping struct {
	// Name identifies the proxy (e.g. the strategy it models), for reporting.
	Name string
	// Betas are the public-factor loadings of the proxy (factor name → beta).
	Betas map[string]float64
}

// FactorExposure maps an illiquid NAV onto public-factor dollar exposures via the
// proxy betas: exposure_k = beta_k · NAV. The result is keyed by factor name,
// directly addable to a FACTOR-01 portfolio factor-exposure vector. A NAV of zero
// (or no betas) yields an empty map.
func (m ProxyMapping) FactorExposure(nav float64) map[string]float64 {
	if nav == 0 || len(m.Betas) == 0 {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(m.Betas))
	for factor, beta := range m.Betas {
		out[factor] = beta * nav
	}
	return out
}

// AggregateExposure sums the proxy factor exposures of several illiquid positions
// into one factor→dollar map — the alternatives sleeve's total public-factor
// exposure, ready to blend into the liquid book's factor exposure.
func AggregateExposure(exposures ...map[string]float64) map[string]float64 {
	total := map[string]float64{}
	for _, e := range exposures {
		for factor, v := range e {
			total[factor] += v
		}
	}
	return total
}

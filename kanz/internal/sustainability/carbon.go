// Package sustainability is the ESG / climate analytics core (CLIMATE-01): the
// portfolio ESG/carbon aggregation (weighted scores, financed emissions, exclusion
// screening), the climate-VaR scenarios (transition + physical), and net-zero
// alignment + TCFD/SFDR reporting. It is a pure analytics package OUTSIDE
// internal/risk (RISK-02): the climate scenario it produces is expanded to
// per-instrument shocks the risk scenario engine reprices, but the carbon data
// stays an INPUT here — the same stance the alternatives/optimization modules take.
//
// Everything is float64: ESG scores, emissions (tCO2e), carbon intensities, and
// financed emissions are derived statistics, the EVT-14 rule that makes returns
// double (money stays exact upstream and lands as float at this analytics edge).
package sustainability

import "sort"

// ESGScore is an issuer's ESG rating — the overall score and its three pillars
// (the working shape behind sustainability.v1.ESGScore). Higher is better.
type ESGScore struct {
	Overall       float64
	Environmental float64
	Social        float64
	Governance    float64
}

// CarbonMetrics is an issuer's GHG footprint and the financial denominators
// intensity and financed emissions are computed against (behind
// sustainability.v1.CarbonMetrics). Emissions are tCO2e; Revenue/EVIC are money.
type CarbonMetrics struct {
	Scope1 float64
	Scope2 float64
	Scope3 float64
	// Revenue is annual revenue — the denominator of carbon intensity.
	Revenue float64
	// EVIC is enterprise value including cash — the PCAF attribution denominator.
	EVIC float64
}

// FinancedScopes is scope1+scope2 (operational emissions, the WACI numerator);
// TotalScopes adds scope3 (the full value-chain footprint, used for PCAF).
func (c CarbonMetrics) FinancedScopes() float64 { return c.Scope1 + c.Scope2 }
func (c CarbonMetrics) TotalScopes() float64    { return c.Scope1 + c.Scope2 + c.Scope3 }

// Intensity is carbon intensity = (scope1+scope2) / revenue (tCO2e per $m, in the
// revenue unit supplied). Zero revenue ⇒ zero (undefined, degraded to zero rather
// than Inf — the risk-measure discipline).
func (c CarbonMetrics) Intensity() float64 {
	if c.Revenue <= 0 {
		return 0
	}
	return c.FinancedScopes() / c.Revenue
}

// Holding is one portfolio position with its ESG/carbon reference data — the unit
// aggregation runs over. MarketValue is the position's value (money, as float at
// this edge); the issuer's ESG/carbon attach by instrument.
type Holding struct {
	InstrumentID string
	MarketValue  float64
	ESG          ESGScore
	Carbon       CarbonMetrics
}

// WeightedAverageESG is the market-value-weighted ESG score across holdings —
// each pillar weighted by |MV|/Σ|MV|. An empty book (zero total value) yields a
// zero score.
func WeightedAverageESG(holdings []Holding) ESGScore {
	total := totalValue(holdings)
	if total <= 0 {
		return ESGScore{}
	}
	var out ESGScore
	for _, h := range holdings {
		w := abs(h.MarketValue) / total
		out.Overall += w * h.ESG.Overall
		out.Environmental += w * h.ESG.Environmental
		out.Social += w * h.ESG.Social
		out.Governance += w * h.ESG.Governance
	}
	return out
}

// WeightedAverageCarbonIntensity (WACI) is the portfolio's market-value-weighted
// carbon intensity — Σ wᵢ·intensityᵢ — the headline TCFD/SFDR carbon number.
// Reconciles exactly with the per-holding contributions (Σ wᵢ·intensityᵢ), the
// property CLIMATE-01e pins.
func WeightedAverageCarbonIntensity(holdings []Holding) float64 {
	total := totalValue(holdings)
	if total <= 0 {
		return 0
	}
	var waci float64
	for _, h := range holdings {
		waci += abs(h.MarketValue) / total * h.Carbon.Intensity()
	}
	return waci
}

// FinancedEmissions is the PCAF attributed absorption of issuer emissions to the
// portfolio: Σ (MVᵢ / EVICᵢ) · totalEmissionsᵢ — each holding owns the share of
// its issuer's emissions equal to its share of the issuer's enterprise value.
// This is the absolute carbon footprint (tCO2e) the portfolio finances. A holding
// with no EVIC contributes nothing (the attribution factor is undefined — skipped,
// surfaced as a data-coverage gap a layer up rather than silently zero-weighted
// into the total).
func FinancedEmissions(holdings []Holding) float64 {
	var financed float64
	for _, h := range holdings {
		if h.Carbon.EVIC <= 0 {
			continue
		}
		attribution := h.MarketValue / h.Carbon.EVIC
		financed += attribution * h.Carbon.TotalScopes()
	}
	return financed
}

// Contribution is one holding's contribution to WACI — its weight times its
// intensity. The contributions sum to the portfolio WACI (the reconciliation a
// report drills into).
type Contribution struct {
	InstrumentID string
	Weight       float64
	Intensity    float64
	Contribution float64 // Weight × Intensity
}

// CarbonContributions returns the per-holding WACI contributions in descending
// contribution order (the largest carbon contributors first — the engagement
// priority list), summing to WeightedAverageCarbonIntensity.
func CarbonContributions(holdings []Holding) []Contribution {
	total := totalValue(holdings)
	out := make([]Contribution, 0, len(holdings))
	if total <= 0 {
		return out
	}
	for _, h := range holdings {
		w := abs(h.MarketValue) / total
		intensity := h.Carbon.Intensity()
		out = append(out, Contribution{
			InstrumentID: h.InstrumentID,
			Weight:       w,
			Intensity:    intensity,
			Contribution: w * intensity,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Contribution != out[j].Contribution {
			return out[i].Contribution > out[j].Contribution
		}
		return out[i].InstrumentID < out[j].InstrumentID
	})
	return out
}

func totalValue(holdings []Holding) float64 {
	var t float64
	for _, h := range holdings {
		t += abs(h.MarketValue)
	}
	return t
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

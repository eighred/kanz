package sustainability

import "sort"

// CLIMATE-01c (sustainability side) — climate-VaR. A climate scenario reprices the
// book through two channels: TRANSITION (a carbon price charged against an
// issuer's emissions erodes its enterprise value) and PHYSICAL (climate-hazard
// damage to exposed assets). This module expands a ClimateScenario into a
// per-instrument return shock and a portfolio climate loss; the risk scenario
// engine applies those shocks as price shocks (the GICS/factor/credit
// scenario-axis pattern, here driven by carbon data the risk module does not hold).

// ClimateScenario is an NGFS-style climate stress: a carbon price (money per tCO2e)
// the transition channel charges emissions at, and a physical-severity factor in
// [0,1] the physical channel applies to exposed value.
type ClimateScenario struct {
	Name             string
	CarbonPrice      float64
	PhysicalSeverity float64
}

// TransitionShock is the fractional value loss from pricing an issuer's
// operational emissions: −(carbonPrice · (scope1+scope2)) / EVIC — the carbon cost
// as a share of enterprise value. Zero/unknown EVIC ⇒ no transition shock
// (undefined attribution). The result is ≤ 0 (a loss) for positive emissions.
func (s ClimateScenario) TransitionShock(c CarbonMetrics) float64 {
	if c.EVIC <= 0 || s.CarbonPrice <= 0 {
		return 0
	}
	cost := s.CarbonPrice * c.FinancedScopes()
	return -cost / c.EVIC
}

// PhysicalShock is the fractional value loss from physical climate damage:
// −physicalSeverity scaled by the issuer's carbon intensity relative to a
// reference, so carbon-intensive (typically physically-exposed) issuers take more.
// Here it is a flat −physicalSeverity on every holding (a uniform hazard), the
// conservative baseline a per-sector hazard map refines.
func (s ClimateScenario) PhysicalShock() float64 {
	if s.PhysicalSeverity <= 0 {
		return 0
	}
	return -s.PhysicalSeverity
}

// InstrumentShock is the combined climate return shock for a holding: the
// transition and physical channels add (both losses). It is ≤ 0.
func (s ClimateScenario) InstrumentShock(c CarbonMetrics) float64 {
	return s.TransitionShock(c) + s.PhysicalShock()
}

// InstrumentShocks expands the scenario into per-instrument return shocks — the
// map the risk scenario engine consumes as price shocks (one per held
// instrument). Instruments with no climate effect are omitted.
func (s ClimateScenario) InstrumentShocks(holdings []Holding) map[string]float64 {
	out := make(map[string]float64, len(holdings))
	for _, h := range holdings {
		if shock := s.InstrumentShock(h.Carbon); shock != 0 {
			out[h.InstrumentID] = shock
		}
	}
	return out
}

// ClimateVaR is the portfolio climate loss under the scenario: Σ |MVᵢ| · |shockᵢ|
// — a positive loss number (the value at risk to the climate scenario). It is
// monotone non-decreasing in the carbon price and the physical severity (a harsher
// scenario never lowers the loss), the property CLIMATE-01e pins.
//
// IT RETURNS THE ATTRIBUTION COVERAGE (#618), and shares it with financed
// emissions because it shares the datum: TransitionShock returns 0 for a holding
// with no EVIC, so an uncovered position takes the physical channel alone and the
// loss comes out LOW. Understating a value-at-risk is the direction that matters,
// and it looked identical to a book with no transition exposure.
func (s ClimateScenario) ClimateVaR(holdings []Holding) (float64, Coverage) {
	var loss float64
	for _, h := range holdings {
		shock := s.InstrumentShock(h.Carbon)
		loss += abs(h.MarketValue) * abs(shock)
	}
	return loss, AttributionCoverage(holdings)
}

// NGFS-style named scenarios — the climate counterpart of the GFC/COVID library.
// Carbon price and physical severity rise from an orderly transition (low price,
// low physical) through a disorderly one (high price) to a hot-house world (little
// transition, high physical damage). Illustrative magnitudes a quant recalibrates.

// NGFSOrderly is an orderly, early, well-anticipated transition: a moderate carbon
// price, little physical damage.
func NGFSOrderly() ClimateScenario {
	return ClimateScenario{Name: "NGFS_ORDERLY", CarbonPrice: 50, PhysicalSeverity: 0.01}
}

// NGFSDisorderly is a late, abrupt transition: a high carbon price (the repricing
// shock), still-limited physical damage.
func NGFSDisorderly() ClimateScenario {
	return ClimateScenario{Name: "NGFS_DISORDERLY", CarbonPrice: 150, PhysicalSeverity: 0.02}
}

// NGFSHotHouse is a hot-house world: little transition (low carbon price) but
// severe physical damage.
func NGFSHotHouse() ClimateScenario {
	return ClimateScenario{Name: "NGFS_HOT_HOUSE", CarbonPrice: 20, PhysicalSeverity: 0.10}
}

// NamedScenario looks up a named NGFS scenario, returning it and true, or a zero
// scenario and false — the dispatch seam for an API/CLI.
func NamedScenario(name string) (ClimateScenario, bool) {
	s, ok := scenarioCatalog[name]
	if !ok {
		return ClimateScenario{}, false
	}
	return s(), true
}

// ScenarioNames returns the catalog names in stable order.
func ScenarioNames() []string {
	out := make([]string, 0, len(scenarioCatalog))
	for name := range scenarioCatalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

var scenarioCatalog = map[string]func() ClimateScenario{
	"NGFS_ORDERLY":    NGFSOrderly,
	"NGFS_DISORDERLY": NGFSDisorderly,
	"NGFS_HOT_HOUSE":  NGFSHotHouse,
}

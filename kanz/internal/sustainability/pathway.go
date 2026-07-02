package sustainability

import (
	"errors"
	"fmt"
	"sort"
)

// Calibrated climate pathways (PARITY-03f). CLIMATE-01c shipped single-point
// scenarios with "illustrative magnitudes a quant recalibrates" and a FLAT
// physical severity ("the conservative baseline a per-sector hazard map
// refines"). This file supplies those refinements:
//
//   - Pathway: a per-year carbon-price / physical-severity trajectory (the
//     shape of the published NGFS scenario data), evaluated to a
//     ClimateScenario at any horizon year — so a 2030 stress and a 2050
//     stress of the same narrative differ the way NGFS says they do.
//   - HazardMap: per-sector physical-severity multipliers replacing the flat
//     shock; an unmapped sector keeps multiplier 1 (the old baseline), so the
//     refinement degrades to CLIMATE-01c, never below it.
//   - CarbonBudget: a TCRE carbon-budget implied-temperature model replacing
//     the report.go overshoot heuristic ("a real ITR uses a carbon-budget
//     model" — this is that model).
//
// The default pathway/hazard/budget numbers are REPRESENTATIVE of the public
// NGFS Phase-IV and IPCC AR6 publications, wired as data (not logic) so the
// composition root swaps in the licensed vendor datasets verbatim.

// Pathway is one named scenario trajectory: carbon price (money/tCO2e) and
// physical severity per pillar year, linearly interpolated between pillars
// and clamped outside them.
type Pathway struct {
	Name         string
	Years        []int // ascending pillar years
	CarbonPrices []float64
	Severities   []float64
}

// ErrPathway is returned for an unusable trajectory.
var ErrPathway = errors.New("sustainability: invalid pathway")

// NewPathway validates and builds a trajectory: aligned non-empty series,
// strictly ascending years, non-negative prices and severities.
func NewPathway(name string, years []int, carbonPrices, severities []float64) (Pathway, error) {
	n := len(years)
	if n == 0 || len(carbonPrices) != n || len(severities) != n {
		return Pathway{}, fmt.Errorf("%w: need equal, non-empty years/prices/severities", ErrPathway)
	}
	for i := 0; i < n; i++ {
		if i > 0 && years[i] <= years[i-1] {
			return Pathway{}, fmt.Errorf("%w: years must be strictly ascending", ErrPathway)
		}
		if carbonPrices[i] < 0 || severities[i] < 0 {
			return Pathway{}, fmt.Errorf("%w: negative price/severity at %d", ErrPathway, years[i])
		}
	}
	return Pathway{Name: name, Years: years, CarbonPrices: carbonPrices, Severities: severities}, nil
}

// ScenarioAt evaluates the trajectory at a horizon year, yielding the
// ClimateScenario the existing shock/VaR machinery consumes unchanged.
func (p Pathway) ScenarioAt(year int) ClimateScenario {
	return ClimateScenario{
		Name:             fmt.Sprintf("%s@%d", p.Name, year),
		CarbonPrice:      interpYear(p.Years, p.CarbonPrices, year),
		PhysicalSeverity: interpYear(p.Years, p.Severities, year),
	}
}

func interpYear(years []int, vals []float64, year int) float64 {
	n := len(years)
	if year <= years[0] {
		return vals[0]
	}
	if year >= years[n-1] {
		return vals[n-1]
	}
	i := sort.SearchInts(years, year)
	if years[i] == year {
		return vals[i]
	}
	w := float64(year-years[i-1]) / float64(years[i]-years[i-1])
	return vals[i-1] + w*(vals[i]-vals[i-1])
}

// NGFSPathways returns the trajectory form of the CLIMATE-01c scenario
// catalog — representative of the NGFS Phase-IV magnitudes (USD/tCO2e):
// Net Zero 2050 prices carbon early and rising; Delayed Transition stays low
// through 2030 then spikes (the disorderly repricing); Current Policies never
// prices carbon but accumulates physical damage.
func NGFSPathways() []Pathway {
	orderly, _ := NewPathway("NGFS_ORDERLY",
		[]int{2025, 2030, 2040, 2050},
		[]float64{50, 100, 160, 225},
		[]float64{0.005, 0.01, 0.015, 0.02})
	disorderly, _ := NewPathway("NGFS_DISORDERLY",
		[]int{2025, 2030, 2035, 2050},
		[]float64{10, 20, 140, 210},
		[]float64{0.005, 0.01, 0.02, 0.03})
	hotHouse, _ := NewPathway("NGFS_HOT_HOUSE",
		[]int{2025, 2030, 2040, 2050},
		[]float64{10, 12, 15, 18},
		[]float64{0.01, 0.03, 0.06, 0.12})
	return []Pathway{orderly, disorderly, hotHouse}
}

// HazardMap maps a sector key ("TAXONOMY:CODE") to a physical-severity
// multiplier: >1 more exposed than baseline, <1 less. A missing sector (or an
// unclassified holding) multiplies by 1 — the CLIMATE-01c flat shock.
type HazardMap map[string]float64

// Multiplier returns the sector's hazard multiplier, 1 when unmapped.
func (h HazardMap) Multiplier(sector string) float64 {
	if m, ok := h[sector]; ok && m >= 0 {
		return m
	}
	return 1
}

// DefaultHazardMap is a representative GICS-sector physical-vulnerability
// ranking (energy/utilities/real-estate assets are climate-exposed;
// financial/tech books are not) — replace with the licensed vendor hazard
// dataset at the composition root.
func DefaultHazardMap() HazardMap {
	return HazardMap{
		"GICS:10": 1.8, // Energy
		"GICS:15": 1.5, // Materials
		"GICS:20": 1.2, // Industrials
		"GICS:25": 1.0, // Consumer Discretionary
		"GICS:30": 1.3, // Consumer Staples (agriculture supply chains)
		"GICS:35": 0.8, // Health Care
		"GICS:40": 0.7, // Financials
		"GICS:45": 0.7, // Information Technology
		"GICS:50": 0.8, // Communication Services
		"GICS:55": 1.6, // Utilities
		"GICS:60": 1.7, // Real Estate
	}
}

// SectorPhysicalShock is PhysicalShock refined by the hazard map: the flat
// severity scaled by the sector's multiplier. ≤ 0 (a loss).
func (s ClimateScenario) SectorPhysicalShock(sector string, hm HazardMap) float64 {
	return s.PhysicalShock() * hm.Multiplier(sector)
}

// InstrumentShocksBySector expands the scenario into per-instrument shocks
// with sector-differentiated physical damage — the hazard-map refinement of
// InstrumentShocks. Instruments with no climate effect are omitted.
func (s ClimateScenario) InstrumentShocksBySector(holdings []Holding, hm HazardMap) map[string]float64 {
	out := make(map[string]float64, len(holdings))
	for _, h := range holdings {
		shock := s.TransitionShock(h.Carbon) + s.SectorPhysicalShock(h.Sector, hm)
		if shock != 0 {
			out[h.InstrumentID] = shock
		}
	}
	return out
}

// ClimateVaRBySector is ClimateVaR with the hazard-map physical channel —
// still monotone non-decreasing in carbon price, severity, and every hazard
// multiplier (the CLIMATE-01e property, preserved by construction).
func (s ClimateScenario) ClimateVaRBySector(holdings []Holding, hm HazardMap) float64 {
	var loss float64
	for _, h := range holdings {
		shock := s.TransitionShock(h.Carbon) + s.SectorPhysicalShock(h.Sector, hm)
		loss += abs(h.MarketValue) * abs(shock)
	}
	return loss
}

// CarbonBudget is the TCRE implied-temperature model: warming scales linearly
// with cumulative emissions (the IPCC transient climate response to cumulative
// carbon emissions), so a portfolio's temperature alignment is its projected
// cumulative emissions measured against its share of the remaining global
// carbon budget for the target.
type CarbonBudget struct {
	// RemainingBudget is the global remaining budget for the target (tCO2e).
	RemainingBudget float64
	// TargetDegrees is the warming the budget corresponds to (e.g. 1.5).
	TargetDegrees float64
	// CurrentWarming is realized warming today — the floor no portfolio can
	// imply below (the planet is already there).
	CurrentWarming float64
	// TCRE is warming per tonne of cumulative CO2e (°C/tCO2e).
	TCRE float64
}

// DefaultBudget15 is the representative IPCC AR6 1.5°C budget: ≈250 GtCO2
// remaining, ≈1.3°C realized, TCRE ≈ 0.45°C per 1000 GtCO2.
func DefaultBudget15() CarbonBudget {
	return CarbonBudget{
		RemainingBudget: 250e9,
		TargetDegrees:   1.5,
		CurrentWarming:  1.3,
		TCRE:            0.45e-12,
	}
}

// ErrCarbonBudget is returned for unusable implied-temperature inputs.
var ErrCarbonBudget = errors.New("sustainability: invalid carbon-budget inputs")

// ImpliedTemperature is the portfolio's implied temperature rise: project the
// portfolio's annual financed emissions (tCO2e/yr, flat trajectory) over the
// horizon, compare against its allocated share of the remaining budget, and
// convert the global-equivalent overshoot to warming via TCRE. Within budget
// ⇒ the target itself (never below realized warming).
//
//	implied = TargetDegrees + TCRE · max(0, cumulative − share·budget)/share
//
// budgetShare is the portfolio's allocation of the global budget (e.g. its
// share of global market capitalization), in (0,1].
func (b CarbonBudget) ImpliedTemperature(annualEmissions float64, budgetShare float64, horizonYears int) (float64, error) {
	if b.RemainingBudget <= 0 || b.TCRE <= 0 || b.TargetDegrees <= 0 {
		return 0, fmt.Errorf("%w: budget/TCRE/target must be positive", ErrCarbonBudget)
	}
	if budgetShare <= 0 || budgetShare > 1 {
		return 0, fmt.Errorf("%w: budgetShare %.4g outside (0,1]", ErrCarbonBudget, budgetShare)
	}
	if horizonYears <= 0 || annualEmissions < 0 {
		return 0, fmt.Errorf("%w: need a positive horizon and non-negative emissions", ErrCarbonBudget)
	}
	cumulative := annualEmissions * float64(horizonYears)
	allocated := budgetShare * b.RemainingBudget
	implied := b.TargetDegrees
	if excess := cumulative - allocated; excess > 0 {
		implied += b.TCRE * excess / budgetShare // global-equivalent overshoot
	}
	if implied < b.CurrentWarming {
		implied = b.CurrentWarming
	}
	return implied, nil
}

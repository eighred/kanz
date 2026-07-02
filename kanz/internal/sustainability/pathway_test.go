package sustainability

import (
	"errors"
	"math"
	"testing"
)

func TestPathway_ScenarioAtInterpolatesAndClamps(t *testing.T) {
	p, err := NewPathway("X", []int{2025, 2030, 2050}, []float64{50, 100, 200}, []float64{0.01, 0.02, 0.04})
	if err != nil {
		t.Fatalf("NewPathway: %v", err)
	}
	if got := p.ScenarioAt(2030).CarbonPrice; got != 100 {
		t.Errorf("pillar year: got %v want 100", got)
	}
	if got := p.ScenarioAt(2040).CarbonPrice; math.Abs(got-150) > 1e-12 {
		t.Errorf("midpoint 2040: got %v want 150 (linear)", got)
	}
	if got := p.ScenarioAt(2020).CarbonPrice; got != 50 {
		t.Errorf("pre-pathway clamps to first pillar: got %v", got)
	}
	if got := p.ScenarioAt(2100).PhysicalSeverity; got != 0.04 {
		t.Errorf("post-pathway clamps to last pillar: got %v", got)
	}
}

func TestPathway_Errors(t *testing.T) {
	cases := map[string]struct {
		years  []int
		prices []float64
		sevs   []float64
	}{
		"empty":         {nil, nil, nil},
		"misaligned":    {[]int{2025, 2030}, []float64{50}, []float64{0.01, 0.02}},
		"non-ascending": {[]int{2030, 2025}, []float64{50, 60}, []float64{0.01, 0.02}},
		"negative":      {[]int{2025, 2030}, []float64{-5, 60}, []float64{0.01, 0.02}},
	}
	for name, tc := range cases {
		if _, err := NewPathway("X", tc.years, tc.prices, tc.sevs); !errors.Is(err, ErrPathway) {
			t.Errorf("%s: want ErrPathway, got %v", name, err)
		}
	}
}

// The NGFS catalog trajectories carry their narratives: the orderly price is
// non-decreasing; the disorderly price spikes after 2030 (2035 price is a
// multiple of 2030's); the hot-house physical severity ends highest.
func TestNGFSPathways_Narratives(t *testing.T) {
	byName := map[string]Pathway{}
	for _, p := range NGFSPathways() {
		byName[p.Name] = p
	}
	orderly := byName["NGFS_ORDERLY"]
	for i := 1; i < len(orderly.CarbonPrices); i++ {
		if orderly.CarbonPrices[i] < orderly.CarbonPrices[i-1] {
			t.Error("orderly carbon price must be non-decreasing")
		}
	}
	dis := byName["NGFS_DISORDERLY"]
	if dis.ScenarioAt(2035).CarbonPrice < 5*dis.ScenarioAt(2030).CarbonPrice {
		t.Error("disorderly pathway must spike after the delayed transition")
	}
	hot := byName["NGFS_HOT_HOUSE"]
	if hot.ScenarioAt(2050).PhysicalSeverity <= orderly.ScenarioAt(2050).PhysicalSeverity {
		t.Error("hot-house 2050 physical severity must exceed orderly")
	}
}

// The hazard map differentiates the physical channel by sector, and an
// unclassified holding keeps the CLIMATE-01c flat baseline exactly.
func TestHazardMap_RefinesPhysicalShock(t *testing.T) {
	s := ClimateScenario{Name: "T", CarbonPrice: 0, PhysicalSeverity: 0.02}
	hm := DefaultHazardMap()

	energy := s.SectorPhysicalShock("GICS:10", hm)
	tech := s.SectorPhysicalShock("GICS:45", hm)
	if !(energy < tech && tech < 0) {
		t.Errorf("energy (%v) must lose more than tech (%v), both losses", energy, tech)
	}
	if got := s.SectorPhysicalShock("", hm); got != s.PhysicalShock() {
		t.Errorf("unclassified sector must keep the flat baseline: got %v want %v", got, s.PhysicalShock())
	}

	holdings := []Holding{
		{InstrumentID: "OIL", MarketValue: 1000, Sector: "GICS:10"},
		{InstrumentID: "SW", MarketValue: 1000, Sector: "GICS:45"},
	}
	shocks := s.InstrumentShocksBySector(holdings, hm)
	if !(shocks["OIL"] < shocks["SW"]) {
		t.Errorf("sector-differentiated shocks: OIL %v must be worse than SW %v", shocks["OIL"], shocks["SW"])
	}

	// CLIMATE-01e monotonicity survives the refinement: harsher severity ⇒ ≥ loss.
	harsher := ClimateScenario{PhysicalSeverity: 0.04}
	if harsher.ClimateVaRBySector(holdings, hm) < s.ClimateVaRBySector(holdings, hm) {
		t.Error("ClimateVaRBySector must be monotone in severity")
	}
}

func TestCarbonBudget_ImpliedTemperature(t *testing.T) {
	b := DefaultBudget15()
	const share = 1e-4 // portfolio owns 0.01% of the global budget

	// Within budget ⇒ exactly the target.
	allocated := share * b.RemainingBudget
	within, err := b.ImpliedTemperature(allocated/30/2, share, 30) // half the budget over 30y
	if err != nil {
		t.Fatalf("ImpliedTemperature: %v", err)
	}
	if within != b.TargetDegrees {
		t.Errorf("within budget: got %v want %v", within, b.TargetDegrees)
	}

	// Overshoot ⇒ above target, monotone in emissions.
	over1, _ := b.ImpliedTemperature(allocated/30*2, share, 30)
	over2, _ := b.ImpliedTemperature(allocated/30*4, share, 30)
	if !(over1 > b.TargetDegrees && over2 > over1) {
		t.Errorf("overshoot must warm monotonically: %v, %v (target %v)", over1, over2, b.TargetDegrees)
	}

	// A doubled global-equivalent budget spend lands near TCRE·budget extra
	// warming: spending the WHOLE remaining budget twice over ⇒ +0.45°C·(250/1000).
	full, _ := b.ImpliedTemperature(allocated*2/30, share, 30)
	wantExtra := b.TCRE * b.RemainingBudget // one extra budget's worth
	if math.Abs((full-b.TargetDegrees)-wantExtra) > 1e-9 {
		t.Errorf("TCRE scaling: extra warming %v want %v", full-b.TargetDegrees, wantExtra)
	}

	for name, call := range map[string]func() (float64, error){
		"zero share":    func() (float64, error) { return b.ImpliedTemperature(1000, 0, 30) },
		"share > 1":     func() (float64, error) { return b.ImpliedTemperature(1000, 1.5, 30) },
		"zero horizon":  func() (float64, error) { return b.ImpliedTemperature(1000, share, 0) },
		"negative emis": func() (float64, error) { return b.ImpliedTemperature(-1, share, 30) },
	} {
		if _, err := call(); !errors.Is(err, ErrCarbonBudget) {
			t.Errorf("%s: want ErrCarbonBudget, got %v", name, err)
		}
	}
}

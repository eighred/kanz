package library

import (
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

func TestNamedClimateScenarioDispatch(t *testing.T) {
	shocks, ok := NamedClimateScenario("DISORDERLY_TRANSITION")
	if !ok || len(shocks) == 0 {
		t.Fatal("DISORDERLY_TRANSITION should resolve to a non-empty sector curve")
	}
	// Every shock is a sector shock under the GICS taxonomy.
	for _, s := range shocks {
		ss, isSector := s.(v1.SectorShock)
		if !isSector {
			t.Fatalf("climate scenario should be sector shocks, got %T", s)
		}
		if ss.Taxonomy != gicsTaxonomy {
			t.Fatalf("expected GICS taxonomy, got %q", ss.Taxonomy)
		}
	}
	if _, ok := NamedClimateScenario("NOPE"); ok {
		t.Fatal("unknown climate scenario should not resolve")
	}
}

func TestDisorderlyTransitionHitsEnergyHardest(t *testing.T) {
	shocks := DisorderlyTransition()
	byCode := map[string]int64{}
	for _, s := range shocks {
		ss := s.(v1.SectorShock)
		byCode[ss.Code] = ss.Pct.GetCoefficient()
	}
	// Energy (carbon-intensive) is shocked harder (more negative) than discretionary.
	if byCode[gicsEnergy] >= byCode[gicsConsumerDiscretionary] {
		t.Fatalf("energy should be hit harder than discretionary: energy=%d disc=%d",
			byCode[gicsEnergy], byCode[gicsConsumerDiscretionary])
	}
}

func TestClimateScenarioNames(t *testing.T) {
	names := ClimateScenarioNames()
	if len(names) != 2 {
		t.Fatalf("want 2 climate scenarios, got %v", names)
	}
}

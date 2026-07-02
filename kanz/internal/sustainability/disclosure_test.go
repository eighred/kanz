package sustainability

import (
	"testing"
	"time"
)

func discInputs() DisclosureInputs {
	return DisclosureInputs{
		Holdings: []Holding{
			{
				InstrumentID: "OILCO",
				MarketValue:  1_000_000,
				Carbon:       CarbonMetrics{Scope1: 40_000, Scope2: 10_000, Scope3: 200_000, Revenue: 500, EVIC: 2_000_000},
			},
			{
				InstrumentID: "TECHCO",
				MarketValue:  1_000_000,
				Carbon:       CarbonMetrics{Scope1: 1_000, Scope2: 500, Scope3: 5_000, Revenue: 800, EVIC: 4_000_000},
			},
		},
		GlidePath:       GlidePath{BaseYear: 2020, TargetYear: 2050, BaseEmissions: 100, ReductionFloor: 0.1},
		Year:            2030,
		Scenario:        NGFSDisorderly(),
		TempSensitivity: 2.0,
		FossilFuel:      map[string]bool{"OILCO": true},
	}
}

// The TCFD disclosure computes every line item from the live holdings + pathway,
// reproduces the standalone metric functions, and is signed + complete.
func TestFileTCFD_LiveMetricsSignedComplete(t *testing.T) {
	in := discInputs()
	asOf := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	rep, err := FileTCFD(in, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Signature == "" || len(rep.LineItems) != 4 {
		t.Fatalf("TCFD disclosure malformed: sig=%q items=%d", rep.Signature, len(rep.LineItems))
	}
	waci, _ := rep.Lookup("TCFD_WACI")
	if waci != WeightedAverageCarbonIntensity(in.Holdings) {
		t.Fatalf("filed WACI %.4f != computed", waci)
	}
	fe, _ := rep.Lookup("TCFD_FINANCED_EMISSIONS")
	if fe != FinancedEmissions(in.Holdings) {
		t.Fatalf("filed financed emissions %.4f != computed", fe)
	}
	// The book overshoots its 2030 glide-path target, so implied rise > 1.5°C.
	itr, _ := rep.Lookup("TCFD_IMPLIED_TEMP_RISE")
	if itr <= 1.5 {
		t.Fatalf("implied temp rise %.3f should exceed 1.5°C for an over-budget book", itr)
	}
}

// The SFDR disclosure derives carbon footprint (per $m) and the fossil-fuel
// share, and is signed + complete.
func TestFileSFDR_DerivesFootprintAndFossilShare(t *testing.T) {
	in := discInputs()
	asOf := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	rep, err := FileSFDR(in, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.LineItems) != 3 {
		t.Fatalf("SFDR disclosure should have 3 line items, got %d", len(rep.LineItems))
	}
	// One of two equal-value holdings is fossil-flagged ⇒ 50% exposure.
	fossil, _ := rep.Lookup("SFDR_FOSSIL_FUEL_EXPOSURE")
	if fossil != 0.5 {
		t.Fatalf("fossil-fuel exposure = %.3f want 0.5", fossil)
	}
	// Footprint is financed emissions normalized per $m invested — positive here.
	fp, _ := rep.Lookup("SFDR_CARBON_FOOTPRINT")
	if fp <= 0 {
		t.Fatalf("carbon footprint = %.4f, want > 0", fp)
	}
}

package sustainability

import "testing"

func TestTransitionShockSignAndMagnitude(t *testing.T) {
	// carbonPrice 100, scope1+2 = 200, EVIC = 1000 ⇒ shock = -(100*200)/1000 = -20.
	s := ClimateScenario{CarbonPrice: 100}
	c := CarbonMetrics{Scope1: 100, Scope2: 100, EVIC: 1000}
	if got := s.TransitionShock(c); !approx(got, -20, 1e-9) {
		t.Fatalf("transition shock: want -20 got %v", got)
	}
	// No EVIC ⇒ no shock (undefined attribution).
	if got := s.TransitionShock(CarbonMetrics{Scope1: 100}); got != 0 {
		t.Fatalf("no-EVIC transition shock should be 0, got %v", got)
	}
}

func TestClimateVaRNonNegativeAndMonotone(t *testing.T) {
	h := sampleHoldings()
	mild := ClimateScenario{CarbonPrice: 50, PhysicalSeverity: 0.01}
	harsh := ClimateScenario{CarbonPrice: 150, PhysicalSeverity: 0.05}

	lossMild := mild.ClimateVaR(h)
	lossHarsh := harsh.ClimateVaR(h)
	if lossMild < 0 || lossHarsh < 0 {
		t.Fatal("climate-VaR is a loss magnitude, must be >= 0")
	}
	// Monotone: a harsher scenario (higher carbon price AND physical severity)
	// never lowers the loss.
	if lossHarsh < lossMild {
		t.Fatalf("harsher scenario should not lower loss: mild=%v harsh=%v", lossMild, lossHarsh)
	}
	// Monotone in carbon price alone.
	hi := ClimateScenario{CarbonPrice: 200, PhysicalSeverity: 0.01}
	if hi.ClimateVaR(h) <= mild.ClimateVaR(h) {
		t.Fatal("raising the carbon price should raise the loss")
	}
	// Monotone in physical severity alone.
	phys := ClimateScenario{CarbonPrice: 50, PhysicalSeverity: 0.10}
	if phys.ClimateVaR(h) <= mild.ClimateVaR(h) {
		t.Fatal("raising physical severity should raise the loss")
	}
}

func TestInstrumentShocksOmitsZero(t *testing.T) {
	s := ClimateScenario{CarbonPrice: 100, PhysicalSeverity: 0.02}
	holdings := []Holding{
		{InstrumentID: "XOM", MarketValue: 100, Carbon: CarbonMetrics{Scope1: 100, Scope2: 100, EVIC: 1000}},
		{InstrumentID: "CASH", MarketValue: 100}, // no carbon, no EVIC ⇒ only physical shock
	}
	shocks := s.InstrumentShocks(holdings)
	if shocks["XOM"] >= 0 {
		t.Fatalf("XOM should have a negative climate shock, got %v", shocks["XOM"])
	}
	// CASH still gets the flat physical shock (-0.02).
	if !approx(shocks["CASH"], -0.02, 1e-9) {
		t.Fatalf("CASH physical shock: want -0.02 got %v", shocks["CASH"])
	}
}

func TestNGFSCatalog(t *testing.T) {
	if names := ScenarioNames(); len(names) != 3 {
		t.Fatalf("want 3 NGFS scenarios, got %v", names)
	}
	dis, ok := NamedScenario("NGFS_DISORDERLY")
	if !ok || dis.CarbonPrice <= NGFSOrderly().CarbonPrice {
		t.Fatalf("disorderly should carry a higher carbon price than orderly")
	}
	if _, ok := NamedScenario("NOPE"); ok {
		t.Fatal("unknown scenario should not resolve")
	}
	// Hot-house: low transition price but the highest physical severity.
	if NGFSHotHouse().PhysicalSeverity <= NGFSDisorderly().PhysicalSeverity {
		t.Fatal("hot-house should carry the highest physical severity")
	}
}

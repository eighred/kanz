package sustainability

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// Two holdings with known intensities; WACI is the MV-weighted average and must
// reconcile exactly with the per-holding contributions.
func sampleHoldings() []Holding {
	return []Holding{
		{InstrumentID: "AAPL", MarketValue: 300, // weight 0.6
			Carbon: CarbonMetrics{Scope1: 10, Scope2: 10, Scope3: 30, Revenue: 100, EVIC: 1000},
			ESG:    ESGScore{Overall: 80, Environmental: 75, Social: 82, Governance: 85}},
		{InstrumentID: "XOM", MarketValue: 200, // weight 0.4
			Carbon: CarbonMetrics{Scope1: 100, Scope2: 100, Scope3: 300, Revenue: 100, EVIC: 1000},
			ESG:    ESGScore{Overall: 40, Environmental: 30, Social: 45, Governance: 50}},
	}
}

func TestWACIReconciliation(t *testing.T) {
	h := sampleHoldings()
	// intensity AAPL = (10+10)/100 = 0.2; XOM = (100+100)/100 = 2.0.
	// WACI = 0.6*0.2 + 0.4*2.0 = 0.12 + 0.8 = 0.92.
	waci := WeightedAverageCarbonIntensity(h)
	if !approx(waci, 0.92, 1e-9) {
		t.Fatalf("WACI: want 0.92 got %v", waci)
	}
	// The per-holding contributions must sum to WACI.
	var sum float64
	for _, c := range CarbonContributions(h) {
		sum += c.Contribution
	}
	if !approx(sum, waci, 1e-9) {
		t.Fatalf("contributions %v != WACI %v", sum, waci)
	}
	// Sorted: XOM (the bigger contributor) first.
	if CarbonContributions(h)[0].InstrumentID != "XOM" {
		t.Fatal("largest carbon contributor should sort first")
	}
}

func TestFinancedEmissionsIdentity(t *testing.T) {
	h := sampleHoldings()
	// PCAF: attribution = MV/EVIC; financed = Σ attribution * totalScopes.
	// AAPL: 300/1000 * (10+10+30=50) = 0.3*50 = 15.
	// XOM:  200/1000 * (100+100+300=500) = 0.2*500 = 100.
	// total = 115.
	if got := FinancedEmissions(h); !approx(got, 115, 1e-9) {
		t.Fatalf("financed emissions: want 115 got %v", got)
	}
}

func TestFinancedEmissionsSkipsNoEVIC(t *testing.T) {
	h := []Holding{{InstrumentID: "X", MarketValue: 100, Carbon: CarbonMetrics{Scope1: 50}}} // EVIC 0
	if got := FinancedEmissions(h); got != 0 {
		t.Fatalf("no-EVIC holding should contribute 0, got %v", got)
	}
}

func TestWeightedAverageESG(t *testing.T) {
	h := sampleHoldings()
	// overall = 0.6*80 + 0.4*40 = 48 + 16 = 64.
	if got := WeightedAverageESG(h).Overall; !approx(got, 64, 1e-9) {
		t.Fatalf("weighted ESG overall: want 64 got %v", got)
	}
}

func TestEmptyBookDegradesToZero(t *testing.T) {
	if WeightedAverageCarbonIntensity(nil) != 0 || FinancedEmissions(nil) != 0 {
		t.Fatal("empty book should give zero, not NaN/Inf")
	}
	if WeightedAverageESG(nil).Overall != 0 {
		t.Fatal("empty book ESG should be zero")
	}
}

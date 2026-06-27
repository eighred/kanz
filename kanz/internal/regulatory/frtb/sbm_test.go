package frtb

import (
	"math"
	"testing"
)

// TestSBM_WorkedExample reconciles the FRTB sensitivities-based method against a
// hand-derived single-bucket computation with two OFFSETTING factors:
//
//	WS = [0.02·1000, 0.02·(−500)] = [20, −10]
//	Medium (ρ=0.5):  K = √(500 − 200) = √300 = 17.3205
//	High   (ρ=0.625): K = √(500 − 250) = √250 = 15.8114
//	Low    (ρ=0.375): K = √(500 − 150) = √350 = 18.7083
//	Charge = max = √350  (the LOW scenario — least netting of the offset)
func TestSBM_WorkedExample(t *testing.T) {
	sens := []Sensitivity{
		{RiskClass: "Equity", Bucket: "A", Factor: "X", Amount: 1000},
		{RiskClass: "Equity", Bucket: "A", Factor: "Y", Amount: -500},
	}
	p := ClassParams{RiskWeight: map[string]float64{"A": 0.02}, IntraCorr: 0.5, InterCorr: 0.5}

	cases := []struct {
		s    Scenario
		want float64
	}{
		{Medium, math.Sqrt(300)},
		{High, math.Sqrt(250)},
		{Low, math.Sqrt(350)},
	}
	for _, c := range cases {
		if got := ChargeForScenario(sens, p, c.s); math.Abs(got-c.want) > 1e-6 {
			t.Fatalf("scenario %d: got %.6f want %.6f", c.s, got, c.want)
		}
	}
	// The aggregate charge is the worst scenario — the LOW correlation here,
	// because the two sensitivities offset and low correlation un-nets them.
	if got := Charge(sens, Params{"Equity": p}); math.Abs(got-math.Sqrt(350)) > 1e-6 {
		t.Fatalf("FRTB charge: got %.6f want %.6f", got, math.Sqrt(350))
	}
}

// TestSBM_SumsAcrossRiskClasses: the charge sums the per-class capital.
func TestSBM_SumsAcrossRiskClasses(t *testing.T) {
	sens := []Sensitivity{
		{RiskClass: "Equity", Bucket: "A", Factor: "X", Amount: 1000},
		{RiskClass: "FX", Bucket: "USD", Factor: "EURUSD", Amount: 2000},
	}
	params := Params{
		"Equity": {RiskWeight: map[string]float64{"A": 0.02}, IntraCorr: 0.5, InterCorr: 0.5},
		"FX":     {RiskWeight: map[string]float64{"USD": 0.15}, IntraCorr: 0.6, InterCorr: 0.6},
	}
	eq := Charge([]Sensitivity{sens[0]}, params)
	fx := Charge([]Sensitivity{sens[1]}, params)
	if got := Charge(sens, params); math.Abs(got-(eq+fx)) > 1e-6 {
		t.Fatalf("charge must sum across risk classes: %.4f != %.4f+%.4f", got, eq, fx)
	}
}

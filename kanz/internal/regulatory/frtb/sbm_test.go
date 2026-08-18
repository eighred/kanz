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

// TestSBM_NegativeRadicandUsesTheAlternativeSb pins MAR21.4(5) (#471).
//
// A NEGATIVE CROSS-BUCKET RADICAND USED TO RETURN ZERO CAPITAL. It is produced by
// buckets that OFFSET each other, which is the ordinary shape of a trading book,
// and the substitution reported NO CAPITAL REQUIRED for a book with real gross
// risk — smaller filing, no error, nothing for an operator to notice. MAR21.4(5)
// prescribes recomputing with Sb = max[min(ΣWS, Kb), −Kb] instead.
//
// Two buckets of (+5,+5) and (−5,−5) at ρ=0: every Kb = √50 and every |Sb| = 10,
// so at γ=0.8 the radicand is 100 − 0.8·200 = −60. With Sb capped to ±√50 it is
// 100 − 0.8·100 = 20, and the charge √20. The reachability condition is γ > ρ,
// which DefaultParams never satisfies and a loaded MAR21 table may.
func TestSBM_NegativeRadicandUsesTheAlternativeSb(t *testing.T) {
	sens := []Sensitivity{
		{RiskClass: "Equity", Bucket: "1", Factor: "X", Amount: 5},
		{RiskClass: "Equity", Bucket: "1", Factor: "Y", Amount: 5},
		{RiskClass: "Equity", Bucket: "2", Factor: "X", Amount: -5},
		{RiskClass: "Equity", Bucket: "2", Factor: "Y", Amount: -5},
	}
	p := ClassParams{
		RiskWeight: map[string]float64{"1": 1, "2": 1},
		IntraCorr:  0, InterCorr: 0.8,
	}

	if got := ChargeForScenario(sens, p, Medium); math.Abs(got-math.Sqrt(20)) > 1e-9 {
		t.Fatalf("medium scenario: got %.9f, want %.9f — a negative radicand must be recomputed "+
			"with Sb capped to ±Kb (MAR21.4(5)), not collapsed to a zero capital charge",
			got, math.Sqrt(20))
	}
	// The LOW scenario scales γ to max(2·0.8−1, 0.75·0.8) = 0.6, so the alternative
	// radicand is 100 − 0.6·100 = 40 and this is the worst of the three.
	if got := Charge(sens, Params{"Equity": p}); math.Abs(got-math.Sqrt(40)) > 1e-9 {
		t.Fatalf("charge: got %.9f, want %.9f", got, math.Sqrt(40))
	}
	// AND IT IS NOT ZERO, stated separately because that is the exact failure the
	// repair removed and an arithmetic slip in the expectations above could
	// reintroduce it while still comparing two matching numbers.
	if Charge(sens, Params{"Equity": p}) <= 0 {
		t.Fatal("a book of four sensitivities of magnitude 5 required no capital at all")
	}
}

// TestSBM_TheAlternativeSbDoesNotDisturbAPositiveRadicand: the repair is
// conditional, exactly as MAR21.4(5) is. Clamping Sb unconditionally would shrink
// every ordinary offsetting book's charge, which is the same understatement
// arriving by the opposite route.
func TestSBM_TheAlternativeSbDoesNotDisturbAPositiveRadicand(t *testing.T) {
	sens := []Sensitivity{
		{RiskClass: "Equity", Bucket: "1", Factor: "X", Amount: 5},
		{RiskClass: "Equity", Bucket: "1", Factor: "Y", Amount: 5},
		{RiskClass: "Equity", Bucket: "2", Factor: "X", Amount: -5},
		{RiskClass: "Equity", Bucket: "2", Factor: "Y", Amount: -5},
	}
	// γ = ρ = 0.5 keeps the radicand non-negative: Kb² = 25+25·... hand-computed
	// below rather than asserted as an inequality.
	p := ClassParams{RiskWeight: map[string]float64{"1": 1, "2": 1}, IntraCorr: 0.5, InterCorr: 0.5}
	// Kb² = 25+25+0.5·2·25 = 75; Sb = ±10; radicand = 150 + 0.5·(0 − 200) = 50.
	if got := ChargeForScenario(sens, p, Medium); math.Abs(got-math.Sqrt(50)) > 1e-9 {
		t.Fatalf("got %.9f, want %.9f — the uncapped Sb must be used whenever the radicand is "+
			"non-negative", got, math.Sqrt(50))
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

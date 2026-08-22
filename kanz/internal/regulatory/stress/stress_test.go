package stress

import (
	"math"
	"testing"
)

func TestExpand_MacroToAssetClass(t *testing.T) {
	model := ExpansionModel{Beta: map[string]map[string]float64{
		"Equity": {"EQUITY": 1.0, "GDP": 2.0},
		"Credit": {"CREDIT_SPREAD": -1.0},
	}}
	sc := MacroScenario{
		Name:   "ADVERSE",
		Shocks: map[string]float64{"EQUITY": -0.20, "GDP": -0.03, "CREDIT_SPREAD": 0.02},
	}
	out, _ := model.Expand(sc)
	// Equity = 1.0·(−0.20) + 2.0·(−0.03) = −0.26.
	if math.Abs(out["Equity"]-(-0.26)) > 1e-9 {
		t.Fatalf("Equity shock: got %.4f want -0.26", out["Equity"])
	}
	// Credit = −1.0·0.02 = −0.02 (spread widening hurts).
	if math.Abs(out["Credit"]-(-0.02)) > 1e-9 {
		t.Fatalf("Credit shock: got %.4f want -0.02", out["Credit"])
	}
	// Severity scales the transmission linearly.
	sc.Severity = 2
	scaled, _ := model.Expand(sc)
	if math.Abs(scaled["Equity"]-(-0.52)) > 1e-9 {
		t.Fatalf("severity 2 should double the shock to -0.52, got %.4f", scaled["Equity"])
	}
}

func TestReverseStress_FindsBreach(t *testing.T) {
	// Loss grows linearly with severity; breach at loss = 5,000,000.
	loss := func(sev float64) float64 { return sev * 1_000_000 }
	sev, ok := ReverseStress(loss, 5_000_000, 10)
	if !ok || math.Abs(sev-5) > 1e-6 {
		t.Fatalf("reverse stress should breach at severity 5, got %.6f ok=%v", sev, ok)
	}
}

func TestReverseStress_NoBreachInRange(t *testing.T) {
	loss := func(sev float64) float64 { return sev * 1_000_000 }
	if _, ok := ReverseStress(loss, 50_000_000, 10); ok {
		t.Fatal("a threshold beyond the max severity must not breach")
	}
}

func TestReverseStress_AlreadyBreached(t *testing.T) {
	loss := func(float64) float64 { return 6_000_000 }
	sev, ok := ReverseStress(loss, 5_000_000, 10)
	if !ok || sev != 0 {
		t.Fatalf("an already-breached book breaches at severity 0, got %.4f ok=%v", sev, ok)
	}
}

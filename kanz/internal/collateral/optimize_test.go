package collateral

import (
	"math"
	"testing"
)

func eligible(h float64) Eligibility { return Eligibility{Eligible: true, Haircut: h} }
func ineligible() Eligibility        { return Eligibility{Eligible: false} }

func TestOptimize_CheapestToDeliver(t *testing.T) {
	assets := []Asset{
		{ID: "CASH", Available: 1000, Cost: 0.01},
		{ID: "BOND", Available: 1000, Cost: 0.05},
	}
	reqs := []Requirement{{
		AgreementID: "AG1", Amount: 500,
		Schedule: map[string]Eligibility{"CASH": eligible(0), "BOND": eligible(0)},
	}}
	alloc, ok := Optimize(assets, reqs)
	if !ok {
		t.Fatal("requirement should be satisfiable")
	}
	// The cheap asset covers the whole requirement; the expensive one is untouched.
	if len(alloc) != 1 || alloc[0].AssetID != "CASH" || alloc[0].PostedValue != 500 {
		t.Fatalf("expected 500 of CASH, got %+v", alloc)
	}
	if c := TotalCost(alloc); math.Abs(c-5) > 1e-9 { // 500 × 0.01
		t.Fatalf("cost: got %.4f want 5", c)
	}
}

func TestOptimize_RespectsEligibilityAndHaircut(t *testing.T) {
	assets := []Asset{
		{ID: "CASH", Available: 1000, Cost: 0.01},
		{ID: "BOND", Available: 1000, Cost: 0.05},
	}
	// CASH ineligible here ⇒ must deliver BOND at a 20% haircut.
	reqs := []Requirement{{
		AgreementID: "AG1", Amount: 400,
		Schedule: map[string]Eligibility{"CASH": ineligible(), "BOND": eligible(0.20)},
	}}
	alloc, ok := Optimize(assets, reqs)
	if !ok || len(alloc) != 1 || alloc[0].AssetID != "BOND" {
		t.Fatalf("must fall back to the eligible BOND, got ok=%v %+v", ok, alloc)
	}
	// To deliver 400 post-value at a 20% haircut needs 400/0.8 = 500 market value.
	if math.Abs(alloc[0].UsedValue-500) > 1e-9 || math.Abs(alloc[0].PostedValue-400) > 1e-9 {
		t.Fatalf("haircut grossing-up wrong: used=%.4f posted=%.4f", alloc[0].UsedValue, alloc[0].PostedValue)
	}
}

func TestOptimize_InsufficientPool(t *testing.T) {
	assets := []Asset{{ID: "CASH", Available: 100, Cost: 0.01}}
	reqs := []Requirement{{
		AgreementID: "AG1", Amount: 500,
		Schedule: map[string]Eligibility{"CASH": eligible(0)},
	}}
	_, ok := Optimize(assets, reqs)
	if ok {
		t.Fatal("an underfunded pool must report ok=false")
	}
}

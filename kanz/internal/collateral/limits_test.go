package collateral

import (
	"github.com/eighred/kanz/internal/dec"
	"testing"
)

func TestConcentrationLimitChangesGlobalOptimum(t *testing.T) {
	a := []Asset{{ID: "A", Currency: "USD", Available: "100", Cost: "1"}, {ID: "B", Currency: "USD", Available: "100", Cost: "2"}}
	r := []Requirement{{AgreementID: "X", Currency: "USD", Amount: "100", Schedule: map[string]Eligibility{"A": {Eligible: true, Haircut: "0"}, "B": {Eligible: true, Haircut: "0"}}}}
	l := []AllocationLimit{{ID: "issuer-A", Maximum: "25", Terms: []AllocationLimitTerm{{AssetID: "A", AgreementID: "X", Weight: "1"}}}}
	got, err := OptimizeConstrained(t.Context(), a, r, l)
	if err != nil || !got.Feasible || got.Cost != "175" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := VerifyConstrainedAllocation(a, r, l, got); err != nil {
		t.Fatal(err)
	}
	got.Certificate.Limits["issuer-A"] = dec.Exact("0")
	if VerifyConstrainedAllocation(a, r, l, got) == nil {
		t.Fatal("forged concentration dual accepted")
	}
	l = append(l, AllocationLimit{ID: "liquidity-B", Maximum: "50", Terms: []AllocationLimitTerm{{AssetID: "B", AgreementID: "X", Weight: "1"}}})
	got, err = OptimizeConstrained(t.Context(), a, r, l)
	if err != nil || got.Feasible {
		t.Fatalf("expected certified infeasibility: %+v %v", got, err)
	}
	if err := VerifyConstrainedAllocation(a, r, l, got); err != nil {
		t.Fatal(err)
	}
}

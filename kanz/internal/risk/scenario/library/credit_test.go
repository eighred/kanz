package library

import "testing"

func TestNamedCreditStress(t *testing.T) {
	s, ok := NamedCreditStress("CREDIT_SPREAD_WIDENING")
	if !ok || s.HazardMult <= 1 {
		t.Fatalf("CREDIT_SPREAD_WIDENING must widen hazards, got %+v ok=%v", s, ok)
	}
	d, ok := NamedCreditStress("COUNTERPARTY_DEFAULT")
	if !ok || !d.JumpToDefault {
		t.Fatalf("COUNTERPARTY_DEFAULT must be a jump-to-default, got %+v", d)
	}
	if _, ok := NamedCreditStress("NOPE"); ok {
		t.Fatal("unknown stress must not resolve")
	}
	names := CreditStressNames()
	if len(names) != 2 || names[0] != "COUNTERPARTY_DEFAULT" || names[1] != "CREDIT_SPREAD_WIDENING" {
		t.Fatalf("names not in stable order: %v", names)
	}
}

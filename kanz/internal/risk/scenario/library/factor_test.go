package library

import "testing"

func TestNamedFactorShock(t *testing.T) {
	s, ok := NamedFactorShock("MOMENTUM_CRASH")
	if !ok {
		t.Fatal("MOMENTUM_CRASH should resolve")
	}
	if s["Momentum"] >= 0 {
		t.Fatalf("a momentum crash must shock momentum negative, got %v", s["Momentum"])
	}
	if _, ok := NamedFactorShock("NOPE"); ok {
		t.Fatal("unknown shock must not resolve")
	}
	names := FactorShockNames()
	if len(names) != 2 || names[0] != "FLIGHT_TO_QUALITY" || names[1] != "MOMENTUM_CRASH" {
		t.Fatalf("names not in stable order: %v", names)
	}
}

package library

import "testing"

func TestNamedLiquidityStress(t *testing.T) {
	s, ok := NamedLiquidityStress("LIQUIDITY_STRESS")
	if !ok {
		t.Fatal("LIQUIDITY_STRESS should resolve")
	}
	if s.SpreadMult <= 1 || s.ADVMult >= 1 {
		t.Fatalf("a liquidity stress must widen spreads and dry up ADV: %+v", s)
	}
	if _, ok := NamedLiquidityStress("NOPE"); ok {
		t.Fatal("unknown stress must not resolve")
	}
	names := LiquidityStressNames()
	if len(names) != 2 || names[0] != "LIQUIDITY_CRISIS" || names[1] != "LIQUIDITY_STRESS" {
		t.Fatalf("names not in stable order: %v", names)
	}
}

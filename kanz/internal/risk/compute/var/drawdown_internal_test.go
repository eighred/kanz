package varmodel

import (
	"math"
	"testing"
)

// Single-peak path: v0=1000, P&L {-100,-50,+200} ⇒ path 1000→900→850→1050.
// Trough 850 vs peak 1000: amount=150, fraction=0.15.
func TestMaxDrawdown_SinglePeak(t *testing.T) {
	frac, amount := maxDrawdown([]float64{-100, -50, 200}, 1000)
	if math.Abs(amount-150) > 1e-9 {
		t.Fatalf("amount = %v, want 150", amount)
	}
	if math.Abs(frac-0.15) > 1e-9 {
		t.Fatalf("fraction = %v, want 0.15", frac)
	}
}

// Monotonic rise ⇒ no drawdown.
func TestMaxDrawdown_NoDrawdown(t *testing.T) {
	frac, amount := maxDrawdown([]float64{10, 20, 5}, 1000)
	if amount != 0 || frac != 0 {
		t.Fatalf("rising path ⇒ 0/0, got fraction=%v amount=%v", frac, amount)
	}
}

// Two peaks: v0=100, P&L {-20,+120,-30} ⇒ path 100→80→200→170. The worst FRACTION
// (0.20, from peak 100 to 80) and the worst AMOUNT (30, from peak 200 to 170)
// fall at DIFFERENT peaks — pins the independent-maxima design.
func TestMaxDrawdown_IndependentMaxima(t *testing.T) {
	frac, amount := maxDrawdown([]float64{-20, 120, -30}, 100)
	if math.Abs(frac-0.20) > 1e-9 {
		t.Fatalf("fraction = %v, want 0.20 (from the low peak)", frac)
	}
	if math.Abs(amount-30) > 1e-9 {
		t.Fatalf("amount = %v, want 30 (from the high peak)", amount)
	}
}

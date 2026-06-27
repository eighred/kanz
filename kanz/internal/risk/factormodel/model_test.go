package factormodel

import (
	"math"
	"testing"
)

// TestShockReturn: a factor shock reprices an instrument by Σ loadingₖ·shockₖ
// over the named factors; an unknown instrument reports ok=false.
func TestShockReturn(t *testing.T) {
	m := newModel(
		[]Factor{{Name: "Value", Type: FactorStyle}, {Name: "Momentum", Type: FactorStyle}},
		[]string{"A"},
		[][]float64{{2, -1}}, // A loads +2 on Value, −1 on Momentum
		[][]float64{{0.04, 0}, {0, 0.04}},
		map[string]float64{"A": 0.01},
	)
	// shock Value +5%, Momentum −10% ⇒ 2·0.05 + (−1)·(−0.10) = 0.20.
	r, ok := m.ShockReturn("A", map[string]float64{"Value": 0.05, "Momentum": -0.10})
	if !ok || math.Abs(r-0.20) > 1e-12 {
		t.Fatalf("shock return: got %.6f ok=%v want 0.20", r, ok)
	}
	if _, ok := m.ShockReturn("UNKNOWN", map[string]float64{"Value": 0.05}); ok {
		t.Fatal("unknown instrument must report ok=false")
	}
}

// TestVaR: parametric VaR is z₀.₉₉ · total risk.
func TestVaR(t *testing.T) {
	m := oneFactorModel()
	values := map[string]float64{"A": 100, "B": -50}
	total := m.Risk(values).Total
	v := m.VaR(values, 0.99)
	if math.Abs(v/total-2.3263) > 1e-3 {
		t.Fatalf("VaR/total = %.5f, want z₀.₉₉ ≈ 2.3263", v/total)
	}
}

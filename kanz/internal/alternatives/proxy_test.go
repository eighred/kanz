package alternatives

import (
	"math"
	"testing"
)

func TestProxyFactorExposure(t *testing.T) {
	// A levered small-cap-like buyout proxy: equity beta 1.3, size 0.4.
	m := ProxyMapping{Name: "buyout", Betas: map[string]float64{"equity": 1.3, "size": 0.4}}
	exp := m.FactorExposure(1_000_000)
	if math.Abs(exp["equity"]-1_300_000) > 1e-6 {
		t.Fatalf("equity exposure: want 1.3e6 got %v", exp["equity"])
	}
	if math.Abs(exp["size"]-400_000) > 1e-6 {
		t.Fatalf("size exposure: want 4e5 got %v", exp["size"])
	}
}

func TestProxyZeroNAV(t *testing.T) {
	m := ProxyMapping{Betas: map[string]float64{"equity": 1.0}}
	if len(m.FactorExposure(0)) != 0 {
		t.Fatal("zero NAV should give empty exposure")
	}
}

func TestAggregateExposure(t *testing.T) {
	pe := ProxyMapping{Betas: map[string]float64{"equity": 1.2}}.FactorExposure(1000)
	re := ProxyMapping{Betas: map[string]float64{"equity": 0.5, "rates": -0.3}}.FactorExposure(1000)
	total := AggregateExposure(pe, re)
	// equity = 1200 + 500 = 1700; rates = -300.
	if math.Abs(total["equity"]-1700) > 1e-6 {
		t.Fatalf("aggregate equity: want 1700 got %v", total["equity"])
	}
	if math.Abs(total["rates"]-(-300)) > 1e-6 {
		t.Fatalf("aggregate rates: want -300 got %v", total["rates"])
	}
}

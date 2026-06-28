package factormodel

import (
	"math"
	"testing"
)

// blendModel is a tiny 2-factor (equity, rates), 1-instrument model: AAPL loads 1
// on equity, 0 on rates; equity variance 0.04 (20% vol), rates 0.01; no specific
// risk. Built via newModel so the test exercises the assembled-model path.
func blendModel() *Model {
	return newModel(
		[]Factor{{Name: "equity", Type: FactorStyle}, {Name: "rates", Type: FactorMacro}},
		[]string{"AAPL"},
		[][]float64{{1.0, 0.0}},
		[][]float64{{0.04, 0.0}, {0.0, 0.01}},
		map[string]float64{"AAPL": 0.0},
	)
}

func TestBlendedFactorExposureAddsProxy(t *testing.T) {
	m := blendModel()
	values := map[string]float64{"AAPL": 1000}
	// Proxy adds 500 of equity exposure plus an unpriced "private" factor.
	proxy := map[string]float64{"equity": 500, "illiquidity": 200}
	got := m.BlendedFactorExposure(values, proxy)
	if math.Abs(got["equity"]-1500) > 1e-9 {
		t.Fatalf("blended equity exposure: want 1500 got %v", got["equity"])
	}
	if math.Abs(got["rates"]) > 1e-9 {
		t.Fatalf("rates exposure should be 0, got %v", got["rates"])
	}
	// An exposure to a factor the model does not price is KEPT (coverage), not dropped.
	if math.Abs(got["illiquidity"]-200) > 1e-9 {
		t.Fatalf("unpriced proxy factor should be kept: %v", got["illiquidity"])
	}
}

func TestBlendedRiskIncludesProxy(t *testing.T) {
	m := blendModel()
	values := map[string]float64{"AAPL": 1000}

	// Liquid-only: e=[1000,0], systematic = √(1000²·0.04) = 200.
	liquid := m.Risk(values)
	if math.Abs(liquid.Total-200) > 1e-6 {
		t.Fatalf("liquid-only risk: want 200 got %v", liquid.Total)
	}

	// Blend a PE proxy mapped to equity beta 1.0 on a 500 NAV ⇒ e=[1500,0],
	// systematic = √(1500²·0.04) = 300.
	proxies := map[string]ProxyPosition{"PE": {Betas: map[string]float64{"equity": 1.0}, NAV: 500}}
	blended := m.BlendedRisk(values, proxies)
	if math.Abs(blended.Total-300) > 1e-6 {
		t.Fatalf("blended risk: want 300 got %v", blended.Total)
	}
	if blended.Total <= liquid.Total {
		t.Fatal("blending a correlated illiquid position should raise total risk")
	}
}

func TestBlendedRiskIgnoresUnpricedProxyFactor(t *testing.T) {
	m := blendModel()
	values := map[string]float64{"AAPL": 1000}
	// A proxy loading only on a factor the model lacks adds exposure but no
	// modelled variance ⇒ total risk equals the liquid-only risk.
	proxies := map[string]ProxyPosition{"X": {Betas: map[string]float64{"illiquidity": 1.0}, NAV: 500}}
	if got := m.BlendedRisk(values, proxies).Total; math.Abs(got-200) > 1e-6 {
		t.Fatalf("unpriced proxy factor should not change risk: want 200 got %v", got)
	}
}

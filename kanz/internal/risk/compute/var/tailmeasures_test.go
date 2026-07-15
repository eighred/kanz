package varmodel_test

import (
	"context"
	"math"
	"testing"

	"github.com/kanz-eng/kanz/internal/risk/compute"
	varmodel "github.com/kanz-eng/kanz/internal/risk/compute/var"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// $1000 under returns {-10,-5,0,+5,+10}% ⇒ P&L {-100,-50,0,50,100}. n=5, α=0.99,
// k=⌈5·0.01⌉=1, so ES99 = −mean(worst 1) = 100.00. VaR99 (interpolated) = 98.00,
// so ES99 ≥ VaR99 holds.
func TestExpectedShortfall_Empirical(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{-0.10, -0.05, 0, 0.05, 0.10}}

	es := varmodel.ExpectedShortfall(varmodel.Config{})(context.Background(), p, prov)
	if es.Name != compute.MeasureES99 {
		t.Fatalf("name = %q, want ES99", es.Name)
	}
	if got := dval(es.Value); math.Abs(got-100.0) > 1e-9 {
		t.Fatalf("ES99 = %v, want 100.00", got)
	}
}

// ES99 ≥ VaR99 on the same sample — the core consistency invariant (both read
// the same portfolioPnL distribution).
func TestExpectedShortfall_GEVaR(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{-0.10, -0.07, -0.03, 0, 0.02, 0.05, 0.08, 0.10}}

	varv := dval(varmodel.Historical(varmodel.Config{})(context.Background(), p, prov).Value)
	es := dval(varmodel.ExpectedShortfall(varmodel.Config{})(context.Background(), p, prov).Value)
	if es < varv-1e-9 {
		t.Fatalf("ES99 (%v) must be ≥ VaR99 (%v)", es, varv)
	}
}

func TestExpectedShortfall_AllGains(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{0.01, 0.02, 0.03, 0.05}} // no losing scenario
	if got := dval(varmodel.ExpectedShortfall(varmodel.Config{})(context.Background(), p, prov).Value); got != 0 {
		t.Fatalf("all-gains window ⇒ ES99 floored at 0, got %v", got)
	}
}

func TestExpectedShortfall_InsufficientData(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{0.01}} // <2 scenarios
	if got := dval(varmodel.ExpectedShortfall(varmodel.Config{})(context.Background(), p, prov).Value); got != 0 {
		t.Fatalf("insufficient data ⇒ zero ES99, got %v", got)
	}
}

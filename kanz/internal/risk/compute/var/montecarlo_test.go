package varmodel_test

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
)

// mapProvider returns a distinct series per instrument (the fixedProvider in
// historical_test.go returns one series for all).
type mapProvider map[string][]float64

func (m mapProvider) Returns(_ context.Context, inst string, _ time.Time, _ int) ([]float64, error) {
	return m[inst], nil
}

func negate(s []float64) []float64 {
	out := make([]float64, len(s))
	for i, v := range s {
		out[i] = -v
	}
	return out
}

var sampleReturns = []float64{-0.10, -0.05, 0.05, 0.10, 0.0, -0.08, 0.08, 0.03}

// TestMonteCarlo_Deterministic: the same inputs + seed reproduce the same VaR
// (the replay-determinism property a random VaR would violate).
func TestMonteCarlo_Deterministic(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: sampleReturns}
	a := varmodel.MonteCarlo(varmodel.Config{})(context.Background(), p, prov)
	b := varmodel.MonteCarlo(varmodel.Config{})(context.Background(), p, prov)
	if a.Value.Coefficient != b.Value.Coefficient || a.Value.Exponent != b.Value.Exponent {
		t.Fatalf("non-deterministic: %v vs %v", dval(a.Value), dval(b.Value))
	}
	if dval(a.Value) <= 0 {
		t.Fatalf("expected a positive VaR, got %v", dval(a.Value))
	}
}

// TestMonteCarlo_SaneMagnitude: a $1000 position with ~2.1% daily vol should
// yield a 99% VaR near value × σ × z₀.₉₉ ≈ 1000 × 0.0214 × 2.326 ≈ 50.
func TestMonteCarlo_SaneMagnitude(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{-0.02, 0.02, -0.02, 0.02, -0.02, 0.02, -0.02, 0.02}}
	got := dval(varmodel.MonteCarlo(varmodel.Config{})(context.Background(), p, prov).Value)
	if got < 30 || got > 70 {
		t.Fatalf("VaR %v outside the sane band [30,70] for ~2.1%% vol on $1000", got)
	}
}

// TestMonteCarlo_DiversificationReducesVaR is the correlated-factor-shock
// payoff: two equal positions whose returns are anti-correlated must carry a
// lower portfolio VaR than the same two when perfectly correlated. This is the
// property historical resampling can't model directly and Monte-Carlo over the
// estimated covariance does.
func TestMonteCarlo_DiversificationReducesVaR(t *testing.T) {
	p := portfolio("USD",
		domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")},
		domain.Position{InstrumentID: "MSFT", MarketValue: money(1000, "USD")},
	)
	correlated := mapProvider{"AAPL": sampleReturns, "MSFT": sampleReturns}
	hedged := mapProvider{"AAPL": sampleReturns, "MSFT": negate(sampleReturns)}

	varCorr := dval(varmodel.MonteCarlo(varmodel.Config{})(context.Background(), p, correlated).Value)
	varHedged := dval(varmodel.MonteCarlo(varmodel.Config{})(context.Background(), p, hedged).Value)

	if varHedged >= varCorr {
		t.Fatalf("diversification not captured: hedged VaR %v should be < correlated VaR %v", varHedged, varCorr)
	}
}

func TestMonteCarlo_BaseCurrencyFilterAndInsufficient(t *testing.T) {
	// Non-base currency ⇒ skipped ⇒ zero.
	eur := portfolio("USD", domain.Position{InstrumentID: "VOD.L", MarketValue: money(1000, "EUR")})
	if got := dval(varmodel.MonteCarlo(varmodel.Config{})(context.Background(), eur, fixedProvider{ret: sampleReturns}).Value); got != 0 {
		t.Fatalf("non-base position must be skipped, got %v", got)
	}
	// One return ⇒ no covariance ⇒ zero.
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	if got := dval(varmodel.MonteCarlo(varmodel.Config{})(context.Background(), p, fixedProvider{ret: []float64{0.01}}).Value); got != 0 {
		t.Fatalf("insufficient data ⇒ zero, got %v", got)
	}
}

func TestRegisterMonteCarlo_OverridesPlaceholder(t *testing.T) {
	r := compute.DefaultRegistry()
	varmodel.RegisterMonteCarlo(context.Background(), r, fixedProvider{ret: sampleReturns}, varmodel.Config{})

	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	set := compute.ComputeMeasures(p, r, nil)
	m, ok := set.Lookup(compute.MeasureVaR99)
	if !ok {
		t.Fatal("VaR99 missing")
	}
	// gross=1000 ⇒ placeholder would be exactly 10; the MC model differs.
	if got := dval(m.Value); got <= 0 || (got > 9.99 && got < 10.01) {
		t.Fatalf("registered MC VaR looks like the placeholder or zero: %v", got)
	}
}

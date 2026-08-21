package varmodel

import (
	"context"
	"github.com/eighred/kanz/internal/dec"
	"math"
	"math/rand"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
)

// genReturns map[id][]float64 satisfies both compute.ReturnsProvider (for the MC
// model) and factormodel.ReturnsProvider (for the statistical factor model) — the
// SAME history feeds both, which is why their VaR reconciles.
type genReturns map[string][]float64

func (g genReturns) Returns(_ context.Context, id string, _ time.Time, _ int) ([]float64, error) {
	return g[id], nil
}

// TestFactorVaR_ReconcilesWithMonteCarlo: a full-rank statistical factor model
// and the MODEL-01e Monte-Carlo VaR are estimated off the same return history, so
// their 99% VaR agree to within Monte-Carlo sampling error — the factor model is
// a re-expression of the same covariance, not a different risk number.
func TestFactorVaR_ReconcilesWithMonteCarlo(t *testing.T) {
	const obs = 500
	rng := rand.New(rand.NewSource(7))
	a := make([]float64, obs)
	b := make([]float64, obs)
	for i := 0; i < obs; i++ {
		z1, z2 := rng.NormFloat64(), rng.NormFloat64()
		a[i] = 0.01 * z1
		b[i] = 0.01 * (0.5*z1 + 0.8660254*z2) // corr ≈ 0.5 with A
	}
	demean(a)
	demean(b)
	rp := genReturns{"A": a, "B": b}

	asOf := time.Now()
	p := domain.NewPortfolio("p1", "USD")
	usd := func(amt int64) *commonpb.Money {
		return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: amt, Exponent: 0}, CurrencyCode: "USD"}
	}
	p.SetPosition(domain.Position{InstrumentID: "A", MarketValue: usd(1_000_000), AsOf: asOf})
	p.SetPosition(domain.Position{InstrumentID: "B", MarketValue: usd(600_000), AsOf: asOf})

	// Factor (statistical, full-rank) VaR.
	model, err := factormodel.Fit(context.Background(), factormodel.Config{Type: factormodel.Statistical, StatFactors: 2},
		[]string{"A", "B"}, asOf, factormodel.Providers{Returns: rp})
	if err != nil {
		t.Fatal(err)
	}
	factorVaR := model.VaR(map[string]float64{"A": 1_000_000, "B": 600_000}, 0.99)

	// MODEL-01e Monte-Carlo VaR over the same returns.
	r := compute.DefaultRegistry()
	RegisterMonteCarlo(context.Background(), r, rp, Config{Confidence: 0.99, Draws: 50000, Seed: 11})
	mc, _ := compute.ComputeMeasures(p, r, nil).Lookup(compute.MeasureVaR99)
	mcVaR := dec.Float64Or(mc.Value, 0)

	if factorVaR <= 0 || mcVaR <= 0 {
		t.Fatalf("both VaRs must be positive: factor=%.2f mc=%.2f", factorVaR, mcVaR)
	}
	if rel := math.Abs(factorVaR-mcVaR) / mcVaR; rel > 0.10 {
		t.Fatalf("factor VaR %.2f and Monte-Carlo VaR %.2f must reconcile within 10%%, got %.1f%%", factorVaR, mcVaR, rel*100)
	}
}

func demean(xs []float64) {
	var m float64
	for _, x := range xs {
		m += x
	}
	m /= float64(len(xs))
	for i := range xs {
		xs[i] -= m
	}
}

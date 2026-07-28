package scenario

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
)

type fmReturns map[string][]float64

func (m fmReturns) Returns(_ context.Context, id string, _ time.Time, _ int) ([]float64, error) {
	return m[id], nil
}

// TestEvaluateFactorShock reprices the book through the factor model: a shock
// produces a different gross exposure than the unshocked state, while a nil model
// / empty shock falls back to plain measures.
func TestEvaluateFactorShock(t *testing.T) {
	rp := fmReturns{
		"A": {0.010, -0.020, 0.015, 0.000, -0.010, 0.020},
		"B": {0.012, -0.018, 0.013, 0.002, -0.011, 0.019},
	}
	model, err := factormodel.Fit(context.Background(), factormodel.Config{Type: factormodel.Statistical, StatFactors: 2},
		[]string{"A", "B"}, time.Now(), factormodel.Providers{Returns: rp})
	if err != nil {
		t.Fatal(err)
	}

	p := domain.NewPortfolio("p1", "USD")
	usd := func(a int64) *commonpb.Money {
		return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: a}, CurrencyCode: "USD"}
	}
	p.SetPosition(domain.Position{InstrumentID: "A", MarketValue: usd(100_000)})
	p.SetPosition(domain.Position{InstrumentID: "B", MarketValue: usd(80_000)})

	r := compute.DefaultRegistry()
	base, _ := EvaluateFactorShock(p, model, nil, r).Lookup(compute.MeasureGrossExposure) // empty shock ⇒ unshocked
	shocked, _ := EvaluateFactorShock(p, model, map[string]float64{"PC1": 0.5}, r).Lookup(compute.MeasureGrossExposure)

	bv := base.Value.GetCoefficient()
	sv := shocked.Value.GetCoefficient()
	if bv == sv {
		t.Fatalf("a factor shock must move gross exposure off the unshocked %d", bv)
	}
	if bv == 0 {
		t.Fatal("base gross exposure should be non-zero")
	}
}

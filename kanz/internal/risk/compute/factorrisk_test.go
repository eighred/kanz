package compute

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/factormodel"
)

type fmReturns map[string][]float64

func (m fmReturns) Returns(_ context.Context, id string, _ time.Time, _ int) ([]float64, error) {
	return m[id], nil
}

type staticModel struct{ m *factormodel.Model }

func (s staticModel) Model(context.Context, time.Time) (*factormodel.Model, bool) {
	return s.m, s.m != nil
}

func factorTestModel(t *testing.T) *factormodel.Model {
	t.Helper()
	rp := fmReturns{
		"A": {0.010, -0.020, 0.015, 0.000, -0.010, 0.020},
		"B": {0.012, -0.018, 0.013, 0.002, -0.011, 0.019},
		"C": {-0.005, 0.010, -0.008, 0.004, 0.012, -0.015},
	}
	m, err := factormodel.Fit(context.Background(), factormodel.Config{Type: factormodel.Statistical, StatFactors: 3},
		[]string{"A", "B", "C"}, time.Now(), factormodel.Providers{Returns: rp})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func factorTestPortfolio() *domain.Portfolio {
	p := domain.NewPortfolio("p1", "USD")
	usd := func(a int64) *commonpb.Money { return &commonpb.Money{Amount: dec(a, 0), CurrencyCode: "USD"} }
	p.SetPosition(domain.Position{InstrumentID: "A", Quantity: dec(100, 0), MarketValue: usd(100_000)})
	p.SetPosition(domain.Position{InstrumentID: "B", Quantity: dec(-50, 0), MarketValue: usd(-50_000)})
	p.SetPosition(domain.Position{InstrumentID: "C", Quantity: dec(30, 0), MarketValue: usd(30_000)})
	return p
}

func TestRegisterFactorRisk_VaRConsistentWithSplit(t *testing.T) {
	r := DefaultRegistry()
	RegisterFactorRisk(context.Background(), r, staticModel{m: factorTestModel(t)})
	ms := ComputeMeasures(factorTestPortfolio(), r, nil)

	sys, _ := ms.Lookup(MeasureSystematicRisk)
	spec, _ := ms.Lookup(MeasureSpecificRisk)
	fvar, ok := ms.Lookup(MeasureFactorVaR99)
	if !ok {
		t.Fatal("FactorVaR99 missing")
	}
	total := math.Hypot(decimalToFloat(sys.Value), decimalToFloat(spec.Value))
	if total <= 0 {
		t.Fatalf("total factor risk must be positive, got %.4f", total)
	}
	// FactorVaR99 = z₀.₉₉ · total, z₀.₉₉ ≈ 2.3263.
	wantVaR := 2.3263 * total
	if d := math.Abs(decimalToFloat(fvar.Value) - wantVaR); d > 0.01*wantVaR {
		t.Fatalf("FactorVaR99 %.2f must be ≈ 2.3263·total %.2f", decimalToFloat(fvar.Value), wantVaR)
	}
}

func TestRegisterFactorRisk_NoModelIsZero(t *testing.T) {
	r := DefaultRegistry()
	RegisterFactorRisk(context.Background(), r, staticModel{m: nil})
	ms := ComputeMeasures(factorTestPortfolio(), r, nil)
	fvar, _ := ms.Lookup(MeasureFactorVaR99)
	if decimalToFloat(fvar.Value) != 0 {
		t.Fatalf("FactorVaR99 with no model must be 0, got %.4f", decimalToFloat(fvar.Value))
	}
}

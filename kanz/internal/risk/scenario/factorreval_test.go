package scenario

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
)

type fmReturns map[string][]float64

func (m fmReturns) Returns(_ context.Context, id string, _ time.Time, _ int) ([]float64, error) {
	return m[id], nil
}

// TestEvaluateFactorShock reprices the book through the factor model: a shock
// produces a different gross exposure than the documented empty-shock no-op.
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
	base, baseCoverage := EvaluateFactorShock(p, model, nil, r)
	shocked, coverage := EvaluateFactorShock(p, model, map[string]float64{"PC1": 0.5}, r)
	if baseCoverage.ExcludedCount != 0 || coverage.ExcludedCount != 0 || coverage.Contributed != 2 {
		t.Fatalf("supported PCA shock refused: %+v / %+v", baseCoverage, coverage)
	}
	bv, sv := factorGross(t, base), factorGross(t, shocked)
	if bv == sv {
		t.Fatalf("a factor shock must move gross exposure off the unshocked %v", bv)
	}
	if bv == 0 {
		t.Fatal("base gross exposure should be non-zero")
	}
}

func rationalFactorModel(t *testing.T) *factormodel.Model {
	t.Helper()
	at := timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	model, err := factormodel.FromSnapshot(&factorpb.FactorModelSnapshot{
		Model: &factorpb.FactorModel{ModelId: "rational-fixture", AsOf: at, Factors: []*factorpb.Factor{
			{Name: "Value", Type: factorpb.FactorType_FACTOR_TYPE_STYLE}, {Name: "Momentum", Type: factorpb.FactorType_FACTOR_TYPE_STYLE},
		}},
		Covariance: &factorpb.FactorCovariance{ModelId: "rational-fixture", AsOf: at, Dimension: 2, Values: []float64{1, 0, 0, 1}},
		Exposures: []*factorpb.FactorExposure{
			{ModelId: "rational-fixture", AsOf: at, InstrumentId: "A", Loadings: []float64{2, -1}},
			{ModelId: "rational-fixture", AsOf: at, InstrumentId: "ZERO", Loadings: []float64{0, 0}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func factorBook(id string, amount int64) *domain.Portfolio {
	p := domain.NewPortfolio("factor-fixture", "USD")
	p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), MarketValue: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: amount}, CurrencyCode: "USD"}})
	return p
}

func factorGross(t *testing.T, set *domain.MeasureSet) float64 {
	t.Helper()
	if set == nil {
		t.Fatal("expected an assessed result")
	}
	m, ok := set.Lookup(compute.MeasureGrossExposure)
	if !ok {
		t.Fatal("gross exposure missing")
	}
	value, ok := dec.Float64(m.Value)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		t.Fatalf("nonfinite/unreadable gross exposure: %+v", m.Value)
	}
	return value
}

func TestFactorShockRationalOracleAndSupportedZero(t *testing.T) {
	model := rationalFactorModel(t)
	p := factorBook("A", 100)
	// 2*(1/20) + (-1)*(-1/10) = 1/5; 100*(1+1/5) = 120 USD.
	result, coverage := EvaluateFactorShock(p, model, map[string]float64{"Value": 1.0 / 20, "Momentum": -1.0 / 10}, nil)
	if coverage.Contributed != 1 || coverage.ExcludedCount != 0 || factorGross(t, result) != 120 {
		t.Fatalf("rational factor result/coverage: %+v", coverage)
	}
	if pos, _ := p.Position("A"); pos.MarketValue.Amount.Coefficient != 100 || pos.MarketValue.Amount.Exponent != 0 {
		t.Fatal("factor shock mutated the original book")
	}
	zero, coverage := EvaluateFactorShock(factorBook("ZERO", 100), model, map[string]float64{"Value": math.MaxFloat64}, nil)
	if coverage.Contributed != 1 || coverage.ExcludedCount != 0 || factorGross(t, zero) != 100 {
		t.Fatalf("supported zero loading confused with unknown input: %+v", coverage)
	}
}

func TestFactorShockPreflightRefusesBeforeDispatchEvenOnEmptyBook(t *testing.T) {
	model := rationalFactorModel(t)
	for _, tc := range []struct {
		name   string
		model  *factormodel.Model
		shocks map[string]float64
		reason string
	}{
		{"no model", nil, map[string]float64{"Value": .1}, compute.SkipNoModel},
		{"unknown", model, map[string]float64{"unknown": .1}, SkipUnsupportedFactorShock},
		{"mixed", model, map[string]float64{"Value": .1, "unknown": .1}, SkipUnsupportedFactorShock},
		{"unknown zero", model, map[string]float64{"unknown": 0}, SkipUnsupportedFactorShock},
		{"NaN", model, map[string]float64{"Value": math.NaN()}, SkipUnsupportedFactorShock},
		{"positive infinity", model, map[string]float64{"Value": math.Inf(1)}, SkipUnsupportedFactorShock},
		{"negative infinity", model, map[string]float64{"Value": math.Inf(-1)}, SkipUnsupportedFactorShock},
	} {
		for _, empty := range []bool{false, true} {
			label := "/holding"
			if empty {
				label = "/empty"
			}
			t.Run(tc.name+label, func(t *testing.T) {
				p := factorBook("A", 100)
				if empty {
					p = domain.NewPortfolio("empty", "USD")
				}
				dispatched := false
				registry := compute.NewRegistry()
				registry.Register(compute.MeasureGrossExposure, func(*domain.Portfolio) v1.Measure { dispatched = true; return v1.Measure{} })
				result, cov := EvaluateFactorShock(p, tc.model, tc.shocks, registry)
				if result != nil || dispatched || cov.ExcludedCount != 1 || cov.Contributed != 0 || len(cov.Exclusions) != 1 || cov.Exclusions[0].InstrumentID != "" || cov.Exclusions[0].Reason != tc.reason {
					t.Fatalf("unsupported batch looked assessed or dispatched: result=%v dispatch=%v coverage=%+v", result, dispatched, cov)
				}
			})
		}
	}
}

func TestFactorShockUnassessedInstrumentNeverReturnsPartialMeasures(t *testing.T) {
	for _, tc := range []struct {
		name, id     string
		shocks       map[string]float64
		missingValue bool
		reason       string
	}{
		{"outside universe", "UNKNOWN", map[string]float64{"Value": .1}, false, SkipUnassessedFactorInstrument},
		{"missing value", "A", map[string]float64{"Value": .1}, true, SkipUnassessedFactorInstrument},
		{"overflowing return", "A", map[string]float64{"Value": math.MaxFloat64}, false, SkipUnassessedFactorInstrument},
		{"fraction not representable", "A", map[string]float64{"Value": 1e13}, false, SkipInvalidFactorShockResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := factorBook(tc.id, 100)
			if tc.missingValue {
				p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(tc.id)})
			}
			// A valid zero-loading leg must not turn the unassessed leg into a
			// seemingly successful partial result.
			p.SetPosition(domain.Position{InstrumentID: "ZERO", MarketValue: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 50}, CurrencyCode: "USD"}})
			result, cov := EvaluateFactorShock(p, rationalFactorModel(t), tc.shocks, nil)
			if result != nil || cov.Contributed != 1 || cov.ExcludedCount != 1 || len(cov.Exclusions) != 1 || cov.Exclusions[0].Reason != tc.reason {
				t.Fatalf("unassessed leg produced partial measures: %v %+v", result, cov)
			}
		})
	}
}

func TestFactorEmptyShockRemainsNoopWithoutModel(t *testing.T) {
	for _, shocks := range []map[string]float64{nil, {}} {
		result, cov := EvaluateFactorShock(factorBook("UNKNOWN", 100), nil, shocks, nil)
		if cov.ExcludedCount != 0 || factorGross(t, result) != 100 {
			t.Fatalf("empty shock no-op changed: %+v", cov)
		}
	}
}

func TestFactorShockUnreadableAmountIsUnassessedEvenAtZeroLoading(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount *commonpb.Decimal
	}{
		{"absent", nil}, {"out of domain", &commonpb.Decimal{Coefficient: 1, Exponent: 65}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := domain.NewPortfolio("unreadable", "USD")
			p.SetPosition(domain.Position{InstrumentID: "ZERO", MarketValue: &commonpb.Money{Amount: tc.amount, CurrencyCode: "USD"}})
			result, cov := EvaluateFactorShock(p, rationalFactorModel(t), map[string]float64{"Value": .1}, nil)
			if result != nil || cov.Contributed != 0 || cov.ExcludedCount != 1 || len(cov.Exclusions) != 1 || cov.Exclusions[0].Reason != SkipUnassessedFactorInstrument {
				t.Fatalf("unknown amount became a clean zero-loading result: %v %+v", result, cov)
			}
		})
	}
}

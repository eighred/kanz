package compute_test

import (
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

func TestGrossExposure_SumsAbsoluteMarketValueInBaseCurrency(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "B", MarketValue: mkMoney(-300, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "C", MarketValue: mkMoney(500, 0, "EUR"), AsOf: baseTime}, // skipped
	)
	got := compute.GrossExposure(p)
	if got.Name != compute.MeasureGrossExposure {
		t.Errorf("Name=%q", got.Name)
	}
	// |1000| + |-300| = 1300 (EUR position excluded)
	if got.Value.Coefficient != 1300 {
		t.Errorf("Value=%d want 1300 (EUR position must be excluded — no FX in compute)", got.Value.Coefficient)
	}
}

func TestNetExposure_SignedSumInBaseCurrency(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "B", MarketValue: mkMoney(-300, 0, "USD"), AsOf: baseTime},
	)
	got := compute.NetExposure(p)
	if got.Value.Coefficient != 700 {
		t.Errorf("Value=%d want 700", got.Value.Coefficient)
	}
}

func TestVaR99_ParametricPlaceholderIs1pctOfGross(t *testing.T) {
	// gross = 1000, VaR99 = 0.01 × 1000 = 10
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
	)
	got := compute.VaR99(p)
	// 10 = 1000 × 0.01 = 1000 × (1 × 10^-2) = 1000 × 10^-2 = 10 with rep (1000, -2)
	if got.Value.Coefficient != 1000 || got.Value.Exponent != -2 {
		t.Errorf("Value=%d × 10^%d want 1000 × 10^-2 (= 10)", got.Value.Coefficient, got.Value.Exponent)
	}
}

func TestDelta_PlaceholderEqualsNetExposure(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(500, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "B", MarketValue: mkMoney(-200, 0, "USD"), AsOf: baseTime},
	)
	d := compute.Delta(p)
	n := compute.NetExposure(p)
	if d.Value.Coefficient != n.Value.Coefficient {
		t.Errorf("Delta=%d want NetExposure=%d (placeholder identity)", d.Value.Coefficient, n.Value.Coefficient)
	}
}

func TestComputeMeasures_DefaultRegistryProducesAll(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
	)
	set := compute.ComputeMeasures(p, nil, nil)
	names := set.Names()
	want := []v1.MeasureName{
		compute.MeasureDelta,
		compute.MeasureGrossExposure,
		compute.MeasureHHI,
		compute.MeasureNetExposure,
		compute.MeasureVaR99,
	}
	if len(names) != len(want) {
		t.Fatalf("names=%v want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("names[%d]=%q want %q", i, names[i], n)
		}
	}
}

func TestComputeMeasures_FilterNarrowsResults(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
	)
	set := compute.ComputeMeasures(p, nil, []v1.MeasureName{compute.MeasureVaR99})
	names := set.Names()
	if len(names) != 1 || names[0] != compute.MeasureVaR99 {
		t.Errorf("names=%v want [VaR99]", names)
	}
}

func TestComputeMeasures_UnknownFilterNameSilentlyDropped(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
	)
	set := compute.ComputeMeasures(p, nil, []v1.MeasureName{"Nonexistent"})
	if got := len(set.Names()); got != 0 {
		t.Errorf("names=%d want 0 (unknown filter ⇒ empty set)", got)
	}
}

func TestRegistry_CustomMeasureReplacesDefault(t *testing.T) {
	// Quants plug in real models via Register — verify the override path.
	r := compute.DefaultRegistry()
	r.Register(compute.MeasureVaR99, func(_ *domain.Portfolio) v1.Measure {
		return v1.Measure{
			Name:  compute.MeasureVaR99,
			Value: &commonpb.Decimal{Coefficient: 99999, Exponent: 0},
		}
	})
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
	)
	set := compute.ComputeMeasures(p, r, nil)
	got, _ := set.Lookup(compute.MeasureVaR99)
	if got.Value.Coefficient != 99999 {
		t.Errorf("Value=%d want 99999 (custom VaR not used)", got.Value.Coefficient)
	}
}

func TestMeasureSet_AsOfPropagatedFromPortfolio(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
	)
	set := compute.ComputeMeasures(p, nil, nil)
	if !set.AsOf().Equal(baseTime) {
		t.Errorf("AsOf=%v want %v", set.AsOf(), baseTime)
	}
}

func TestRegistry_NamesSortedLexicographically(t *testing.T) {
	r := compute.NewRegistry()
	r.Register("Zeta", compute.GrossExposure)
	r.Register("Alpha", compute.NetExposure)
	r.Register("Mu", compute.Delta)
	got := r.Names()
	want := []v1.MeasureName{"Alpha", "Mu", "Zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

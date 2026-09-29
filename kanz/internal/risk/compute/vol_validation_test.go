package compute

import (
	"context"
	"math"
	"sync"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing/volsurface"
)

func TestInvalidVolatilityIsExcludedFromGreeksAndRevaluation(t *testing.T) {
	asOf, terms, p := greeksSkipFixture(t)
	for _, vol := range []VolProvider{constVol(math.NaN()), constVol(math.Inf(1)), constVol(math.Inf(-1)), constVol(-.2), constVol(0), volsurface.NewStore()} {
		providers := GreeksProviders{Terms: terms, Spot: staticSpot{"UND": 100}, Vol: vol}
		r := DefaultRegistry()
		RegisterGreeks(context.Background(), r, providers)
		// No OnSkip callback: the published coverage must stand on its own.
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				ms := ComputeMeasures(p, r, nil)
				for _, name := range []v1.MeasureName{MeasureDelta, MeasureGamma, MeasureVega, MeasureTheta, MeasureRho} {
					m, ok := ms.Lookup(name)
					if !ok || m.Value.GetCoefficient() != 0 || m.Coverage.Contributed != 0 || m.Coverage.ExcludedCount != 1 || len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].Reason != SkipNoVol {
						t.Errorf("invalid vol produced complete-looking %s: %+v", name, m)
					}
				}
			})
		}
		wg.Wait()
		var skipped string
		providers.OnSkip = func(_, reason string) { skipped = reason }
		if money, ok := NewRevaluer(providers).RevalueOption(context.Background(), "OPT_A", asOf, dec0("USD"), RevalShocks{PriceFrac: -.1}); ok || money != nil || skipped != SkipNoVol {
			t.Fatalf("invalid vol entered revaluation: %v %v %s", money, ok, skipped)
		}
	}
}

func TestFiniteInputsCannotSerializeFailedPricingArithmetic(t *testing.T) {
	asOf, terms, p := greeksSkipFixture(t)
	spec := terms["OPT_A"]
	spec.Expiry = asOf.AddDate(2, 0, 0)
	terms["OPT_A"] = spec
	providers := GreeksProviders{Terms: terms, Spot: staticSpot{"UND": 100}, Vol: constVol(math.MaxFloat64)}
	r := DefaultRegistry()
	RegisterGreeks(context.Background(), r, providers)
	ms := ComputeMeasures(p, r, nil)
	for _, name := range []v1.MeasureName{MeasureDelta, MeasureGamma, MeasureVega, MeasureTheta, MeasureRho} {
		m, _ := ms.Lookup(name)
		if m.Value.GetCoefficient() != 0 || m.Coverage.Contributed != 0 || m.Coverage.ExcludedCount != 1 || m.Coverage.Exclusions[0].Reason != SkipInvalidPricing {
			t.Fatalf("failed pricing serialized: %+v", m)
		}
	}
	var skipped string
	providers.OnSkip = func(_, reason string) { skipped = reason }
	if money, ok := NewRevaluer(providers).RevalueOption(context.Background(), "OPT_A", asOf, dec0("USD"), RevalShocks{}); ok || money != nil || skipped != SkipInvalidPricing {
		t.Fatalf("failed revaluation serialized: %v %v %s", money, ok, skipped)
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64, 0x1p63} {
		if representablePricingAmount(value, 0) {
			t.Fatalf("unrepresentable coefficient accepted: %v", value)
		}
	}
	if !representablePricingAmount(-0x1p63, 0) || !representablePricingAmount(1.25, -2) {
		t.Fatal("representable coefficient rejected")
	}
}

func TestGreekAggregateOverflowIsExplicitlyUnassessed(t *testing.T) {
	asOf, terms, p := greeksSkipFixture(t)
	terms["OPT_B"] = terms["OPT_A"]
	for _, id := range []domain.InstrumentID{"OPT_A", "OPT_B"} {
		p.SetPosition(domain.Position{InstrumentID: id, Quantity: dec(100000000000, 0), MarketValue: dec0("USD"), AsOf: asOf})
	}
	r := DefaultRegistry()
	RegisterGreeks(context.Background(), r, GreeksProviders{Terms: terms, Spot: staticSpot{"UND": 100}, Vol: constVol(.2)})
	m, _ := ComputeMeasures(p, r, nil).Lookup(MeasureDelta)
	if m.Value.GetCoefficient() != 0 || m.Coverage.Contributed != 2 || m.Coverage.ExcludedCount != 1 || m.Coverage.Exclusions[0].Reason != SkipInvalidPricing {
		t.Fatalf("sum of representable legs overflowed without coverage: %+v", m)
	}
}

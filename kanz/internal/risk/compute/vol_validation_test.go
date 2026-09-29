package compute

import (
	"context"
	"math"
	"sync"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
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

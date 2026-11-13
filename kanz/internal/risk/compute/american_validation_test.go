package compute

import (
	"context"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/pricing"
)

func TestAmericanInvalidTreeIsUnassessedInGreeksAndRevaluation(t *testing.T) {
	asOf, terms, port := greeksSkipFixture(t)
	spec := terms["OPT_A"]
	spec.Type, spec.Exercise = pricing.Put, pricing.American
	terms["OPT_A"] = spec
	providers := GreeksProviders{Terms: terms, Spot: staticSpot{"UND": 60}, Vol: constVol(.001), Curve: pricing.FlatCurve(1)}
	r := DefaultRegistry()
	RegisterGreeks(context.Background(), r, providers)
	ms := ComputeMeasures(port, r, nil)
	for _, name := range []v1.MeasureName{MeasureDelta, MeasureGamma, MeasureVega, MeasureTheta, MeasureRho} {
		m, ok := ms.Lookup(name)
		if !ok || m.Value.GetCoefficient() != 0 || m.Coverage.Contributed != 0 || m.Coverage.ExcludedCount != 1 || len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].Reason != SkipInvalidPricing {
			t.Fatalf("invalid American tree produced complete-looking %s: %+v", name, m)
		}
	}
	var skipped string
	providers.OnSkip = func(_, reason string) { skipped = reason }
	if value, ok := NewRevaluer(providers).RevalueOption(context.Background(), "OPT_A", asOf, dec0("USD"), RevalShocks{}); ok || value != nil || skipped != SkipInvalidPricing {
		t.Fatalf("invalid American tree revalued: %v %v %q", value, ok, skipped)
	}
	// A valid base snapshot must not make a failed shocked lattice look usable.
	providers.Curve, providers.Vol = pricing.FlatCurve(.05), constVol(.2)
	skipped = ""
	if value, ok := NewRevaluer(providers).RevalueOption(context.Background(), "OPT_A", asOf, dec0("USD"), RevalShocks{GlobalVolBump: -.199999}); ok || value != nil || skipped != SkipInvalidPricing {
		t.Fatalf("invalid shocked American tree revalued: %v %v %q", value, ok, skipped)
	}
}

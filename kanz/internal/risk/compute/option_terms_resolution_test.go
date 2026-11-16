package compute_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/scenario"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

type optionResolutionFixture struct {
	t          *testing.T
	resolution compute.TermsResolution
	err        error
	asOf       time.Time
}

func (f optionResolutionFixture) OptionTerms(_ context.Context, _ string, asOf time.Time) (compute.OptionSpec, compute.TermsResolution, error) {
	if !asOf.Equal(f.asOf) {
		f.t.Errorf("terms horizon = %v, want %v", asOf, f.asOf)
	}
	return compute.OptionSpec{}, f.resolution, f.err
}

func resolutionPortfolio() *domain.Portfolio {
	asOf := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	p := domain.NewPortfolio("resolution", "USD")
	p.SetPosition(domain.Position{InstrumentID: "HOLDING", AsOf: asOf,
		Quantity:    &commonpb.Decimal{Coefficient: 1},
		MarketValue: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 100}, CurrencyCode: "USD"}})
	return p
}

func TestUnknownOptionTermsCannotBecomeLinearGreeksOrCleanStress(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resolution compute.TermsResolution
		err        error
		reason     string
	}{
		{"missing", compute.TermsUnknown, nil, compute.SkipNoTerms},
		{"invalid", compute.TermsUnusable, nil, compute.SkipInvalidOptionTerms},
		{"read failure", compute.TermsUnknown, errors.New("private store diagnostic"), compute.SkipTermsReadFailed},
		{"error overrides other", compute.TermsOtherVariant, errors.New("private store diagnostic"), compute.SkipTermsReadFailed},
		{"unknown resolution", compute.TermsResolution(99), nil, compute.SkipNoTerms},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := resolutionPortfolio()
			providers := compute.GreeksProviders{Terms: optionResolutionFixture{t, tc.resolution, tc.err, p.AsOf()}}
			assertUnassessedResolution(t, p, providers, tc.reason)
		})
	}
	t.Run("no provider", func(t *testing.T) {
		assertUnassessedResolution(t, resolutionPortfolio(), compute.GreeksProviders{}, compute.SkipNoTerms)
	})
}

func assertUnassessedResolution(t *testing.T, p *domain.Portfolio, providers compute.GreeksProviders, reason string) {
	t.Helper()
	r := compute.DefaultRegistry()
	compute.RegisterGreeks(context.Background(), r, providers)
	for _, name := range []v1.MeasureName{compute.MeasureDelta, compute.MeasureGamma, compute.MeasureVega, compute.MeasureTheta, compute.MeasureRho} {
		m, ok := compute.ComputeMeasures(p, r, nil).Lookup(name)
		if !ok || m.Value.GetCoefficient() != 0 || m.Coverage.Contributed != 0 || m.Coverage.ExcludedCount != 1 || len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].Reason != reason {
			t.Fatalf("unknown classification produced complete-looking %s: %+v", name, m)
		}
	}
	rv := compute.NewRevaluer(providers)
	pos, _ := p.Position("HOLDING")
	result := rv.RevalueOption(context.Background(), "HOLDING", p.AsOf(), pos.MarketValue, compute.RevalShocks{PriceFrac: -.2})
	if result.Status != compute.RevalUnassessed || result.Value != nil || result.Reason != reason {
		t.Fatalf("revaluation disposition = %+v", result)
	}
	projected, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{v1.ParallelShift{Pct: &commonpb.Decimal{Coefficient: -2, Exponent: -1}}}, nil, rv)
	if cov.ExcludedCount != 1 || cov.Contributed != 0 || len(cov.Exclusions) != 1 || cov.Exclusions[0].Reason != reason {
		t.Fatalf("unassessed revaluation returned clean stress: %+v", cov)
	}
	// Preserve the approximation without certifying it. The independent linear
	// arithmetic is 100 * (1 - 1/5) = 80 USD; the input book stays at 100 USD.
	m, _ := projected.Lookup(compute.MeasureGrossExposure)
	if m.Value.GetCoefficient() != 80000000 || m.Value.GetExponent() != -6 {
		t.Fatalf("degraded approximation, want exact USD80: %+v", m.Value)
	}
	if got := pos.MarketValue.Amount; got.Coefficient != 100 || got.Exponent != 0 {
		t.Fatalf("input value mutated: %+v", got)
	}
}

func TestKnownOtherOptionTermsPreserveLinearDeltaAndStress(t *testing.T) {
	p := resolutionPortfolio()
	providers := compute.GreeksProviders{Terms: optionResolutionFixture{t, compute.TermsOtherVariant, nil, p.AsOf()}}
	r := compute.DefaultRegistry()
	compute.RegisterGreeks(context.Background(), r, providers)
	m, _ := compute.ComputeMeasures(p, r, nil).Lookup(compute.MeasureDelta)
	if m.Value.GetCoefficient() != 1000000 || m.Value.GetExponent() != -4 || m.Coverage.Contributed != 1 || m.Coverage.ExcludedCount != 0 {
		t.Fatalf("known linear USD100 delta = %+v", m)
	}
	projected, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{v1.ParallelShift{Pct: &commonpb.Decimal{Coefficient: -2, Exponent: -1}}}, nil, compute.NewRevaluer(providers))
	if cov.ExcludedCount != 0 {
		t.Fatalf("known linear holding refused: %+v", cov)
	}
	m, _ = projected.Lookup(compute.MeasureGrossExposure)
	if m.Value.GetCoefficient() != 80000000 || m.Value.GetExponent() != -6 {
		t.Fatalf("known linear stress, want exact USD80: %+v", m.Value)
	}
}

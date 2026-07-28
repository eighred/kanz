package compute

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing"
)

// --- deterministic test providers ------------------------------------------

type staticTerms map[string]OptionSpec

func (m staticTerms) OptionTerms(_ context.Context, id string, _ time.Time) (OptionSpec, bool) {
	s, ok := m[id]
	return s, ok
}

type staticSpot map[string]float64

func (m staticSpot) Spot(_ context.Context, id string, _ time.Time) (float64, bool) {
	s, ok := m[id]
	return s, ok
}

type constVol float64

func (v constVol) Vol(_ context.Context, _ string, _, _ float64, _ time.Time) (float64, bool) {
	return float64(v), true
}

func TestRegisterGreeks_PortfolioAggregation(t *testing.T) {
	asOf := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := asOf.AddDate(1, 0, 0) // ~1y
	const spot, sigma, mult = 100.0, 0.20, 100.0

	terms := staticTerms{
		"OPT_A": {UnderlyingID: "UND", Strike: 100, Expiry: expiry, Type: pricing.Call, Exercise: pricing.European, Multiplier: mult},
		"OPT_B": {UnderlyingID: "UND", Strike: 110, Expiry: expiry, Type: pricing.Call, Exercise: pricing.European, Multiplier: mult},
	}
	providers := GreeksProviders{Terms: terms, Spot: staticSpot{"UND": spot}, Vol: constVol(sigma)}

	r := DefaultRegistry()
	RegisterGreeks(context.Background(), r, providers)

	p := domain.NewPortfolio("p1", "USD")
	// Long 10 OPT_A, short 5 OPT_B, plus a cash-equity position (linear Δ).
	p.SetPosition(domain.Position{InstrumentID: "OPT_A", Quantity: dec(10, 0), MarketValue: dec0("USD"), AsOf: asOf})
	p.SetPosition(domain.Position{InstrumentID: "OPT_B", Quantity: dec(-5, 0), MarketValue: dec0("USD"), AsOf: asOf})
	p.SetPosition(domain.Position{InstrumentID: "EQ", Quantity: dec(100, 0), MarketValue: &commonpb.Money{Amount: dec(5000, 0), CurrencyCode: "USD"}, AsOf: asOf})

	ms := ComputeMeasures(p, r, nil)

	// Expected: dollar delta = Σ g.Delta·S·n for options + equity MarketValue.
	ttm := expiry.Sub(asOf).Hours() / 24 / 365
	_, gA := pricing.PriceGreeks(pricing.Call, pricing.European, spot, 100, ttm, 0, 0, sigma)
	_, gB := pricing.PriceGreeks(pricing.Call, pricing.European, spot, 110, ttm, 0, 0, sigma)
	wantDelta := gA.Delta*spot*(10*mult) + gB.Delta*spot*(-5*mult) + 5000

	got, ok := ms.Lookup(MeasureDelta)
	if !ok {
		t.Fatal("Delta measure missing")
	}
	if d := math.Abs(decimalToFloat(got.Value) - wantDelta); d > 0.5 {
		t.Fatalf("portfolio Delta: got %.4f want %.4f (Δ %.4f)", decimalToFloat(got.Value), wantDelta, d)
	}

	// Gamma is option-only (equity contributes none) and positive for net... here
	// net gamma = (10−5)·dollar-gamma, still positive.
	wantGamma := gA.Gamma*spot*spot*(10*mult) + gB.Gamma*spot*spot*(-5*mult)
	g, _ := ms.Lookup(MeasureGamma)
	if d := math.Abs(decimalToFloat(g.Value) - wantGamma); d > 0.5 {
		t.Fatalf("portfolio Gamma: got %.4f want %.4f", decimalToFloat(g.Value), wantGamma)
	}
}

func TestRegisterGreeks_ReplacesPlaceholderDelta(t *testing.T) {
	// With no option terms, Delta falls back to the linear (net MarketValue)
	// behavior the placeholder provided — so existing equity books are unchanged.
	r := DefaultRegistry()
	RegisterGreeks(context.Background(), r, GreeksProviders{Terms: staticTerms{}, Spot: staticSpot{}, Vol: constVol(0.2)})

	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "EQ", Quantity: dec(1, 0), MarketValue: &commonpb.Money{Amount: dec(7500, 0), CurrencyCode: "USD"}, AsOf: time.Now()})
	ms := ComputeMeasures(p, r, nil)
	got, _ := ms.Lookup(MeasureDelta)
	if v := decimalToFloat(got.Value); math.Abs(v-7500) > 1e-6 {
		t.Fatalf("linear Delta fallback: got %.4f want 7500", v)
	}
}

func dec0(ccy string) *commonpb.Money { return &commonpb.Money{Amount: dec(0, 0), CurrencyCode: ccy} }

package varmodel_test

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
)

var asOf = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// fixedProvider returns one return series for every instrument.
type fixedProvider struct{ ret []float64 }

func (f fixedProvider) Returns(_ context.Context, _ string, _ time.Time, _ int) ([]float64, error) {
	return f.ret, nil
}

func money(amount int64, currency string) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: amount, Exponent: 0}, CurrencyCode: currency}
}

func portfolio(currency string, positions ...domain.Position) *domain.Portfolio {
	p := domain.NewPortfolio(v1.PortfolioID("P1"), domain.CurrencyCode(currency))
	p.SetAggregate(domain.AggregateUpdate{AsOf: asOf, BaseCurrency: domain.CurrencyCode(currency)})
	for _, pos := range positions {
		p.SetPosition(pos)
	}
	return p
}

func dval(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

// TestHistorical_EmpiricalQuantile pins the historical-simulation arithmetic on
// a hand-computable distribution: $1000 under returns {-10,-5,0,+5,+10}% gives
// P&L {-100,-50,0,50,100}; the 1% lower-tail quantile (linear-interpolated) is
// -98, so VaR99 = 98.00.
func TestHistorical_EmpiricalQuantile(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{-0.10, -0.05, 0, 0.05, 0.10}}

	m := varmodel.Historical(varmodel.Config{})(context.Background(), p, prov)
	if m.Name != compute.MeasureVaR99 {
		t.Fatalf("name = %q, want VaR99", m.Name)
	}
	if got := dval(m.Value); math.Abs(got-98.0) > 1e-9 {
		t.Fatalf("VaR99 = %v, want 98.00", got)
	}
}

// TestHistorical_DecouplesFromGrossPlaceholder is the failure-as-documentation
// pin (the RISK-12 `VaR99/Gross=0.01` property no longer holds for the real
// model): the historical VaR is the empirical tail loss, NOT 1%×gross.
func TestHistorical_DecouplesFromGrossPlaceholder(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{-0.10, -0.05, 0, 0.05, 0.10}}

	gross := dval(compute.GrossExposure(p).Value)                                             // 1000
	placeholder := dval(compute.VaR99(p).Value)                                               // 10 = 1%×gross
	hist := dval(varmodel.Historical(varmodel.Config{})(context.Background(), p, prov).Value) // 98

	if placeholder/gross != 0.01 {
		t.Fatalf("sanity: placeholder should still be 1%%×gross, got ratio %v", placeholder/gross)
	}
	if math.Abs(hist/gross-0.01) < 1e-6 {
		t.Fatalf("historical VaR must decouple from the 1%%×gross placeholder, got ratio %v", hist/gross)
	}
}

func TestHistorical_BaseCurrencyFilter(t *testing.T) {
	// Only a non-base-currency position ⇒ no legs ⇒ zero-value measure.
	p := portfolio("USD", domain.Position{InstrumentID: "VOD.L", MarketValue: money(1000, "EUR")})
	prov := fixedProvider{ret: []float64{-0.10, 0.10}}
	if got := dval(varmodel.Historical(varmodel.Config{})(context.Background(), p, prov).Value); got != 0 {
		t.Fatalf("non-base-currency position must be skipped, got VaR %v", got)
	}
}

func TestHistorical_InsufficientData(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	for name, prov := range map[string]fixedProvider{
		"empty":  {ret: nil},
		"single": {ret: []float64{0.01}},
	} {
		if got := dval(varmodel.Historical(varmodel.Config{})(context.Background(), p, prov).Value); got != 0 {
			t.Errorf("%s: insufficient data ⇒ zero VaR, got %v", name, got)
		}
	}
}

// TestRegister_OverridesPlaceholderEndToEnd wires the real path: a Memory price
// store → StoreReturnsProvider → Register over a registry → ComputeMeasures, and
// asserts the served VaR99 is the data-driven model, not the placeholder.
func TestRegister_OverridesPlaceholderEndToEnd(t *testing.T) {
	s := store.NewMemory()
	// Five closes → four returns with a clear down-move tail.
	closes := []float64{100, 110, 105, 95, 90}
	var obs []store.Observation
	for i, px := range closes {
		obs = append(obs, store.Observation{
			InstrumentID:    "AAPL",
			ObservationTime: asOf.AddDate(0, 0, i-len(closes)),
			Price:           &commonpb.Decimal{Coefficient: int64(px * 100), Exponent: -2},
			Kind:            store.PriceKindClose,
			KnowledgeTime:   asOf.AddDate(0, 0, i-len(closes)),
		})
	}
	if err := s.Put(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	provider := returns.NewStoreReturnsProvider(s, returns.ReturnsConfig{Method: returns.ReturnSimple})

	r := compute.DefaultRegistry()
	varmodel.Register(context.Background(), r, provider, varmodel.Config{})

	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	set := compute.ComputeMeasures(p, r, nil)

	m, ok := set.Lookup(compute.MeasureVaR99)
	if !ok {
		t.Fatal("VaR99 missing from computed set")
	}
	hist := dval(m.Value)
	if hist <= 0 {
		t.Fatalf("data-driven VaR should be a positive loss, got %v", hist)
	}
	// Must differ from the 1%×gross placeholder (gross=1000 ⇒ placeholder 10).
	if math.Abs(hist-10.0) < 1e-6 {
		t.Fatalf("registered VaR still equals the placeholder (10), override failed: %v", hist)
	}
}

// TestRegister_RegistersTailMeasures asserts the one Register call wires ES99 and
// both drawdown measures alongside VaR99, all served off the same provider.
func TestRegister_RegistersTailMeasures(t *testing.T) {
	s := store.NewMemory()
	closes := []float64{100, 110, 105, 95, 90}
	var obs []store.Observation
	for i, px := range closes {
		obs = append(obs, store.Observation{
			InstrumentID:    "AAPL",
			ObservationTime: asOf.AddDate(0, 0, i-len(closes)),
			Price:           &commonpb.Decimal{Coefficient: int64(px * 100), Exponent: -2},
			Kind:            store.PriceKindClose,
			KnowledgeTime:   asOf.AddDate(0, 0, i-len(closes)),
		})
	}
	if err := s.Put(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	provider := returns.NewStoreReturnsProvider(s, returns.ReturnsConfig{Method: returns.ReturnSimple})

	r := compute.DefaultRegistry()
	varmodel.Register(context.Background(), r, provider, varmodel.Config{})

	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	set := compute.ComputeMeasures(p, r, nil)

	for _, name := range []v1.MeasureName{
		compute.MeasureES99, compute.MeasureMaxDrawdown, compute.MeasureMaxDrawdownAmount,
	} {
		if _, ok := set.Lookup(name); !ok {
			t.Errorf("%s missing from the registered set", name)
		}
	}
	// ES99 ≥ VaR99 on the served set.
	varM, _ := set.Lookup(compute.MeasureVaR99)
	esM, _ := set.Lookup(compute.MeasureES99)
	if dval(esM.Value) < dval(varM.Value)-1e-9 {
		t.Fatalf("served ES99 (%v) must be ≥ VaR99 (%v)", dval(esM.Value), dval(varM.Value))
	}
}

// TestDefaultRegistry_NoMarketDataHasHHINotTail: without Register (no price
// store), HHI is served (positions-only) but the returns-backed tail measures
// are absent — the honest no-market-data posture.
func TestDefaultRegistry_NoMarketDataHasHHINotTail(t *testing.T) {
	r := compute.DefaultRegistry()
	names := map[v1.MeasureName]bool{}
	for _, n := range r.Names() {
		names[n] = true
	}
	if !names[compute.MeasureHHI] {
		t.Error("HHI must be served even without market data")
	}
	for _, n := range []v1.MeasureName{
		compute.MeasureES99, compute.MeasureMaxDrawdown, compute.MeasureMaxDrawdownAmount,
	} {
		if names[n] {
			t.Errorf("%s must NOT be in the default (no-market-data) registry", n)
		}
	}
}

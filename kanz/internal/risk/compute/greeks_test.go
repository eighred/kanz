package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

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
	if d := math.Abs(decutil.Float64Or(got.Value, 0) - wantDelta); d > 0.5 {
		t.Fatalf("portfolio Delta: got %.4f want %.4f (Δ %.4f)", decutil.Float64Or(got.Value, 0), wantDelta, d)
	}

	// Gamma is option-only (equity contributes none) and positive for net... here
	// net gamma = (10−5)·dollar-gamma, still positive.
	wantGamma := gA.Gamma*spot*spot*(10*mult) + gB.Gamma*spot*spot*(-5*mult)
	g, _ := ms.Lookup(MeasureGamma)
	if d := math.Abs(decutil.Float64Or(g.Value, 0) - wantGamma); d > 0.5 {
		t.Fatalf("portfolio Gamma: got %.4f want %.4f", decutil.Float64Or(g.Value, 0), wantGamma)
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
	if v := decutil.Float64Or(got.Value, 0); math.Abs(v-7500) > 1e-6 {
		t.Fatalf("linear Delta fallback: got %.4f want 7500", v)
	}
}

func dec0(ccy string) *commonpb.Money { return &commonpb.Money{Amount: dec(0, 0), CurrencyCode: ccy} }

// --- an unpriceable option is reported, not silently mispriced --------------

// noVol resolves nothing: the surface has no point for any strike/expiry.
type noVol struct{}

func (noVol) Vol(_ context.Context, _ string, _, _ float64, _ time.Time) (float64, bool) {
	return 0, false
}

// greeksSkipFixture is one long option position on OPT_A, plus the terms that
// make it an option. Each test below removes exactly one pricing input.
func greeksSkipFixture(t *testing.T) (time.Time, staticTerms, *domain.Portfolio) {
	t.Helper()
	asOf := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	terms := staticTerms{
		"OPT_A": {UnderlyingID: "UND", Strike: 100, Expiry: asOf.AddDate(1, 0, 0),
			Type: pricing.Call, Exercise: pricing.European, Multiplier: 100},
	}
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "OPT_A", Quantity: dec(10, 0),
		MarketValue: &commonpb.Money{Amount: dec(90000, 0), CurrencyCode: "USD"}, AsOf: asOf})
	return asOf, terms, p
}

// AN OPTION WITH NO SPOT IS REPORTED, AND IS NOT PRICED AS STOCK.
//
// This is the failure the observer exists for, and the one that used to be a
// misclassification rather than a gap: an unmarked underlying made the spot
// lookup fail, the position fell through to the linear branch, and a contract
// worth $90,000 of market value was added to Delta as though it were Δ≡1 stock —
// overstating the book's directional risk by the whole notional, with Gamma zero.
func TestRegisterGreeks_AnOptionWithNoSpotIsReportedAndNotPricedAsStock(t *testing.T) {
	_, terms, p := greeksSkipFixture(t)

	var skipped []string
	r := DefaultRegistry()
	RegisterGreeks(context.Background(), r, GreeksProviders{
		Terms:  terms,
		Spot:   staticSpot{}, // UND was never marked
		Vol:    constVol(0.2),
		OnSkip: func(id, reason string) { skipped = append(skipped, id+":"+reason) },
	})

	ms := ComputeMeasures(p, r, nil)

	d, _ := ms.Lookup(MeasureDelta)
	if got := decutil.Float64Or(d.Value, 0); got != 0 {
		t.Fatalf("Delta = %.4f for an option with no spot, want 0 — a contract with no "+
			"underlying mark is being reported as %.4f of directional exposure it was never "+
			"priced for", got, got)
	}
	if len(skipped) == 0 {
		t.Fatal("an option contributed zero to every Greek and nothing was reported — a book " +
			"whose spots went missing reads exactly like a book holding no options")
	}
	for _, s := range skipped {
		if s != "OPT_A:"+SkipNoSpot {
			t.Errorf("reported %q, want OPT_A:%s", s, SkipNoSpot)
		}
	}
}

// AN OPTION WITH NO VOL IS REPORTED. Zero Greeks here was already arithmetically
// contained — the position stayed an option — but it was silent, so a surface
// that failed to calibrate showed up as a book that had lost its convexity.
func TestRegisterGreeks_AnOptionWithNoVolIsReported(t *testing.T) {
	_, terms, p := greeksSkipFixture(t)

	var skipped []string
	r := DefaultRegistry()
	RegisterGreeks(context.Background(), r, GreeksProviders{
		Terms:  terms,
		Spot:   staticSpot{"UND": 100},
		Vol:    noVol{}, // the surface has no point for this strike/expiry
		OnSkip: func(id, reason string) { skipped = append(skipped, id+":"+reason) },
	})

	ms := ComputeMeasures(p, r, nil)
	g, _ := ms.Lookup(MeasureGamma)
	if got := decutil.Float64Or(g.Value, 0); got != 0 {
		t.Fatalf("Gamma = %.4f with no vol, want 0 — the fixture is not exercising the skip", got)
	}
	if len(skipped) == 0 {
		t.Fatal("an option was left unpriced for want of an implied vol and nothing was " +
			"reported — the book's Gamma fell to zero with no gap recorded anywhere")
	}
	for _, s := range skipped {
		if s != "OPT_A:"+SkipNoVol {
			t.Errorf("reported %q, want OPT_A:%s", s, SkipNoVol)
		}
	}
}

// A SHARE IS NOT REPORTED, or the counter is useless.
//
// Without this the two tests above are satisfied by an observer that fires for
// every position, which would bury the options that really were dropped under
// every equity on the book and make the alert unbuildable.
func TestRegisterGreeks_APlainShareIsNotReportedAsSkipped(t *testing.T) {
	asOf := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var skipped []string
	r := DefaultRegistry()
	RegisterGreeks(context.Background(), r, GreeksProviders{
		Terms:  staticTerms{}, // resolves nothing: every position is a non-option
		Spot:   staticSpot{},
		Vol:    noVol{},
		OnSkip: func(id, reason string) { skipped = append(skipped, id+":"+reason) },
	})

	p := domain.NewPortfolio("p1", "USD")
	for _, id := range []string{"EQ_A", "EQ_B", "EQ_C"} {
		p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), Quantity: dec(10, 0),
			MarketValue: &commonpb.Money{Amount: dec(5000, 0), CurrencyCode: "USD"}, AsOf: asOf})
	}
	ComputeMeasures(p, r, nil)

	if len(skipped) != 0 {
		t.Errorf("a share book reported %d Greek skips: %v — a share is correctly priced "+
			"linearly rather than skipped, and counting it would bury the options that really "+
			"were dropped", len(skipped), skipped)
	}
}

// A NIL SPOT OR VOL PROVIDER DOES NOT TAKE THE RISK ENGINE DOWN.
//
// A GreeksProviders built with a seam left unwired used to dereference it on the
// first option position on the book — a nil-pointer panic inside a measure
// closure, i.e. the whole valuation run lost, not one position. It is now the
// same skip as any other absent input, and it says so.
func TestRegisterGreeks_ANilSpotOrVolProviderSkipsRatherThanPanics(t *testing.T) {
	_, terms, p := greeksSkipFixture(t)

	for _, tc := range []struct {
		name      string
		providers GreeksProviders
		want      string
	}{
		{"nil Spot", GreeksProviders{Terms: terms, Spot: nil, Vol: constVol(0.2)}, SkipNoSpot},
		{"nil Vol", GreeksProviders{Terms: terms, Spot: staticSpot{"UND": 100}, Vol: nil}, SkipNoVol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var skipped []string
			providers := tc.providers
			providers.OnSkip = func(id, reason string) { skipped = append(skipped, id+":"+reason) }

			r := DefaultRegistry()
			RegisterGreeks(context.Background(), r, providers)
			ms := ComputeMeasures(p, r, nil) // must not panic

			d, _ := ms.Lookup(MeasureDelta)
			if got := decutil.Float64Or(d.Value, 0); got != 0 {
				t.Fatalf("Delta = %.4f with %s, want 0", got, tc.name)
			}
			if len(skipped) == 0 {
				t.Fatalf("%s produced no report — an unwired seam must surface on the first "+
					"event, not sit at a healthy-looking zero", tc.name)
			}
			for _, s := range skipped {
				if s != "OPT_A:"+tc.want {
					t.Errorf("reported %q, want OPT_A:%s", s, tc.want)
				}
			}
		})
	}
}

// AN EXPIRED OPTION IS REPORTED even though its zero Greeks are correct: a
// contract past expiry still on the book at valuation time is an unprocessed
// settlement or a stale reference record, and this is where it becomes visible.
func TestRegisterGreeks_AnExpiredOptionIsReported(t *testing.T) {
	asOf := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	terms := staticTerms{
		"OPT_A": {UnderlyingID: "UND", Strike: 100, Expiry: asOf.AddDate(0, 0, -1),
			Type: pricing.Call, Exercise: pricing.European, Multiplier: 100},
	}
	var skipped []string
	r := DefaultRegistry()
	RegisterGreeks(context.Background(), r, GreeksProviders{
		Terms:  terms,
		Spot:   staticSpot{"UND": 100},
		Vol:    constVol(0.2),
		OnSkip: func(id, reason string) { skipped = append(skipped, id+":"+reason) },
	})

	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "OPT_A", Quantity: dec(10, 0),
		MarketValue: &commonpb.Money{Amount: dec(90000, 0), CurrencyCode: "USD"}, AsOf: asOf})
	ComputeMeasures(p, r, nil)

	if len(skipped) == 0 {
		t.Fatal("an expired contract sat on the book unremarked — the settlement or reference " +
			"record behind it has no other place to surface")
	}
	for _, s := range skipped {
		if s != "OPT_A:"+SkipExpired {
			t.Errorf("reported %q, want OPT_A:%s", s, SkipExpired)
		}
	}
}

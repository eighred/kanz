package volsurface

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing"
)

// sviQuotes generates listed-option quotes whose smile IS an SVI slice, so the
// fit has a known ground truth: price each strike under vol √(w(k)/T).
func sviQuotes(t *testing.T, p SVIParams, expiry, spot, r, q float64, strikes []float64) []OptionQuote {
	t.Helper()
	fwd := spot * math.Exp((r-q)*expiry)
	var quotes []OptionQuote
	for _, K := range strikes {
		w := p.TotalVar(math.Log(K / fwd))
		if w <= 0 {
			t.Fatalf("ground-truth slice has non-positive variance at K=%.4g", K)
		}
		vol := math.Sqrt(w / expiry)
		quotes = append(quotes, OptionQuote{
			Type:   pricing.Call,
			Strike: K,
			Expiry: expiry,
			Price:  pricing.BlackScholesPrice(pricing.Call, spot, K, expiry, r, q, vol),
		})
	}
	return quotes
}

// FitSVI must recover a synthetic SVI smile: fitted vols within a few bp of
// the generating vols across the quoted strikes, and the surface arb-free.
func TestFitSVI_RecoversSyntheticSmile(t *testing.T) {
	truth := SVIParams{A: 0.02, B: 0.4, Rho: -0.4, M: 0.0, Sigma: 0.4}
	const spot, r, q, expiry = 100.0, 0.03, 0.0, 0.5
	strikes := []float64{70, 80, 90, 95, 100, 105, 110, 120, 130}
	quotes := sviQuotes(t, truth, expiry, spot, r, q, strikes)

	surf, err := FitSVI(quotes, spot, pricing.FlatCurve(r), q)
	if err != nil {
		t.Fatalf("FitSVI: %v", err)
	}
	fwd := spot * math.Exp((r-q)*expiry)
	for _, K := range strikes {
		want := math.Sqrt(truth.TotalVar(math.Log(K/fwd)) / expiry)
		got, ok := surf.Vol(K, expiry)
		if !ok {
			t.Fatalf("Vol(%v) not ok", K)
		}
		if math.Abs(got-want) > 2e-3 {
			t.Errorf("vol at K=%v: got %.5f want %.5f", K, got, want)
		}
	}
}

// A flat smile is legitimate SVI (b=0) — the fit must not degenerate.
func TestFitSVI_FlatSmile(t *testing.T) {
	const spot, r, vol, expiry = 100.0, 0.02, 0.25, 1.0
	var quotes []OptionQuote
	for _, K := range []float64{80, 90, 100, 110, 120} {
		quotes = append(quotes, OptionQuote{
			Type: pricing.Call, Strike: K, Expiry: expiry,
			Price: pricing.BlackScholesPrice(pricing.Call, spot, K, expiry, r, 0, vol),
		})
	}
	surf, err := FitSVI(quotes, spot, pricing.FlatCurve(r), 0)
	if err != nil {
		t.Fatalf("FitSVI: %v", err)
	}
	if got, _ := surf.Vol(100, expiry); math.Abs(got-vol) > 2e-3 {
		t.Errorf("flat smile: got %.5f want %.5f", got, vol)
	}
}

// The Axel Vogt SVI parameters are the classic butterfly-arbitrage example —
// the density goes negative, so ButterflyFree must reject them.
func TestButterflyFree_RejectsVogtParams(t *testing.T) {
	vogt := SVIParams{A: -0.0410, B: 0.1331, Rho: 0.3060, M: 0.3586, Sigma: 0.4153}
	if vogt.ButterflyFree(-1.5, 1.5) {
		t.Fatal("Vogt parameters admit butterfly arbitrage; ButterflyFree must be false")
	}
	// A tame smile passes.
	ok := SVIParams{A: 0.02, B: 0.1, Rho: -0.3, M: 0, Sigma: 0.3}
	if !ok.ButterflyFree(-1.5, 1.5) {
		t.Fatal("arb-free parameters wrongly rejected")
	}
}

// Total variance decreasing in expiry is calendar arbitrage: a 6M 40%-vol
// slice followed by a 1Y 20%-vol slice must be rejected as a set.
func TestFitSVI_RejectsCalendarArbitrage(t *testing.T) {
	const spot, r = 100.0, 0.0
	strikes := []float64{85, 95, 100, 105, 115}
	var quotes []OptionQuote
	for _, K := range strikes {
		quotes = append(quotes,
			OptionQuote{Type: pricing.Call, Strike: K, Expiry: 0.5,
				Price: pricing.BlackScholesPrice(pricing.Call, spot, K, 0.5, r, 0, 0.40)},
			OptionQuote{Type: pricing.Call, Strike: K, Expiry: 1.0,
				Price: pricing.BlackScholesPrice(pricing.Call, spot, K, 1.0, r, 0, 0.20)},
		)
	}
	if _, err := FitSVI(quotes, spot, pricing.FlatCurve(r), 0); !errors.Is(err, ErrArbitrage) {
		t.Fatalf("want ErrArbitrage, got %v", err)
	}
}

func TestFitSVI_Errors(t *testing.T) {
	if _, err := FitSVI(nil, 100, pricing.FlatCurve(0.02), 0); !errors.Is(err, ErrSVIFit) {
		t.Errorf("empty quotes: want ErrSVIFit, got %v", err)
	}
	two := []OptionQuote{
		{Type: pricing.Call, Strike: 100, Expiry: 1, Price: 10},
		{Type: pricing.Call, Strike: 110, Expiry: 1, Price: 6},
	}
	if _, err := FitSVI(two, 100, pricing.FlatCurve(0.02), 0); !errors.Is(err, ErrSVIFit) {
		t.Errorf("2-quote slice: want ErrSVIFit, got %v", err)
	}
}

type staticVolQuotes struct {
	quotes []OptionQuote
	spot   float64
	err    error
}

func (s staticVolQuotes) Quotes(context.Context, string, time.Time) ([]OptionQuote, float64, error) {
	return s.quotes, s.spot, s.err
}

// Refresh publishes point-in-time; a failed fit leaves the prior surface
// serving; the store resolves per as_of (the curve.Store contract).
func TestVolCalibrator_RefreshAndStore(t *testing.T) {
	ctx := context.Background()
	const spot, r, expiry = 100.0, 0.02, 0.5
	truth := SVIParams{A: 0.02, B: 0.3, Rho: -0.3, M: 0, Sigma: 0.4}
	quotes := sviQuotes(t, truth, expiry, spot, r, 0, []float64{80, 90, 100, 110, 120})

	store := NewStore()
	asOf := time.Date(2026, 7, 1, 16, 0, 0, 0, time.UTC)
	cal := &Calibrator{
		Source: staticVolQuotes{quotes: quotes, spot: spot},
		Store:  store,
		Disc:   pricing.FlatCurve(r),
	}
	if _, err := cal.Refresh(ctx, "SPX", asOf); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := store.Vol(ctx, "SPX", 100, expiry, asOf.Add(-time.Minute)); ok {
		t.Error("resolved a surface before the first version")
	}
	got, ok := store.Vol(ctx, "SPX", 100, expiry, asOf.Add(time.Hour))
	if !ok {
		t.Fatal("surface not resolved after publish")
	}
	fwd := spot * math.Exp(r*expiry)
	want := math.Sqrt(truth.TotalVar(math.Log(100/fwd)) / expiry)
	if math.Abs(got-want) > 2e-3 {
		t.Errorf("stored vol: got %.5f want %.5f", got, want)
	}

	bad := &Calibrator{Source: staticVolQuotes{err: errors.New("vendor down")}, Store: store, Disc: pricing.FlatCurve(r)}
	if _, err := bad.Refresh(ctx, "SPX", asOf.Add(2*time.Hour)); err == nil {
		t.Fatal("Refresh must surface a source error")
	}
	if v, ok := store.Vol(ctx, "SPX", 100, expiry, asOf.Add(3*time.Hour)); !ok || math.Abs(v-got) > 1e-12 {
		t.Error("a failed Refresh must leave the previous surface serving")
	}
}

package volsurface

import (
	"math"
	"testing"

	"github.com/eighred/kanz/internal/risk/pricing"
)

func approx(t *testing.T, name string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Fatalf("%s: got %.8f want %.8f", name, got, want)
	}
}

// Price an option at a known vol, then recover that vol — the implied-vol solve
// round-trips.
func TestImpliedVol_RoundTrip(t *testing.T) {
	S, K, ttm, r, q := 100.0, 100.0, 1.0, 0.05, 0.0
	for _, want := range []float64{0.10, 0.20, 0.45, 0.80} {
		price := pricing.BlackScholesPrice(pricing.Call, S, K, ttm, r, q, want)
		got, ok := ImpliedVol(pricing.Call, price, S, K, ttm, r, q)
		if !ok {
			t.Fatalf("implied vol failed for σ=%.2f", want)
		}
		approx(t, "implied vol", got, want, 1e-4)
	}
}

func TestImpliedVol_OutsideArbitrageBand(t *testing.T) {
	// A price above spot is impossible for a call ⇒ no finite vol.
	if _, ok := ImpliedVol(pricing.Call, 200, 100, 100, 1, 0.05, 0); ok {
		t.Fatal("expected ok=false for an above-spot call price")
	}
}

// Build a grid, query exact nodes (round-trip) and interpolate a midpoint.
func TestSurface_RoundTripAndInterpolation(t *testing.T) {
	points := []Point{
		{Strike: 90, Expiry: 0.5, Vol: 0.30}, {Strike: 110, Expiry: 0.5, Vol: 0.20},
		{Strike: 90, Expiry: 1.0, Vol: 0.28}, {Strike: 110, Expiry: 1.0, Vol: 0.22},
	}
	s, err := Build(points)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range points {
		got, ok := s.Vol(p.Strike, p.Expiry)
		if !ok {
			t.Fatalf("grid node (%v,%v) missing", p.Strike, p.Expiry)
		}
		approx(t, "grid node", got, p.Vol, 1e-12)
	}
	// Center of the grid: bilinear mean of the four corners.
	mid, _ := s.Vol(100, 0.75)
	approx(t, "bilinear center", mid, (0.30+0.20+0.28+0.22)/4, 1e-12)
	// Clamp: outside the strike range pins to the edge column.
	lo, _ := s.Vol(50, 0.5)
	approx(t, "clamp low strike", lo, 0.30, 1e-12)
	hi, _ := s.Vol(200, 1.0)
	approx(t, "clamp high strike", hi, 0.22, 1e-12)
}

func TestBuild_IncompleteGridErrors(t *testing.T) {
	// Three of four cells ⇒ incomplete grid.
	_, err := Build([]Point{
		{Strike: 90, Expiry: 0.5, Vol: 0.3}, {Strike: 110, Expiry: 0.5, Vol: 0.2},
		{Strike: 90, Expiry: 1.0, Vol: 0.28},
	})
	if err == nil {
		t.Fatal("expected incomplete-grid error")
	}
}

func TestFromQuotes(t *testing.T) {
	S, r, q := 100.0, 0.05, 0.0
	mk := func(k, ttm, sigma float64) OptionQuote {
		return OptionQuote{Type: pricing.Call, Strike: k, Expiry: ttm, Price: pricing.BlackScholesPrice(pricing.Call, S, k, ttm, r, q, sigma)}
	}
	quotes := []OptionQuote{mk(90, 0.5, 0.30), mk(110, 0.5, 0.20), mk(90, 1.0, 0.28), mk(110, 1.0, 0.22)}
	s, err := FromQuotes(quotes, S, r, q)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Vol(90, 0.5)
	approx(t, "recovered surface vol", got, 0.30, 1e-4)
}

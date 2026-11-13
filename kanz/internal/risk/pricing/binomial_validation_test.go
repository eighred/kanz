package pricing

import (
	"math"
	"testing"
)

func TestBinomialInvalidTreeNeverSubstitutesEuropean(t *testing.T) {
	// An American put can be exercised now for 40. The old inadmissible
	// probability fallback returned the European value (~0), deleting that right.
	if p := Price(Put, American, 60, 100, 1, 1, 0, .001); !math.IsNaN(p) {
		t.Fatalf("invalid CRR probability produced a price: %v (immediate exercise 40)", p)
	}
	// The base tree is valid, but the negative rate bump makes p < 0.
	// A finite price cannot authorize a partially available set of sensitivities.
	base, g := PriceGreeks(Put, American, 100, 100, 1, -.02, 0, .001415)
	if math.IsNaN(base) || math.IsInf(base, 0) {
		t.Fatal("single-bump regression must start from a valid base tree")
	}
	for _, v := range []float64{g.Delta, g.Gamma, g.Vega, g.Theta, g.Rho} {
		if !math.IsNaN(v) {
			t.Fatalf("failed bump left a partial Greek: %+v", g)
		}
	}
}

func TestBinomialNonfiniteInputAndCollapsedTreeRefuse(t *testing.T) {
	for field := range 6 {
		for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			v := [6]float64{60, 100, 0, .05, 0, .2}
			v[field] = bad
			p, g := PriceGreeks(Put, American, v[0], v[1], v[2], v[3], v[4], v[5])
			for _, value := range []float64{p, g.Delta, g.Gamma, g.Vega, g.Theta, g.Rho} {
				if !math.IsNaN(value) {
					t.Fatalf("field %d unavailable input became a candidate: %v", field, value)
				}
			}
		}
	}
	for _, v := range [][6]float64{{100, 100, 1e-320, 0, 0, 1e-320}, {100, 100, 1, 0, 0, 1000}, {1e308, 100, 1, 0, 0, 1}} {
		if p := BinomialPrice(Put, American, v[0], v[1], v[2], v[3], v[4], v[5], 200); !math.IsNaN(p) {
			t.Fatalf("unusable lattice returned finite price: %v", p)
		}
	}
}

func TestBinomialHandDerivedStoppingTree(t *testing.T) {
	// Three annual periods, u=2, d=1/2, p=1/2, discount=4/5.
	// Put terminal payoffs [9,6,0,0]; optimal preceding rows [8,12/5,0],
	// [6,24/25]. Root = (4/5)*(6+24/25)/2 = 348/125.
	p := BinomialPrice(Put, American, 8, 10, 3, math.Log(1.25), 0, math.Log(2), 3)
	if math.IsNaN(p) || math.IsInf(p, 0) || math.Abs(p-348.0/125) > 1e-12 {
		t.Fatalf("optimal stopping: got %.17g, want 348/125", p)
	}
}

func TestBinomialValidAmericanAndFiniteDegeneratesRemainAvailable(t *testing.T) {
	price, g := PriceGreeks(Put, American, 100, 100, 1, .05, 0, .2)
	for _, value := range []float64{price, g.Delta, g.Gamma, g.Vega, g.Theta, g.Rho} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			t.Fatalf("ordinary valid American contract unavailable: %v %+v", price, g)
		}
	}
	if price < BlackScholesPrice(Put, 100, 100, 1, .05, 0, .2) {
		t.Fatal("valid American put lost its exercise premium")
	}
	for _, tc := range []struct {
		kind         OptionType
		spot, strike float64
		time, vol    float64
		intrinsic    float64
	}{
		{Put, 60, 100, 0, .2, 40},
		{Call, 110, 100, 0, .2, 10},
		{Put, 60, 100, 1, 0, 40},
		{Call, 0, 100, 1, .2, 0},
		{Put, 100, 0, 1, .2, 0},
	} {
		price, g := PriceGreeks(tc.kind, American, tc.spot, tc.strike, tc.time, .05, 0, tc.vol)
		if price != tc.intrinsic || g != (Greeks{}) {
			t.Fatalf("finite degenerate convention changed: %+v: %v %+v", tc, price, g)
		}
	}
}

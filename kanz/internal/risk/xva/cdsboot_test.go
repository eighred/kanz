package xva

import (
	"errors"
	"math"
	"testing"
)

func discAt(r float64) func(float64) float64 {
	return func(t float64) float64 { return math.Exp(-r * t) }
}

// Every input CDS must reprice to par off the bootstrapped curve — the FI-01f
// round-trip property on credit.
func TestBootstrapCDS_RoundTrip(t *testing.T) {
	quotes := []CDSQuote{{1, 0.0060}, {3, 0.0090}, {5, 0.0110}, {10, 0.0130}}
	df := discAt(0.03)
	c, err := BootstrapCDS(quotes, 0.4, df)
	if err != nil {
		t.Fatalf("BootstrapCDS: %v", err)
	}
	for _, q := range quotes {
		if got := c.ParSpread(q.Tenor, df); math.Abs(got-q.Spread) > 1e-10 {
			t.Errorf("par spread at %.0fy: got %.8f want %.8f", q.Tenor, got, q.Spread)
		}
	}
	// Upward-sloping spreads imply increasing forward hazards.
	for k := 1; k < len(c.Hazards); k++ {
		if c.Hazards[k] <= c.Hazards[k-1] {
			t.Errorf("forward hazard must increase with an upward spread curve: h[%d]=%.6f <= h[%d]=%.6f",
				k, c.Hazards[k], k-1, c.Hazards[k-1])
		}
	}
}

// A single quote must land near the credit triangle h ≈ s/LGD — the bootstrap
// generalizes FromCDS, it does not contradict it.
func TestBootstrapCDS_SingleQuoteNearCreditTriangle(t *testing.T) {
	c, err := BootstrapCDS([]CDSQuote{{5, 0.0100}}, 0.4, discAt(0.02))
	if err != nil {
		t.Fatalf("BootstrapCDS: %v", err)
	}
	triangle := 0.0100 / 0.6
	if rel := math.Abs(c.Hazards[0]-triangle) / triangle; rel > 0.05 {
		t.Errorf("hazard %.6f vs credit triangle %.6f: rel err %.3f > 5%%", c.Hazards[0], triangle, rel)
	}
}

// nil df means undiscounted — still solvable and repricing.
func TestBootstrapCDS_NilDiscount(t *testing.T) {
	c, err := BootstrapCDS([]CDSQuote{{2, 0.005}, {5, 0.008}}, 0.4, nil)
	if err != nil {
		t.Fatalf("BootstrapCDS: %v", err)
	}
	if got := c.ParSpread(5, nil); math.Abs(got-0.008) > 1e-10 {
		t.Errorf("undiscounted par spread: got %.8f want 0.008", got)
	}
}

func TestBootstrapCDS_Errors(t *testing.T) {
	df := discAt(0.02)
	cases := map[string]struct {
		quotes   []CDSQuote
		recovery float64
	}{
		"empty":             {nil, 0.4},
		"duplicate tenor":   {[]CDSQuote{{5, 0.01}, {5, 0.011}}, 0.4},
		"negative spread":   {[]CDSQuote{{5, -0.01}}, 0.4},
		"recovery >= 1":     {[]CDSQuote{{5, 0.01}}, 1.0},
		"inverted too deep": {[]CDSQuote{{5, 0.0200}, {10, 0.0020}}, 0.4},
	}
	for name, tc := range cases {
		if _, err := BootstrapCDS(tc.quotes, tc.recovery, df); !errors.Is(err, ErrCDSBootstrap) {
			t.Errorf("%s: want ErrCDSBootstrap, got %v", name, err)
		}
	}
}

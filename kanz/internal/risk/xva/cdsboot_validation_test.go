package xva

import (
	"errors"
	"math"
	"testing"
)

func TestCDSNonfiniteQuotesAndRecoveryRefuse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		quotes   []CDSQuote
		recovery float64
	}{
		{"NaN spread", []CDSQuote{{1, math.NaN()}}, 0.4},
		{"positive infinite spread", []CDSQuote{{1, math.Inf(1)}}, 0.4},
		{"negative infinite spread", []CDSQuote{{1, math.Inf(-1)}}, 0.4},
		{"NaN tenor", []CDSQuote{{math.NaN(), 0.01}}, 0.4},
		{"positive infinite tenor", []CDSQuote{{math.Inf(1), 0.01}}, 0.4},
		{"negative infinite tenor", []CDSQuote{{math.Inf(-1), 0.01}}, 0.4},
		{"NaN recovery", []CDSQuote{{1, 0.01}}, math.NaN()},
		{"positive infinite recovery", []CDSQuote{{1, 0.01}}, math.Inf(1)},
		{"negative infinite recovery", []CDSQuote{{1, 0.01}}, math.Inf(-1)},
		{"later invalid quote", []CDSQuote{{1, 0.01}, {3, math.NaN()}}, 0.4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c, err := BootstrapCDS(tc.quotes, tc.recovery, func(float64) float64 {
				calls++
				return 1
			})
			if !errors.Is(err, ErrCDSBootstrap) || len(c.Hazards) != 0 || len(c.Tenors) != 0 {
				t.Fatalf("unusable inputs produced curve %+v, error=%v", c, err)
			}
			if calls != 0 {
				t.Fatalf("invalid inputs reached discount/model evaluation %d times", calls)
			}
		})
	}
}

func TestCDSUnusableDiscountAndIntermediateStatesRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		df   func(float64) float64
	}{
		{"NaN discount", func(float64) float64 { return math.NaN() }},
		{"positive infinite discount", func(float64) float64 { return math.Inf(1) }},
		{"negative infinite discount", func(float64) float64 { return math.Inf(-1) }},
		{"zero discount", func(float64) float64 { return 0 }},
		{"negative discount", func(float64) float64 { return -1 }},
		{"underflowed risky annuity", func(float64) float64 { return math.SmallestNonzeroFloat64 }},
		{"overflowed risky annuity", func(float64) float64 { return math.MaxFloat64 }},
		{"invalid later payment", func(t float64) float64 {
			if t > 0.25 {
				return math.NaN()
			}
			return 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, spread := range []float64{0, 0.01} {
				c, err := BootstrapCDS([]CDSQuote{{2, spread}}, 0.4, tc.df)
				if !errors.Is(err, ErrCDSBootstrap) || len(c.Hazards) != 0 {
					t.Fatalf("spread %g: unusable model produced %+v, error=%v", spread, c, err)
				}
			}
		})
	}
}

func TestCDSParSpreadSignalsUnusableLegAndPreservesExpiredContract(t *testing.T) {
	c := FlatHazard(0.02, 0.4)
	tinyDF := func(float64) float64 { return 1e-322 }
	if got := c.ParSpread(0.25, tinyDF); !math.IsNaN(got) {
		t.Fatalf("positive protection leg underflow became reported spread %g", got)
	}
	if c, err := BootstrapCDS([]CDSQuote{{2, 0.01}}, 0.4, tinyDF); !errors.Is(err, ErrCDSBootstrap) || len(c.Hazards) != 0 {
		t.Fatalf("underflowed positive leg escaped bootstrap refusal: %+v, error=%v", c, err)
	}
	if c, err := BootstrapCDS([]CDSQuote{{2, 0}}, 0.4, tinyDF); err != nil || c.Hazards[0] != 0 {
		t.Fatalf("valid zero protection leg was refused: %+v, error=%v", c, err)
	}
	for _, tenor := range []float64{0, -1} {
		if got := c.ParSpread(tenor, func(float64) float64 {
			t.Fatal("expired CDS evaluated discounting")
			return math.NaN()
		}); got != 0 {
			t.Fatalf("expired tenor %g changed its zero contract: %g", tenor, got)
		}
	}
}

func TestCDSBracketCannotAdmitNegativeForwardHazard(t *testing.T) {
	// First-quarter survival is exactly 1/2; with no later defaults the
	// half-year risky annuity is 5/16 and protection is 3/10, so par is 0.96.
	c := CreditCurve{Tenors: []float64{0.25}, Hazards: []float64{4 * math.Log(2)}, Recovery: 0.4}
	if h, err := solveHazard(c, CDSQuote{0.5, 0.96 - 1e-13}, nil); !errors.Is(err, ErrCDSBootstrap) {
		t.Fatalf("unbracketed quote accepted with hazard %g, error=%v", h, err)
	}
	if c, err := BootstrapCDS([]CDSQuote{{0.25, 100}}, 0.4, nil); !errors.Is(err, ErrCDSBootstrap) || len(c.Hazards) != 0 {
		t.Fatalf("quote beyond the finite upper bracket produced %+v, error=%v", c, err)
	}
}

func TestCDSBisectionChecksEveryTrial(t *testing.T) {
	calls := 0
	df := func(float64) float64 {
		calls++
		// A 1y quote has four payments. Both bracket endpoints are usable;
		// the first midpoint's first payment is not.
		if calls == 9 {
			return math.NaN()
		}
		return 1
	}
	if c, err := BootstrapCDS([]CDSQuote{{1, 0.01}}, 0.4, df); !errors.Is(err, ErrCDSBootstrap) || len(c.Hazards) != 0 {
		t.Fatalf("invalid bisection trial produced %+v, error=%v", c, err)
	}
}

func TestCDSZeroSpreadAndIndependentFlatHazardOracle(t *testing.T) {
	for _, tc := range []struct {
		name string
		df   func(float64) float64
	}{
		{"undiscounted", nil},
		{"discounted", discAt(0.03)},
		{"negative rate", discAt(-0.03)},
		{"small positive discount", func(float64) float64 { return 1e-300 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zero, err := BootstrapCDS([]CDSQuote{{1, 0}, {3, 0}}, 0.4, tc.df)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range zero.Hazards {
				if h != 0 {
					t.Fatalf("valid zero spread became hazard %g", h)
				}
			}
			if zero.Survival(3) != 1 || zero.ParSpread(3, tc.df) != 0 {
				t.Fatal("zero spread did not preserve survival one and par spread zero")
			}
			// With flat hazard and equal quarterly accruals the geometric terms
			// cancel, even under time-varying discounting. Independently:
			// s = (2*LGD/delta)*tanh(h*delta/2), delta=1/4 year.
			const hazard = 0.02
			spread := 4.8 * math.Tanh(hazard/8)
			c, err := BootstrapCDS([]CDSQuote{{1, spread}, {3, spread}}, 0.4, tc.df)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range c.Hazards {
				if math.IsNaN(h) || math.IsInf(h, 0) || math.Abs(h-hazard) > 1e-12 {
					t.Fatalf("hazard=%g, want independent %.12g/year within 1e-12", h, hazard)
				}
			}
			if got := c.ParSpread(3, tc.df); math.IsNaN(got) || math.Abs(got-spread) > 1e-13 {
				t.Fatalf("par spread=%g, want independent %.12g within 1e-13", got, spread)
			}
		})
	}
}

func TestCDSUsableAggregateLegsSurviveVanishingTailContributions(t *testing.T) {
	// The h=20 bracket's far tail underflows over 100y, but both aggregate
	// legs remain usable. This must not refuse an otherwise solvable curve.
	const hazard = 0.02
	spread := 4.8 * math.Tanh(hazard/8)
	c, err := BootstrapCDS([]CDSQuote{{100, spread}}, 0.4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := c.Hazards[0]; math.IsNaN(h) || math.Abs(h-hazard) > 1e-12 {
		t.Fatalf("long-maturity hazard=%g, want independent %.12g/year", h, hazard)
	}
}

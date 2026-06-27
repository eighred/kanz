package curve

import (
	"math"
	"testing"
)

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.8f want %.8f (Δ %.2e)", name, got, want, math.Abs(got-want))
	}
}

func TestZeroCurve_DiscountAndRate(t *testing.T) {
	// Flat 5% continuous zero ⇒ DF(t) = exp(-0.05·t).
	c, err := NewZeroCurve([]float64{1, 2, 5}, []float64{0.05, 0.05, 0.05}, Continuous, LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "Rate(3)", c.Rate(3), 0.05, 1e-12)
	approx(t, "DF(2)", c.Discount(2), math.Exp(-0.10), 1e-12)
	approx(t, "DF(0)", c.Discount(0), 1, 0)
	// Flat extrapolation beyond the long pillar.
	approx(t, "Rate(30)", c.Rate(30), 0.05, 1e-12)
}

func TestZeroCurve_LinearInterpolation(t *testing.T) {
	c, err := NewZeroCurve([]float64{1, 3}, []float64{0.02, 0.04}, Continuous, LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	// Midpoint tenor 2 ⇒ linear average 0.03.
	approx(t, "Zero(2)", c.Zero(2), 0.03, 1e-12)
}

func TestZeroCurve_CompoundingConversion(t *testing.T) {
	// 5% annual-compounded ⇒ continuous = ln(1.05); DF(1) must equal 1/1.05.
	c, err := NewZeroCurve([]float64{1}, []float64{0.05}, Annual, LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "DF(1) annual", c.Discount(1), 1/1.05, 1e-12)
}

func TestZeroCurve_LogLinearForwardConstant(t *testing.T) {
	// Log-linear-DF interpolation ⇒ constant instantaneous forward between pillars.
	c, err := NewZeroCurve([]float64{1, 4}, []float64{0.03, 0.05}, Continuous, LogLinearDF)
	if err != nil {
		t.Fatal(err)
	}
	f12 := c.Forward(1, 2)
	f23 := c.Forward(2, 3)
	approx(t, "constant forward", f12, f23, 1e-9)
}

func TestZeroCurve_Errors(t *testing.T) {
	if _, err := NewZeroCurve(nil, nil, Continuous, LinearZero); err == nil {
		t.Fatal("want ErrNoPillars")
	}
	if _, err := NewZeroCurve([]float64{2, 2}, []float64{0.01, 0.02}, Continuous, LinearZero); err == nil {
		t.Fatal("want ErrNonAscending on duplicate tenor")
	}
}

func TestBootstrap_RoundTrip(t *testing.T) {
	// Par quotes at annual tenors; the bootstrap + ParRate inverse must recover
	// every input quote exactly (the FI-01f round-trip property).
	quotes := []ParQuote{
		{1, 0.030},
		{2, 0.035},
		{3, 0.040},
		{4, 0.043},
		{5, 0.045},
	}
	c, err := Bootstrap(quotes, LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	for n, q := range quotes {
		ct := make([]float64, n+1)
		for i := range ct {
			ct[i] = float64(i + 1) // annual coupon dates 1..n+1
		}
		approx(t, "ParRate round-trip", c.ParRate(ct), q.Rate, 1e-10)
	}
}

func TestBootstrap_DiscountFactorsDescend(t *testing.T) {
	c, err := Bootstrap([]ParQuote{{1, 0.04}, {2, 0.045}, {5, 0.05}}, LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	if !(c.Discount(1) > c.Discount(2) && c.Discount(2) > c.Discount(5)) {
		t.Fatalf("discount factors must descend: %.6f %.6f %.6f", c.Discount(1), c.Discount(2), c.Discount(5))
	}
	if c.Discount(1) >= 1 {
		t.Fatalf("DF(1) must be < 1 for positive rates, got %.6f", c.Discount(1))
	}
}

func TestBootstrap_Rejects(t *testing.T) {
	if _, err := Bootstrap(nil, LinearZero); err == nil {
		t.Fatal("want error on empty quotes")
	}
}

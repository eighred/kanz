package curve

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestNegativeParRatesHavePositiveDiscountsAboveOne(t *testing.T) {
	c, err := Bootstrap([]ParQuote{{1, -.01}, {2, -.01}}, LogLinearDF)
	if err != nil {
		t.Fatalf("valid negative-rate strip refused: %v", err)
	}
	// Exact annual par equations: D1=100/99, D2=10000/9801.
	for i, want := range []float64{100.0 / 99, 10000.0 / 9801} {
		got := c.Discount(float64(i + 1))
		if !finiteCurve(got) || math.Abs(got-want) > 2e-15 {
			t.Fatalf("negative-rate DF%d=%g want %g", i+1, got, want)
		}
	}
}

func TestUnknownOrNonfiniteCurveDefinitionRefuses(t *testing.T) {
	for _, v := range []struct {
		tenors, rates []float64
		comp          Compounding
		interp        Interpolation
	}{
		{[]float64{1}, []float64{.02}, Compounding(99), LinearZero},
		{[]float64{1}, []float64{.02}, Continuous, Interpolation(99)},
		{[]float64{math.NaN()}, []float64{.02}, Continuous, LinearZero},
		{[]float64{math.Inf(1)}, []float64{.02}, Continuous, LinearZero},
		{[]float64{1}, []float64{math.NaN()}, Continuous, LinearZero},
		{[]float64{1}, []float64{math.Inf(1)}, Continuous, LinearZero},
		{[]float64{1}, []float64{-1}, Annual, LinearZero},
		{[]float64{1}, []float64{-2}, SemiAnnual, LinearZero},
	} {
		if c, err := NewZeroCurve(v.tenors, v.rates, v.comp, v.interp); err == nil || c != nil {
			t.Errorf("unavailable definition produced a curve: %+v", v)
		}
	}
	for _, rate := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if c, err := Bootstrap([]ParQuote{{1, rate}}, LinearZero); c != nil || err == nil {
			t.Errorf("invalid par rate produced a curve: %v", rate)
		}
	}
	if c, err := Bootstrap([]ParQuote{{1e308, -5e-309}}, LinearZero); c != nil || err == nil {
		t.Error("overflowing solved annuity produced a curve")
	}
}

func TestCompoundingPreservesSubEpsilonRates(t *testing.T) {
	for _, comp := range []Compounding{Annual, SemiAnnual} {
		c, err := NewZeroCurve([]float64{1}, []float64{1e-20}, comp, LinearZero)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Zero(1); math.Abs(got/1e-20-1) > 1e-15 {
			t.Errorf("compounding %d erased a nonzero rate: %g", comp, got)
		}
	}
	c, err := Bootstrap([]ParQuote{{1, 1e-20}, {2, 1e-20}}, LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Zero(2); math.Abs(got/1e-20-1) > 1e-15 {
		t.Errorf("bootstrap erased a rate: %g", got)
	}
	if got := c.ParRate([]float64{1, 2}); math.Abs(got/1e-20-1) > 1e-15 {
		t.Errorf("repricing erased a rate: %g", got)
	}
}

func TestForwardAcrossAdjacentBinary64Tenors(t *testing.T) {
	// Linear zero z(t)=.01+.01*t gives instantaneous forward .01+.02*t.
	// Log-linear discounts through (1,.02),(3,.04) give forward (.12-.02)/2.
	for _, interp := range []Interpolation{LinearZero, LogLinearDF} {
		c, _ := NewZeroCurve([]float64{1, 3}, []float64{.02, .04}, Continuous, interp)
		if f := c.Forward(2, math.Nextafter(2, math.Inf(1))); math.Abs(f-.05) > 5e-16 {
			t.Errorf("interpolation %d lost the interval forward: %.17g", interp, f)
		}
	}
}

func TestInvalidCalibrationDoesNotInstallOrCapture(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := NewStore()
	prior, _ := NewZeroCurve([]float64{1}, []float64{.03}, Continuous, LinearZero)
	store.Put("USD", at, prior)
	for _, bad := range []RateQuote{{Kind: Deposit, Tenor: 1, Value: math.NaN()}, {Kind: Swap, Tenor: math.Inf(1), Value: .03}, {Kind: Swap, Tenor: 4097, Value: .03}, {Kind: Future, Tenor: 1, Span: -1, Value: .03}} {
		called := false
		cal := Calibrator{Store: store, Source: capturedSource{strip: Strip{Quotes: []RateQuote{bad}, Capture: &InputCapture{CapturedAt: at}}},
			OnCalibrated: func(context.Context, string, time.Time, *Curve) error { called = true; return nil },
			OnCaptured:   func(CalibrationCapture) { called = true },
		}
		if got, err := cal.Refresh(context.Background(), "USD", at.Add(time.Hour)); err == nil || got != nil || called {
			t.Errorf("invalid quote installed or captured: %+v", bad)
		}
		if got, ok := store.Curve(context.Background(), "USD", at.Add(time.Hour)); !ok || got != prior {
			t.Fatal("failed refresh replaced the prior curve")
		}
	}
}

func TestFutureStartingAtValuationTimeRemainsSupported(t *testing.T) {
	// A future's tenor is its period start, not its maturity. Zero is a known
	// start at valuation time; the existing separate short-end anchor still gates it.
	c, err := Calibrate([]RateQuote{{Kind: Deposit, Tenor: .1, Value: .03}, {Kind: Future, Tenor: 0, Span: .25, Value: .04}}, LogLinearDF)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Discount(.25); !finiteCurve(got) || math.Abs(got-100.0/101) > 1e-15 {
		t.Fatalf("known zero-start future: %g", got)
	}
}

func BenchmarkBootstrap(b *testing.B) {
	for _, n := range []int{16, 64, 256} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			quotes := make([]ParQuote, n)
			for i := range quotes {
				quotes[i] = ParQuote{float64(i+1) / 4, .02}
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Bootstrap(quotes, LogLinearDF); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestUnusableCurveQueriesNeverBecomeDefaultsOrPanic(t *testing.T) {
	c, _ := NewZeroCurve([]float64{1, 2}, []float64{.03, .04}, Continuous, LinearZero)
	for name, query := range map[string]func() float64{
		"nan-zero":           func() float64 { return c.Zero(math.NaN()) },
		"nan-discount":       func() float64 { return c.Discount(math.NaN()) },
		"infinite-rate":      func() float64 { return c.Rate(math.Inf(1)) },
		"underflow-discount": func() float64 { return c.Discount(1e308) },
		"reversed-forward":   func() float64 { return c.Forward(2, 1) },
		"empty-coupons":      func() float64 { return c.ParRate(nil) },
		"repeated-coupons":   func() float64 { return c.ParRate([]float64{1, 1}) },
		"nan-coupon":         func() float64 { return c.ParRate([]float64{math.NaN()}) },
		"zero-curve":         func() float64 { return new(Curve).Discount(0) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("unavailable query panicked: %v", r)
				}
			}()
			if v := query(); !math.IsNaN(v) {
				t.Errorf("unavailable query returned %g", v)
			}
		})
	}
}

// The expectation must reject NaN explicitly; abs(NaN)>epsilon is false.
func finiteCurve(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

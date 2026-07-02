package curve

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// On a swaps-only annual strip Calibrate must reproduce Bootstrap exactly —
// the two solve the same par condition, one by recurrence, one by bisection.
func TestCalibrate_SwapsOnlyMatchesBootstrap(t *testing.T) {
	pars := []ParQuote{{1, 0.030}, {2, 0.034}, {3, 0.037}, {4, 0.039}, {5, 0.040}}
	want, err := Bootstrap(pars, LogLinearDF)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	var quotes []RateQuote
	for _, p := range pars {
		quotes = append(quotes, RateQuote{Kind: Swap, Tenor: p.Tenor, Value: p.Rate})
	}
	got, err := Calibrate(quotes, LogLinearDF)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	for _, tenor := range want.Tenors() {
		w, g := want.Zero(tenor), got.Zero(tenor)
		if math.Abs(w-g) > 1e-10 {
			t.Errorf("zero(%.0fy): Calibrate %.12f != Bootstrap %.12f", tenor, g, w)
		}
	}
}

// Every input instrument must reprice off the calibrated curve (FI-01f
// round-trip): deposit DF, futures forward, swap par rate.
func TestCalibrate_MixedCurveRepricesInputs(t *testing.T) {
	quotes := []RateQuote{
		{Kind: Deposit, Tenor: 0.25, Value: 0.0300},
		{Kind: Deposit, Tenor: 0.5, Value: 0.0315},
		{Kind: Future, Tenor: 0.5, Span: 0.25, Value: FutureRateFromPrice(96.60)}, // 3.40%
		{Kind: Future, Tenor: 0.75, Span: 0.25, Value: 0.0355},
		{Kind: Swap, Tenor: 2, Value: 0.0380},
		{Kind: Swap, Tenor: 5, Value: 0.0405},
		{Kind: Swap, Tenor: 10, Value: 0.0420},
	}
	c, err := Calibrate(quotes, LogLinearDF)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	for _, q := range quotes {
		switch q.Kind {
		case Deposit:
			want := 1 / (1 + q.Value*q.Tenor)
			if got := c.Discount(q.Tenor); math.Abs(got-want) > 1e-12 {
				t.Errorf("deposit %.2fy DF: got %.12f want %.12f", q.Tenor, got, want)
			}
		case Future:
			// Implied simple forward over the underlying period.
			d1, d2 := c.Discount(q.Tenor), c.Discount(q.Tenor+q.Span)
			if got := (d1/d2 - 1) / q.Span; math.Abs(got-q.Value) > 1e-10 {
				t.Errorf("future %.2fy fwd: got %.8f want %.8f", q.Tenor, got, q.Value)
			}
		case Swap:
			if got := c.ParRate(annualCouponTimes(q.Tenor)); math.Abs(got-q.Value) > 1e-9 {
				t.Errorf("swap %.0fy par: got %.8f want %.8f", q.Tenor, got, q.Value)
			}
		}
	}
}

func TestCalibrate_Errors(t *testing.T) {
	cases := map[string][]RateQuote{
		"empty":            nil,
		"duplicate pillar": {{Kind: Deposit, Tenor: 1, Value: 0.03}, {Kind: Swap, Tenor: 1, Value: 0.03}},
		"future unanchored": {
			{Kind: Future, Tenor: 0.25, Value: 0.03},
		},
		"unsolvable swap": {{Kind: Swap, Tenor: 5, Value: 3.0}},
		"negative DF deposit": {
			{Kind: Deposit, Tenor: 1, Value: -1.5},
		},
	}
	for name, quotes := range cases {
		if _, err := Calibrate(quotes, LinearZero); !errors.Is(err, ErrCalibrate) {
			t.Errorf("%s: want ErrCalibrate, got %v", name, err)
		}
	}
}

// The store resolves the latest curve at or before as_of — never a later one.
func TestStore_PointInTimeResolution(t *testing.T) {
	s := NewStore()
	t0 := time.Date(2026, 7, 1, 16, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)
	c0, _ := NewZeroCurve([]float64{1}, []float64{0.03}, Continuous, LinearZero)
	c1, _ := NewZeroCurve([]float64{1}, []float64{0.04}, Continuous, LinearZero)
	// Out-of-order publish: intraday first, then the earlier nightly close.
	s.Put("USD", t1, c1)
	s.Put("USD", t0, c0)

	ctx := context.Background()
	if _, ok := s.Curve(ctx, "USD", t0.Add(-time.Minute)); ok {
		t.Error("resolved a curve before the first version")
	}
	if got, ok := s.Curve(ctx, "USD", t0.Add(time.Hour)); !ok || got != c0 {
		t.Error("as_of between versions must resolve the earlier curve")
	}
	if got, ok := s.Curve(ctx, "USD", t1.Add(time.Hour)); !ok || got != c1 {
		t.Error("as_of after the latest version must resolve it")
	}
	if _, ok := s.Curve(ctx, "EUR", t1); ok {
		t.Error("unknown currency must not resolve")
	}
	// Same-asOf Put replaces.
	s.Put("USD", t1, c0)
	if got, _ := s.Curve(ctx, "USD", t1); got != c0 {
		t.Error("Put at an existing asOf must replace the version")
	}
}

type staticQuotes struct{ qs []RateQuote }

func (s staticQuotes) RateQuotes(context.Context, string, time.Time) ([]RateQuote, error) {
	return s.qs, nil
}

type failingQuotes struct{}

func (failingQuotes) RateQuotes(context.Context, string, time.Time) ([]RateQuote, error) {
	return nil, errors.New("vendor down")
}

// Refresh publishes on success and leaves the store untouched on failure — the
// previous curve keeps serving.
func TestCalibrator_Refresh(t *testing.T) {
	ctx := context.Background()
	store := NewStore()
	asOf := time.Date(2026, 7, 1, 16, 0, 0, 0, time.UTC)
	cal := &Calibrator{
		Source: staticQuotes{qs: []RateQuote{{Kind: Deposit, Tenor: 0.5, Value: 0.03}, {Kind: Swap, Tenor: 5, Value: 0.04}}},
		Store:  store,
		Interp: LogLinearDF,
	}
	c, err := cal.Refresh(ctx, "USD", asOf)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got, ok := store.Curve(ctx, "USD", asOf); !ok || got != c {
		t.Fatal("Refresh must publish the calibrated curve at asOf")
	}

	bad := &Calibrator{Source: failingQuotes{}, Store: store, Interp: LogLinearDF}
	if _, err := bad.Refresh(ctx, "USD", asOf.Add(time.Hour)); err == nil {
		t.Fatal("Refresh must surface a source error")
	}
	if got, _ := store.Curve(ctx, "USD", asOf.Add(2*time.Hour)); got != c {
		t.Error("a failed Refresh must leave the previous curve serving")
	}
}

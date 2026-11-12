package curve

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestMixedCalibrationPreservesTinyQuotes(t *testing.T) {
	for _, interp := range []Interpolation{LinearZero, LogLinearDF} {
		for _, rate := range []float64{-1e-20, 0, 1e-20, 1e-100} {
			for _, qs := range [][]RateQuote{
				{{Kind: Deposit, Tenor: 1, Value: rate}},
				{{Kind: Deposit, Tenor: 1, Value: rate}, {Kind: Future, Tenor: 1, Span: 1, Value: rate}},
				{{Kind: Swap, Tenor: 1, Value: rate}},
			} {
				c, err := Calibrate(qs, interp)
				if err != nil {
					t.Fatalf("%v %g: %v", qs, rate, err)
				}
				// All three examples have exact DF(n)=(1+rate)^(-n).
				want := math.Log1p(rate)
				got := c.Zero(qs[len(qs)-1].end())
				if !finite(got) || math.Abs(got-want) > 64*0x1p-52*math.Abs(want) {
					t.Errorf("interp=%d quotes=%v zero=%.17g want %.17g", interp, qs, got, want)
				}
			}
		}
	}
}

func TestMixedCalibrationSolvesFutureAcrossPillarGap(t *testing.T) {
	// DF(.25)=400/403 and DF(start)/DF(end)=807/800. Substitution
	// into the declared interpolation gives these independent rational checks.
	for _, interp := range []Interpolation{LinearZero, LogLinearDF} {
		qs := []RateQuote{{Kind: Deposit, Tenor: .25, Value: .03}, {Kind: Future, Tenor: 1, Span: .25, Value: .035}}
		c, err := Calibrate(qs, interp)
		if err != nil {
			t.Fatalf("interpolation %d: %v", interp, err)
		}
		for _, q := range qs {
			if got := c.QuoteRate(q); !finite(got) || math.Abs(got-q.Value) > 128*0x1p-52*math.Abs(q.Value) {
				t.Errorf("quote did not reprice: %.17g vs %.17g", got, q.Value)
			}
		}
		df := c.Discount(1.25)
		want := (400.0 / 403) * math.Pow(800.0/807, 4)
		got := df
		if interp == LinearZero {
			got, want = df*df, math.Pow(320000.0/325221, 5)
		}
		if !finite(got) || math.Abs(got-want) > 32*0x1p-52*want {
			t.Errorf("rational gap discount: %.17g vs %.17g", got, want)
		}
	}
}

func TestMixedCalibrationExhaustionReturnsError(t *testing.T) {
	b := &Curve{interp: LinearZero}
	q := RateQuote{Kind: Swap, Tenor: 3.5, Value: .03}
	for _, budget := range []int{0, 1, 5} {
		if _, err := solveSwapPillarWithBudget(b, q, budget); err == nil {
			t.Fatalf("exhausted %d evaluations returned a candidate", budget)
		}
	}
	if _, err := solveSwapPillarWithBudget(b, q, maxCalibrationEvaluations); err != nil {
		t.Fatal(err)
	}
}

func TestMixedGapCalibrationRecordsThenInstallsAndCaptures(t *testing.T) {
	at := time.Unix(100, 0)
	for _, interp := range []Interpolation{LinearZero, LogLinearDF} {
		store := NewStore()
		prior, _ := NewZeroCurve([]float64{1}, []float64{.03}, Continuous, interp)
		store.Put("USD", at, prior)
		qs := []RateQuote{{Kind: Deposit, Tenor: .25, Value: .03}, {Kind: Future, Tenor: 1, Span: .25, Value: .035}}
		recorded, captured := false, false
		cal := Calibrator{Store: store, Interp: interp, Source: capturedSource{strip: Strip{Quotes: qs, Capture: &InputCapture{CapturedAt: at}}},
			OnCalibrated: func(ctx context.Context, currency string, timestamp time.Time, c *Curve) error {
				if installed, ok := store.Curve(ctx, currency, timestamp); !ok || installed != prior {
					t.Fatal("installed before recording")
				}
				for _, q := range qs {
					if !quoteResidualFits(c.QuoteRate(q), q.Value) {
						t.Fatal("recording an unrepriced input")
					}
				}
				recorded = true
				return nil
			},
			OnCaptured: func(c CalibrationCapture) {
				installed, ok := store.Curve(context.Background(), "USD", at.Add(time.Hour))
				if !recorded || !ok || installed == prior || len(c.Tenors) != 2 || c.Tenors[1] != 1.25 || c.Zeros[1] != installed.Zero(1.25) {
					t.Fatal("capture preceded install or lost extended curve")
				}
				captured = true
			},
		}
		if c, err := cal.Refresh(context.Background(), "USD", at.Add(time.Hour)); err != nil || c == nil || !recorded || !captured {
			t.Fatalf("gap refresh: curve=%v err=%v record=%v capture=%v", c, err, recorded, captured)
		}
	}
}

func TestMixedCalibrationRefusesFutureWithRoundedAwayPeriod(t *testing.T) {
	q := RateQuote{Kind: Future, Tenor: 1e16, Span: .25, Value: .03}
	c, _ := NewZeroCurve([]float64{.25}, []float64{.03}, Continuous, LinearZero)
	if !math.IsNaN(c.QuoteRate(q)) {
		t.Fatal("non-increasing future period returned a rate")
	}
	if result, err := Calibrate([]RateQuote{{Kind: Deposit, Tenor: .25, Value: .03}, q}, LinearZero); err == nil || result != nil {
		t.Fatal("rounded-away period returned a calibrated curve")
	}
}

func TestMixedCalibrationResidualRefusalPreservesPriorState(t *testing.T) {
	at := time.Unix(100, 0)
	store := NewStore()
	prior, _ := NewZeroCurve([]float64{1}, []float64{.03}, Continuous, LinearZero)
	store.Put("USD", at, prior)
	// This binary64 curve cannot retain the tiny forward on top of its much
	// larger accumulated log-discount. Refusal must precede every write/capture.
	qs := []RateQuote{{Kind: Deposit, Tenor: 1, Value: .03}, {Kind: Future, Tenor: 1, Span: 1, Value: 1e-100}}
	written, captured, coverage := false, false, false
	cal := Calibrator{Store: store, Source: capturedSource{strip: Strip{Quotes: qs, Capture: &InputCapture{CapturedAt: at}}},
		OnCalibrated: func(context.Context, string, time.Time, *Curve) error { written = true; return nil },
		OnCaptured:   func(CalibrationCapture) { captured = true },
		OnCoverage:   func(string, StripCoverage) { coverage = true },
	}
	if c, err := cal.Refresh(context.Background(), "USD", at.Add(time.Hour)); err == nil || c != nil || written || captured || !coverage {
		t.Fatalf("residual refusal lost state boundary: curve=%v err=%v write=%v capture=%v coverage=%v", c, err, written, captured, coverage)
	}
	if c, ok := store.Curve(context.Background(), "USD", at.Add(time.Hour)); !ok || c != prior {
		t.Fatal("refused calibration replaced prior PIT curve")
	}
}

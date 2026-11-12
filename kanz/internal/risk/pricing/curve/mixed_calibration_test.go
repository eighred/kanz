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

func TestMixedCalibrationRejectsUnfixedFutureStart(t *testing.T) {
	// The old closed form extrapolates DF(start) before adding a pillar that
	// changes that same start. A returned curve then fails its own future quote.
	for _, interp := range []Interpolation{LinearZero, LogLinearDF} {
		if c, err := Calibrate([]RateQuote{{Kind: Deposit, Tenor: .25, Value: .03}, {Kind: Future, Tenor: 1, Span: .25, Value: .035}}, interp); err == nil || c != nil {
			t.Errorf("unfixed future start returned a calibrated curve: %v", c)
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

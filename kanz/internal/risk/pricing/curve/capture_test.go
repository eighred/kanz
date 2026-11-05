package curve

import (
	"context"
	"errors"
	"testing"
	"time"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

type capturedSource struct{ strip Strip }

func (s capturedSource) RateQuotes(context.Context, string, time.Time) (Strip, error) {
	return s.strip, nil
}

func TestCapturedObserverRunsAfterInstallWithDetachedData(t *testing.T) {
	at := time.Unix(100, 0)
	value := .05
	src := capturedSource{Strip{Quotes: []RateQuote{{Kind: Deposit, Tenor: 1, Value: value}}, Coverage: StripCoverage{Configured: 2, Quoted: 1, Missing: []MissingQuote{{InstrumentID: "missing"}}}, Capture: &InputCapture{CapturedAt: at, Inputs: []CapturedInput{{InstrumentID: "deposit", ResolvedRate: &value, Observation: &marketpb.MarketDataEvent{InstrumentId: "deposit"}}}}}}
	store := NewStore()
	called := false
	cal := Calibrator{Source: src, Store: store, Interp: LinearZero, OnCaptured: func(c CalibrationCapture) {
		called = true
		if _, ok := store.Curve(context.Background(), "USD", at); !ok {
			t.Fatal("observer ran before authoritative install")
		}
		c.Tenors[0] = 99
		c.Zeros[0] = 99
		c.Strip.Quotes[0].Value = 99
		c.Strip.Coverage.Missing[0].InstrumentID = "changed"
		*c.Strip.Capture.Inputs[0].ResolvedRate = 99
		c.Strip.Capture.Inputs[0].Observation.InstrumentId = "changed"
	}}
	if _, err := cal.Refresh(context.Background(), "USD", at); err != nil {
		t.Fatal(err)
	}
	installed, _ := store.Curve(context.Background(), "USD", at)
	if !called || installed.Tenors()[0] != 1 || installed.Zeros()[0] > 1 || src.strip.Quotes[0].Value != .05 || value != .05 || src.strip.Capture.Inputs[0].Observation.InstrumentId != "deposit" || src.strip.Coverage.Missing[0].InstrumentID != "missing" {
		t.Fatal("observer alias changed authoritative input/curve")
	}
	called = false
	cal.OnCalibrated = func(context.Context, string, time.Time, *Curve) error { return errors.New("record refused") }
	if _, err := cal.Refresh(context.Background(), "USD", at.Add(time.Hour)); err == nil || called {
		t.Fatal("failed authoritative recording reached shadow observer")
	}
	cal.OnCalibrated = nil
	cal.Source = capturedSource{Strip{Quotes: []RateQuote{{Kind: Deposit, Tenor: 1, Value: .05}}}}
	if _, err := cal.Refresh(context.Background(), "USD", at.Add(2*time.Hour)); err != nil || called {
		t.Fatal("source without capture fabricated an observation")
	}
}

package livequote

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCalibrationCaptureOwnsOneLatestCut(t *testing.T) {
	inputs := []RateInstrument{{InstrumentID: "deposit", Currency: "USD", Kind: curve.Deposit, Tenor: 1}, {InstrumentID: "missing", Currency: "USD", Kind: curve.Swap, Tenor: 2}}
	cache := New(inputs)
	ev := quoteEvent("deposit", decv(4, -2), decv(6, -2))
	ev.EventTime = timestamppb.New(time.Unix(200, 0))
	ev.SourceSequence = 17
	cache.Update(ev)
	ev.GetQuote().BidPrice.Coefficient = 99 // Update must own its observation.
	source := NewSnapshotRateSource(cache, inputs)
	strip, err := source.RateQuotes(context.Background(), "USD", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if strip.Capture == nil || strip.Capture.CapturedAt.IsZero() || len(strip.Capture.Inputs) != 2 {
		t.Fatal("missing capture")
	}
	in := strip.Capture.Inputs[0]
	if in.Observation.SourceSequence != 17 || in.Observation.EventTime.AsTime().Unix() != 200 || *in.ResolvedRate != strip.Quotes[0].Value || strip.Quotes[0].Value != .05 {
		t.Fatal("solver input/provenance drift")
	}
	if strip.Capture.Inputs[1].Observation != nil || strip.Capture.Inputs[1].ResolvedRate != nil || strip.Coverage.Quoted != 1 {
		t.Fatal("missing input fabricated")
	}
	cache.Update(quoteEvent("deposit", decv(8, -2), decv(8, -2)))
	if in.Observation.GetQuote().BidPrice.Coefficient != 4 {
		t.Fatal("later cache update changed capture")
	}
	in.Observation.GetQuote().BidPrice.Coefficient = 55
	latest, _ := cache.Latest("deposit")
	latest.GetQuote().BidPrice.Coefficient = 66
	next, _ := source.RateQuotes(context.Background(), "USD", time.Now())
	if next.Quotes[0].Value != .08 {
		t.Fatal("capture/latest caller mutated cache")
	}
}

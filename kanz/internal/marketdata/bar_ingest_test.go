package marketdata

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

var barTime = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

func barEvent(mic string, open, close time.Time) *marketpb.MarketDataEvent {
	return &marketpb.MarketDataEvent{
		InstrumentId: "BTC-USDT",
		Mic:          mic,
		EventTime:    timestamppb.New(close),
		Data: &marketpb.MarketDataEvent_Bar{Bar: &marketpb.Bar{
			OpenTime:   timestamppb.New(open),
			CloseTime:  timestamppb.New(close),
			Open:       &commonpb.Decimal{Coefficient: 10000, Exponent: -2},
			High:       &commonpb.Decimal{Coefficient: 11000, Exponent: -2},
			Low:        &commonpb.Decimal{Coefficient: 9000, Exponent: -2},
			Close:      &commonpb.Decimal{Coefficient: 10500, Exponent: -2},
			Volume:     &commonpb.Decimal{Coefficient: 15, Exponent: -1},
			TradeCount: 42,
		}},
	}
}

func barEnvelope() *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventType:     "market.bar",
		IngestionTime: timestamppb.New(barTime.Add(time.Minute)),
	}
}

// THE DEFECT, IN ONE TEST (#425).
//
// market.v1.Bar has always carried open, high, low, close, volume and
// trade_count. Ingest kept the close and dropped the rest — and nothing anywhere
// reported a loss, because the series simply did not exist and so no query
// missed it. This asserts every field survives the fold.
func TestHandlerKeepsTheWholeCandle(t *testing.T) {
	fw := &fakeWriter{}
	ing, err := NewIngestor(fw)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := proto.Marshal(barEvent("XBIN", barTime, barTime.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ing.Handler(context.Background(), barEnvelope(), payload); err != nil {
		t.Fatalf("Handler: %v", err)
	}

	if len(fw.bars) != 1 {
		t.Fatalf("bars written = %d, want 1 — the candle was discarded, exactly as before", len(fw.bars))
	}
	b := fw.bars[0]
	for _, f := range []struct {
		name string
		got  *commonpb.Decimal
		want int64
	}{
		{"open", b.Open, 10000}, {"high", b.High, 11000},
		{"low", b.Low, 9000}, {"close", b.Close, 10500},
	} {
		if f.got.GetCoefficient() != f.want {
			t.Errorf("%s = %v, want coefficient %d", f.name, f.got, f.want)
		}
	}
	if b.Volume.GetCoefficient() != 15 {
		t.Errorf("volume = %v, want 1.5", b.Volume)
	}
	if b.TradeCount != 42 {
		t.Errorf("trade_count = %d, want 42", b.TradeCount)
	}
	if b.Venue != "XBIN" {
		t.Errorf("venue = %q, want XBIN — taken from the event's mic", b.Venue)
	}
	if b.Resolution != store.Resolution1m {
		t.Errorf("resolution = %q, want 1m — derived from the bar's own interval", b.Resolution)
	}

	// AND THE CLOSE IS STILL A MARK. Both writes happen: the scalar series is
	// what risk and pricing already read, and breaking it to add candles would
	// trade one defect for a worse one.
	if len(fw.got) != 1 {
		t.Fatalf("observations written = %d, want 1 — the existing price series stopped being fed",
			len(fw.got))
	}
	if fw.got[0].Price.GetCoefficient() != 10500 {
		t.Errorf("mark = %v, want the bar's close", fw.got[0].Price)
	}
}

// AN INTERVAL THIS PLATFORM DOES NOT STORE IS REFUSED, not filed under an
// invented resolution. A 47-second candle is a defect in whatever produced it,
// and keeping it quietly creates a series nothing queries and nobody knows
// exists. A non-nil return nacks the delivery, so it surfaces to an operator.
func TestHandlerRefusesAnUnknownBarInterval(t *testing.T) {
	fw := &fakeWriter{}
	ing, _ := NewIngestor(fw)
	payload, _ := proto.Marshal(barEvent("XBIN", barTime, barTime.Add(47*time.Second)))

	if err := ing.Handler(context.Background(), barEnvelope(), payload); err == nil {
		t.Fatal("a 47-second bar was accepted")
	}
	if len(fw.bars) != 0 || len(fw.got) != 0 {
		t.Errorf("a refused bar still wrote: %d bars, %d observations", len(fw.bars), len(fw.got))
	}
}

// A CANDLE WITH NO VENUE IS REFUSED. It cannot be matched to the book an order
// would execute against, so it is a price the platform cannot honestly trade on.
func TestHandlerRefusesABarWithNoVenue(t *testing.T) {
	fw := &fakeWriter{}
	ing, _ := NewIngestor(fw)
	payload, _ := proto.Marshal(barEvent("", barTime, barTime.Add(time.Minute)))

	if err := ing.Handler(context.Background(), barEnvelope(), payload); err == nil {
		t.Fatal("a bar with no mic was accepted")
	}
	if len(fw.bars) != 0 {
		t.Error("a venue-less bar was written")
	}
}

// A TRADE IS NOT A CANDLE. Non-bar events must keep feeding the scalar series
// and write no bar — otherwise every tick would forge a one-tick candle.
func TestHandlerWritesNoBarForATrade(t *testing.T) {
	fw := &fakeWriter{}
	ing, _ := NewIngestor(fw)
	payload, _ := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: "BTC-USDT",
		Mic:          "XBIN",
		EventTime:    timestamppb.New(barTime),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
			Price: &commonpb.Decimal{Coefficient: 10500, Exponent: -2},
			Size:  &commonpb.Decimal{Coefficient: 1, Exponent: 0},
		}},
	})

	if err := ing.Handler(context.Background(), barEnvelope(), payload); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if len(fw.bars) != 0 {
		t.Fatalf("a trade produced %d bar(s) — every tick would forge a one-tick candle", len(fw.bars))
	}
	if len(fw.got) != 1 {
		t.Fatalf("observations = %d, want 1", len(fw.got))
	}
}

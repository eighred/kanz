package marketdata

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

// AN UNSTAMPED ENVELOPE IS REFUSED, FOR BOTH PATHS (#427).
//
// knowledge_time used to fall back to the observation's OWN time — the bar's
// close, or the event time for a trade. That is not a missing value being filled
// in; it is the maximally optimistic CLAIM available: that Kanz knew the price
// the instant the market produced it. Every as-of read then treats a late
// arrival, or a correction, as though it had been knowable live — the one
// direction a backtest is never audited in.
//
// Both paths are tested because both had to change together: fixing one would
// leave price_observations and ohlcv_bars disagreeing about what an unstamped
// envelope means, which is worse than one consistent wrong answer.

func unstampedEnvelope() *envelopepb.Envelope {
	// Neither ingestion_time nor publish_time. envelope.v1 marks both Required
	// and pkg/bus stamps publish_time on every event it sends, so this shape is
	// hand-built or malformed — exactly what must stop.
	return &envelopepb.Envelope{EventType: "market.crypto.bar"}
}

func TestUnstampedEnvelopeIsRefusedForABar(t *testing.T) {
	at := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	ev := &marketpb.MarketDataEvent{
		InstrumentId: "BTC-USDT", Mic: "XBIN", EventTime: timestamppb.New(at.Add(time.Minute)),
		Data: &marketpb.MarketDataEvent_Bar{Bar: &marketpb.Bar{
			OpenTime:  timestamppb.New(at),
			CloseTime: timestamppb.New(at.Add(time.Minute)),
			Open:      &commonpb.Decimal{Coefficient: 100, Exponent: 0},
			High:      &commonpb.Decimal{Coefficient: 100, Exponent: 0},
			Low:       &commonpb.Decimal{Coefficient: 100, Exponent: 0},
			Close:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
			Volume:    &commonpb.Decimal{Coefficient: 1, Exponent: 0},
		}},
	}

	_, _, err := TranslateBar(unstampedEnvelope(), ev)
	if err == nil {
		t.Fatal("a bar with no envelope stamp was accepted. It is now recorded as having been known " +
			"the instant the candle closed, and every point-in-time read believes that.")
	}
	for _, want := range []string{"ingestion_time", "publish_time"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

func TestUnstampedEnvelopeIsRefusedForAnObservation(t *testing.T) {
	ev := &marketpb.MarketDataEvent{
		InstrumentId: "BTC-USDT", Mic: "XBIN",
		EventTime: timestamppb.New(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
			Price: &commonpb.Decimal{Coefficient: 100, Exponent: 0},
		}},
	}

	if _, err := TranslateEvent(unstampedEnvelope(), ev); err == nil {
		t.Fatal("a trade with no envelope stamp was accepted into the price series with a " +
			"knowledge_time it invented")
	}
}

// THE HANDLER NACKS IT, so it reaches a DLQ rather than being dropped. Market
// data loss must be loud — and an envelope this malformed is a defect in
// whatever produced it, not a transient.
func TestUnstampedEnvelopeNacksTheDelivery(t *testing.T) {
	fw := &fakeWriter{}
	ing, err := NewIngestor(fw)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: "BTC-USDT", Mic: "XBIN", EventTime: timestamppb.New(at),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
			Price: &commonpb.Decimal{Coefficient: 100, Exponent: 0},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := ing.Handler(context.Background(), unstampedEnvelope(), payload); err == nil {
		t.Fatal("the handler accepted an unstamped envelope")
	}
	if len(fw.got) != 0 || len(fw.bars) != 0 {
		t.Errorf("a refused envelope still wrote: %d observations, %d bars", len(fw.got), len(fw.bars))
	}
}

// EITHER STAMP IS ENOUGH, and ingestion_time wins. It is when Kanz first
// received the event at the edge; publish_time is later by however long the
// internal pipeline took, so preferring it would overstate the delay before the
// platform knew.
func TestEitherStampIsAcceptedAndIngestionWins(t *testing.T) {
	at := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	ingested := at.Add(2 * time.Second)
	published := at.Add(9 * time.Second)
	ev := &marketpb.MarketDataEvent{
		InstrumentId: "BTC-USDT", Mic: "XBIN", EventTime: timestamppb.New(at),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
			Price: &commonpb.Decimal{Coefficient: 100, Exponent: 0},
		}},
	}

	both, err := TranslateEvent(&envelopepb.Envelope{
		EventType:     "market.crypto.trade",
		IngestionTime: timestamppb.New(ingested),
		PublishTime:   timestamppb.New(published),
	}, ev)
	if err != nil {
		t.Fatalf("TranslateEvent: %v", err)
	}
	if !both.KnowledgeTime.Equal(ingested) {
		t.Errorf("knowledge_time = %s, want the ingestion time %s — publish_time is later by the "+
			"internal pipeline's latency", both.KnowledgeTime, ingested)
	}

	publishOnly, err := TranslateEvent(&envelopepb.Envelope{
		EventType:   "market.crypto.trade",
		PublishTime: timestamppb.New(published),
	}, ev)
	if err != nil {
		t.Fatalf("an envelope with only publish_time was refused: %v", err)
	}
	if !publishOnly.KnowledgeTime.Equal(published) {
		t.Errorf("knowledge_time = %s, want %s", publishOnly.KnowledgeTime, published)
	}
}

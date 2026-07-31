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

var (
	evtTime = time.Date(2026, 3, 2, 21, 0, 0, 0, time.UTC)
	ingTime = time.Date(2026, 3, 2, 21, 0, 5, 0, time.UTC) // Kanz learned it 5s later
)

func env(eventType string) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventType:     eventType,
		PartitionKey:  "AAPL",
		EventTime:     timestamppb.New(evtTime),
		IngestionTime: timestamppb.New(ingTime),
	}
}

func decv(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

func TestTranslateEvent_Bar(t *testing.T) {
	closeTime := time.Date(2026, 3, 2, 21, 0, 0, 0, time.UTC)
	ev := &marketpb.MarketDataEvent{
		InstrumentId: "AAPL",
		EventTime:    timestamppb.New(evtTime),
		Data: &marketpb.MarketDataEvent_Bar{Bar: &marketpb.Bar{
			CloseTime: timestamppb.New(closeTime),
			Close:     decv(15012, -2), // 150.12
		}},
	}
	o, err := TranslateEvent(env("market.equity.bar"), ev)
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != store.PriceKindClose {
		t.Fatalf("bar → CLOSE, got %v", o.Kind)
	}
	if o.Price.Coefficient != 15012 {
		t.Fatalf("want bar close 150.12, got %+v", o.Price)
	}
	if !o.ObservationTime.Equal(closeTime) {
		t.Fatalf("observation_time should be the bar close_time, got %v", o.ObservationTime)
	}
	// knowledge_time is the envelope ingestion_time — the bitemporal axis.
	if !o.KnowledgeTime.Equal(ingTime) {
		t.Fatalf("knowledge_time should be ingestion_time %v, got %v", ingTime, o.KnowledgeTime)
	}
}

func TestTranslateEvent_Trade(t *testing.T) {
	ev := &marketpb.MarketDataEvent{
		InstrumentId: "AAPL",
		EventTime:    timestamppb.New(evtTime),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decv(15000, -2)}},
	}
	o, err := TranslateEvent(env("market.equity.trade"), ev)
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != store.PriceKindLast || o.Price.Coefficient != 15000 {
		t.Fatalf("trade → LAST 150.00, got kind=%v price=%+v", o.Kind, o.Price)
	}
	if !o.ObservationTime.Equal(evtTime) {
		t.Fatalf("trade observation_time should be event_time, got %v", o.ObservationTime)
	}
}

func TestTranslateEvent_QuoteMid(t *testing.T) {
	ev := &marketpb.MarketDataEvent{
		InstrumentId: "AAPL",
		EventTime:    timestamppb.New(evtTime),
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
			BidPrice: decv(15000, -2), // 150.00
			AskPrice: decv(15010, -2), // 150.10  → mid 150.05
		}},
	}
	o, err := TranslateEvent(env("market.equity.quote"), ev)
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != store.PriceKindMid {
		t.Fatalf("quote → MID, got %v", o.Kind)
	}
	// mid = (15000 + 15010)/2 = 15005 at 1e-2 → stored as 150050 at 1e-3 exactly.
	if got := float64(o.Price.Coefficient) * pow10f(o.Price.Exponent); got != 150.05 {
		t.Fatalf("want mid 150.05, got %v (%+v)", got, o.Price)
	}
}

func TestTranslateEvent_Errors(t *testing.T) {
	// No oneof variant set.
	if _, err := TranslateEvent(env("market.equity.trade"), &marketpb.MarketDataEvent{InstrumentId: "AAPL"}); err == nil {
		t.Error("want error for missing market data variant")
	}
	// Variant set but price nil.
	ev := &marketpb.MarketDataEvent{
		InstrumentId: "AAPL",
		EventTime:    timestamppb.New(evtTime),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{}},
	}
	if _, err := TranslateEvent(env("market.equity.trade"), ev); err == nil {
		t.Error("want error for nil price")
	}
}

func TestTranslateEvent_InstrumentFallbackToPartitionKey(t *testing.T) {
	ev := &marketpb.MarketDataEvent{ // no InstrumentId
		EventTime: timestamppb.New(evtTime),
		Data:      &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decv(1, 0)}},
	}
	o, err := TranslateEvent(env("market.equity.trade"), ev)
	if err != nil {
		t.Fatal(err)
	}
	if o.InstrumentID != "AAPL" {
		t.Fatalf("instrument should fall back to partition_key, got %q", o.InstrumentID)
	}
}

// fakeWriter captures Put calls.
type fakeWriter struct{ got []store.Observation }

func (f *fakeWriter) Put(_ context.Context, obs []store.Observation) error {
	f.got = append(f.got, obs...)
	return nil
}

func TestIngestor_Handler_WritesObservation(t *testing.T) {
	fw := &fakeWriter{}
	ing, err := NewIngestor(fw)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: "AAPL",
		EventTime:    timestamppb.New(evtTime),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decv(15000, -2)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ing.Handler(context.Background(), env("market.equity.trade"), payload); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if len(fw.got) != 1 || fw.got[0].InstrumentID != "AAPL" {
		t.Fatalf("want one AAPL observation written, got %+v", fw.got)
	}
}

func TestIngestor_Handler_BadPayload(t *testing.T) {
	ing, _ := NewIngestor(&fakeWriter{})
	if err := ing.Handler(context.Background(), env("market.equity.trade"), []byte("not-proto")); err == nil {
		t.Error("want unmarshal error surfaced (→ DLQ), got nil")
	}
}

func pow10f(exp int32) float64 {
	out := 1.0
	for ; exp < 0; exp++ {
		out /= 10
	}
	for ; exp > 0; exp-- {
		out *= 10
	}
	return out
}

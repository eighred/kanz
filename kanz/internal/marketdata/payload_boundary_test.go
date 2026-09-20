package marketdata

import (
	"context"
	"testing"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHandlerBookSnapshotDoesNotWriteOrNack(t *testing.T) {
	for _, twoSided := range []bool{false, true} {
		book := &marketpb.OrderBookSnapshot{
			InstrumentId: "AAPL", EventTime: timestamppb.New(evtTime),
			Bids: []*marketpb.PriceLevel{{Price: decv(100, 0)}, {Price: decv(90, 0)}},
		}
		if twoSided {
			book.Asks = []*marketpb.PriceLevel{{Price: decv(110, 0)}}
		}
		payload, err := proto.Marshal(book)
		if err != nil {
			t.Fatal(err)
		}
		writer := &fakeWriter{}
		ing, _ := NewIngestor(writer)
		if err := ing.Handler(context.Background(), env("market.book.snapshot"), payload); err != nil {
			t.Errorf("two-sided=%v: snapshot must be acknowledged, got %v", twoSided, err)
		}
		if len(writer.got)+len(writer.bars)+len(writer.cov) != 0 {
			t.Errorf("two-sided=%v: snapshot polluted durable market history: %+v", twoSided, writer.got)
		}
	}
}

func TestHandlerRefusesUnknownTypeAndMismatchedVariant(t *testing.T) {
	trade := &marketpb.MarketDataEvent{
		InstrumentId: "AAPL", EventTime: timestamppb.New(evtTime),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decv(100, 0)}},
	}
	payload, err := proto.Marshal(trade)
	if err != nil {
		t.Fatal(err)
	}
	for _, eventType := range []string{"market.crypto.new_variant", "market.equity.quote", "market.equity.bar", "market..trade", "market.trade", ""} {
		t.Run(eventType, func(t *testing.T) {
			writer := &fakeWriter{}
			ing, _ := NewIngestor(writer)
			if err := ing.Handler(context.Background(), env(eventType), payload); err == nil {
				t.Fatal("expected refusal for DLQ, got successful delivery")
			}
			if len(writer.got)+len(writer.bars)+len(writer.cov) != 0 {
				t.Fatal("refused payload reached store")
			}
		})
	}
}

func TestHandlerKnownNonPriceIsNotDecoded(t *testing.T) {
	for _, eventType := range []string{"market.book.snapshot", "market.crypto.volume_profile"} {
		writer := &fakeWriter{}
		ing, _ := NewIngestor(writer)
		if err := ing.Handler(context.Background(), env(eventType), []byte("not a MarketDataEvent")); err != nil {
			t.Errorf("%s decoded on the price path: %v", eventType, err)
		}
		if len(writer.got)+len(writer.bars)+len(writer.cov) != 0 {
			t.Fatal("non-price payload reached store")
		}
	}
}

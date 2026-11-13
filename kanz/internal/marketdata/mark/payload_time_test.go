package mark_test

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/mark"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPayloadTimestampMalformedPreservesPriceAndTouch(t *testing.T) {
	for name, stamp := range map[string]*timestamppb.Timestamp{
		"negative nanos":  {Seconds: base.Unix() + 1, Nanos: -1_000_000_000},
		"negative one":    {Seconds: base.Unix(), Nanos: -1},
		"overflow nanos":  {Seconds: base.Unix() - 1, Nanos: 1_000_000_000},
		"before year one": {Seconds: -62135596801},
		"after year 9999": {Seconds: 253402300800},
	} {
		for _, typ := range []string{"trade", "quote"} {
			t.Run(name+"/"+typ, func(t *testing.T) {
				s := mark.New(func() time.Time { return base }, time.Minute)
				foldQuote(t, s, "TIME", dec(100, 0), dec(102, 0), base)
				ev := &marketpb.MarketDataEvent{InstrumentId: "TIME", EventTime: stamp}
				if typ == "trade" {
					ev.Data = &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: dec(900, 0)}}
				} else {
					ev.Data = &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: dec(900, 0), AskPrice: dec(902, 0)}}
				}
				payload, err := proto.Marshal(ev)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Handle(context.Background(), env("market.crypto."+typ), payload); err != nil {
					t.Fatalf("malformed monitor message must remain an ack/no-op: %v", err)
				}
				price, at, seen := s.Lookup("TIME")
				if !seen || price == nil || price.FloatString(0) != "101" || !at.Equal(base) {
					t.Fatalf("malformed timestamp mutated prior price: %v %v %v", price, at, seen)
				}
				bid, ask, at, ok := s.Touch("TIME")
				if !ok || bid.FloatString(0) != "100" || ask.FloatString(0) != "102" || !at.Equal(base) {
					t.Fatalf("malformed timestamp mutated prior touch: %v %v %v %v", bid, ask, at, ok)
				}
			})
		}
	}
}

func TestPayloadTimestampValidPrecisionAndNilFallback(t *testing.T) {
	for _, at := range []time.Time{
		time.Unix(base.Unix(), 0).UTC(),
		time.Unix(base.Unix(), 1).UTC(),
		time.Unix(base.Unix(), 999999999).UTC(),
		time.Unix(-62135596800, 1).UTC(),
		time.Unix(253402300799, 999999999).UTC(),
	} {
		t.Run(at.Format(time.RFC3339Nano), func(t *testing.T) {
			s := mark.New(func() time.Time { return at }, time.Minute)
			foldQuote(t, s, "TIME", dec(100, 0), dec(102, 0), at)
			price, got, seen := s.Lookup("TIME")
			if !seen || price == nil || price.FloatString(0) != "101" || !got.Equal(at) {
				t.Fatalf("valid price instant changed: %v %v %v", price, got, seen)
			}
			_, _, got, ok := s.Touch("TIME")
			if !ok || !got.Equal(at) {
				t.Fatal("valid touch timestamp changed")
			}
		})
	}
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEventNoTime(t, "TIME", dec(101, 0))); err != nil {
		t.Fatal(err)
	}
	price, at, seen := s.Lookup("TIME")
	if !seen || price == nil || price.FloatString(0) != "101" || !at.Equal(base) {
		t.Fatal("nil payload timestamp lost its valid envelope fallback")
	}
}

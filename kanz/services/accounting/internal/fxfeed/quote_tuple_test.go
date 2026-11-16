package fxfeed

import (
	"math/big"
	"reflect"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func unusableQuoteTuples() []struct {
	name     string
	bid, ask *commonpb.Decimal
} {
	return []struct {
		name     string
		bid, ask *commonpb.Decimal
	}{
		{"negative-bid", decimal(-100, 0), decimal(110, 0)},
		{"zero-bid", decimal(0, 0), decimal(2, 0)},
		{"negative-ask", decimal(110, 0), decimal(-100, 0)},
		{"zero-ask", decimal(2, 0), decimal(0, 0)},
		{"bid-exponent-high", decimal(1, 65), decimal(2, 0)},
		{"bid-exponent-low", decimal(1, -65), decimal(2, 0)},
		{"ask-exponent-high", decimal(1, 0), decimal(2, 65)},
		{"ask-exponent-low", decimal(1, 0), decimal(2, -65)},
		{"crossed", decimal(2, 0), decimal(1, 0)},
		{"both-absent", nil, nil},
		{"only-zero-bid", &commonpb.Decimal{}, nil},
		{"only-zero-ask", nil, &commonpb.Decimal{}},
		{"only-negative-bid", decimal(-1, 0), nil},
		{"only-negative-ask", nil, decimal(-1, 0)},
	}
}

func TestLiveFXQuoteTupleRefusalHasNoStateEffects(t *testing.T) {
	for _, bad := range unusableQuoteTuples() {
		for _, state := range []string{"cold", "warm-future", "warm-same-time", "conflict-same-time", "conflict-future"} {
			t.Run(bad.name+"-"+state, func(t *testing.T) {
				live := New("USD", map[string]string{"EURUSD": "EUR"})
				if state != "cold" {
					if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime, 120); err != nil {
						t.Fatal(err)
					}
				}
				if state == "conflict-same-time" || state == "conflict-future" {
					if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime, 125); err == nil {
						t.Fatal("fixture did not establish a real same-time conflict")
					}
				}
				before, existed := live.rates["EUR"]
				frozen := live.Converter()
				frozenRate, frozenKnown := frozen.Rate("EUR")
				at := fxFixtureTime
				if state == "cold" || state == "warm-future" || state == "conflict-future" {
					at = at.Add(time.Hour)
				}
				if err := live.Handler(t.Context(), &envelopepb.Envelope{EventType: "market.fx.quote"}, fxQuote("EURUSD", bad.bid, bad.ask, at)); err != nil {
					t.Fatalf("unusable tuple must retain the production nil/ACK disposition: %v", err)
				}
				after, exists := live.rates["EUR"]
				if existed != exists || !reflect.DeepEqual(before, after) {
					t.Fatalf("unusable tuple created/changed rate, conflict or watermark: before=%+v after=%+v", before, after)
				}
				if err := deliverTimeQuote(t, live, "EURUSD", fxFixtureTime.Add(time.Second), 130); err != nil {
					t.Fatal(err)
				}
				requireTimeRate(t, live, 130) // valid repair is older than the ignored future tuple
				if rate, known := frozen.Rate("EUR"); known != frozenKnown || (known && rate.Cmp(frozenRate) != 0) {
					t.Fatal("tuple refusal or repair changed an issued converter")
				}
				if rate, known := live.Converter().Rate("USD"); !known || rate.Cmp(big.NewRat(1, 1)) != 0 {
					t.Fatal("reporting currency lost its identity rate")
				}
				if _, known := live.Converter().Rate("GBP"); known {
					t.Fatal("tuple handling invented an unrelated currency rate")
				}
			})
		}
	}
}

func TestLiveFXQuoteTupleExactValidAndAbsentSideCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, exact string
		bid, ask    *commonpb.Decimal
	}{
		{"two-sided", "6/5", decimal(119, -2), decimal(121, -2)},
		{"locked", "6/5", decimal(12, -1), decimal(120, -2)},
		{"absent-bid", "6/5", nil, decimal(120, -2)},
		{"absent-ask", "6/5", decimal(120, -2), nil},
		{"sub-float-spread", "1200000000000000001/1000000000000000000", decimal(1200000000000000000, -18), decimal(1200000000000000002, -18)},
		{"minimum-exponent", "1/10000000000000000000000000000000000000000000000000000000000000000", decimal(1, -64), decimal(1, -64)},
		{"maximum-exponent", "10000000000000000000000000000000000000000000000000000000000000000", decimal(1, 64), decimal(1, 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := New("USD", map[string]string{"EURUSD": "EUR"})
			if err := live.Handler(t.Context(), &envelopepb.Envelope{EventType: "market.fx.quote"}, fxQuote("EURUSD", tc.bid, tc.ask)); err != nil {
				t.Fatal(err)
			}
			want, ok := new(big.Rat).SetString(tc.exact)
			if !ok {
				t.Fatal("invalid independent exact-rate fixture")
			}
			got, known := live.Converter().Rate("EUR")
			if !known || got.Cmp(want) != 0 {
				t.Fatalf("exact quote rate=%v known=%v, want %s", got, known, want)
			}
		})
	}
}

func TestLiveFXQuoteTupleChangePreservesTradeBarAndUnconfiguredInputs(t *testing.T) {
	for _, variant := range []string{"trade", "bar"} {
		t.Run(variant, func(t *testing.T) {
			live := New("USD", map[string]string{"EURUSD": "EUR"})
			for i, price := range []int64{120, 0, -1, 130} {
				at := fxFixtureTime.Add(time.Duration(i) * time.Second)
				if price <= 0 {
					at = fxFixtureTime.Add(time.Hour)
				}
				event := &marketpb.MarketDataEvent{InstrumentId: "EURUSD", EventTime: timestamppb.New(at)}
				if variant == "trade" {
					event.Data = &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decimal(price, -2)}}
				} else {
					event.Data = &marketpb.MarketDataEvent_Bar{Bar: &marketpb.Bar{Close: decimal(price, -2)}}
				}
				payload, err := proto.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				if err := live.Handler(t.Context(), &envelopepb.Envelope{}, payload); err != nil {
					t.Fatal(err)
				}
				want := int64(120)
				if price == 130 {
					want = 130
				}
				requireTimeRate(t, live, want)
			}
			// Unconfigured events are ignored before timestamp and tuple checks.
			payload, err := proto.Marshal(&marketpb.MarketDataEvent{InstrumentId: "unconfigured"})
			if err != nil {
				t.Fatal(err)
			}
			if err := live.Handler(t.Context(), &envelopepb.Envelope{}, payload); err != nil {
				t.Fatal(err)
			}
			requireTimeRate(t, live, 130)
		})
	}
}

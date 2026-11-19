package spotsource_test

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/marketdata"
	"github.com/eighred/kanz/internal/marketdata/marketdatatest"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/risk/spotsource"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

// Use one instrument with distinct simultaneous marks. A missing kind filter
// can otherwise return a plausible price from the wrong series.
func TestPITPriceKindsThroughIngestionAndRisk(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" || os.Getenv("TEST_NATS_URL") == "" {
		t.Skip("requires TEST_POSTGRES_URL and TEST_NATS_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st := store.NewPostgres(marketdatatest.NewPool(t, ctx, dsn, filepath.Join("..", "..", "..", "services", "market_data", "migrations")))
	ingestor, err := marketdata.NewIngestor(st)
	if err != nil {
		t.Fatal(err)
	}
	broker := marketdatatest.NewBroker(t, ctx)
	observed := time.Date(2026, 10, 19, 12, 30, 0, 0, time.UTC)
	first := observed.Add(time.Second)
	revised := first.Add(time.Minute)
	instrument := broker.ID
	at := timestamppb.New(observed)
	dec := func(c int64, e int32) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: c, Exponent: e} }
	bar := &marketpb.MarketDataEvent{InstrumentId: instrument, Mic: "XBIN", EventTime: at, Data: &marketpb.MarketDataEvent_Bar{Bar: &marketpb.Bar{
		OpenTime: timestamppb.New(observed.Add(-time.Minute)), CloseTime: at,
		Open: dec(100, 0), High: dec(110, 0), Low: dec(90, 0), Close: dec(105, 0), Volume: dec(15, -1), TradeCount: 42,
	}}}
	trade := &marketpb.MarketDataEvent{InstrumentId: instrument, Mic: "XBIN", EventTime: at,
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: dec(9007199254740993, -2)}}}
	quote := &marketpb.MarketDataEvent{InstrumentId: instrument, Mic: "XBIN", EventTime: at,
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: dec(2303, -2), AskPrice: dec(2305, -2)}}}
	original := []marketdatatest.Frame{
		{EventType: "market.crypto.bar", Payload: bar},
		{EventType: "market.crypto.trade", Payload: trade},
		{EventType: "market.crypto.quote", Payload: quote},
	}
	broker.Publish(t, ctx, original, first)
	correction := proto.Clone(trade).(*marketpb.MarketDataEvent)
	correction.GetTrade().Price = dec(9007199254740995, -2)
	broker.Publish(t, ctx, []marketdatatest.Frame{{EventType: "market.crypto.trade", Payload: correction}}, revised)
	broker.Consume(t, ctx, 0, ingestor.Handler, len(original)+1, nil)

	for _, tc := range []struct {
		kind           store.PriceKind
		first, revised *big.Rat
	}{
		{store.PriceKindClose, big.NewRat(105, 1), big.NewRat(105, 1)},
		{store.PriceKindLast, big.NewRat(9007199254740993, 100), big.NewRat(9007199254740995, 100)},
		{store.PriceKindMid, big.NewRat(2304, 100), big.NewRat(2304, 100)},
	} {
		type refusal struct {
			instrument, reason string
			age                time.Duration
		}
		var refusals []refusal
		provider, err := spotsource.FromStore(st, spotsource.WithPriceKind(tc.kind), spotsource.WithMaxAge(3*time.Minute),
			spotsource.WithUnresolvedObserver(func(id, reason string, age time.Duration) {
				refusals = append(refusals, refusal{id, reason, age})
			}))
		if err != nil {
			t.Fatal(err)
		}
		for _, read := range []struct {
			at   time.Time
			want *big.Rat
		}{
			{observed, nil},
			{first, tc.first},
			{revised.Add(-time.Microsecond), tc.first},
			{revised, tc.revised},
			{observed.Add(3 * time.Minute), tc.revised},
			{observed.Add(3*time.Minute + time.Microsecond), nil},
		} {
			got, ok := provider.Mark(ctx, instrument, read.at)
			if ok != (read.want != nil) || ok && got.Cmp(read.want) != 0 {
				t.Errorf("kind %v at %s: got (%v, %t), want %v", tc.kind, read.at, got, ok, read.want)
			}
		}
		wantRefusals := []refusal{
			{instrument, spotsource.ReasonNoObservation, 0},
			{instrument, spotsource.ReasonStale, 3*time.Minute + time.Microsecond},
		}
		if !slices.Equal(refusals, wantRefusals) {
			t.Errorf("kind %v: refusal history %v, want %v", tc.kind, refusals, wantRefusals)
		}
	}
	for _, kind := range []store.PriceKind{
		store.PriceKindAdjustedClose, store.PriceKindOpen, store.PriceKindVWAP, store.PriceKindSettlement,
	} {
		if _, found, err := st.LatestAsOf(ctx, instrument, kind, revised); err != nil || found {
			t.Errorf("unproduced kind %v in the store: found=%t err=%v", kind, found, err)
		}
		if _, err := spotsource.FromStore(st, spotsource.WithPriceKind(kind)); err == nil {
			t.Errorf("risk accepted unproduced kind %v", kind)
		}
	}
}

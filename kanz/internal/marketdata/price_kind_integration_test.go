package marketdata

import (
	"context"
	"math/big"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/risk/spotsource"
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
	st := store.NewPostgres(boundaryPool(t, ctx, dsn))
	ingestor, err := NewIngestor(st)
	if err != nil {
		t.Fatal(err)
	}
	broker := newPayloadTimeBroker(t, ctx)
	observed := time.Date(2026, 10, 19, 12, 30, 0, 0, time.UTC)
	first := observed.Add(time.Second)
	revised := first.Add(time.Minute)
	instrument := broker.id
	bar := barEvent("XBIN", observed.Add(-time.Minute), observed)
	trade := timedMarketEvent("trade", observed)
	quote := timedMarketEvent("quote", observed)
	bar.InstrumentId, trade.InstrumentId, quote.InstrumentId = instrument, instrument, instrument
	trade.GetTrade().Price = decv(9007199254740993, -2)
	quote.GetQuote().BidPrice, quote.GetQuote().AskPrice = decv(2303, -2), decv(2305, -2)
	original := []payloadTimeCase{
		{"bar close", "market.crypto.bar", bar},
		{"trade", "market.crypto.trade", trade},
		{"quote", "market.crypto.quote", quote},
	}
	broker.publish(t, ctx, original, first)
	correction := timedMarketEvent("trade", observed)
	correction.InstrumentId = instrument
	correction.GetTrade().Price = decv(9007199254740995, -2)
	updated := []payloadTimeCase{{"corrected trade", "market.crypto.trade", correction}}
	broker.publish(t, ctx, updated, revised)
	broker.consume(t, ctx, 0, ingestor.Handler, append(original, updated...), nil)

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

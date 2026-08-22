package binance

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// tickerFeedOver builds the feed exactly as runTicker does, so a test cannot
// certify a wiring the connector does not use.
func tickerFeedOver(rest *binanceREST, pub Publisher, onDropped func(mic, instrumentID string)) *binanceTickerFeed {
	return &binanceTickerFeed{
		rest:    rest,
		symbols: map[string]string{"BTC-USD": "BTCUSDT"},
		ticks:   newMarkTickPublisher(pub, nil, "BINANCE", onDropped),
	}
}

func tickerREST(t *testing.T, f *fakeBinance) *binanceREST {
	t.Helper()
	bucket := newWeightBucket(1200, time.Minute, nil)
	return newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})
}

// The ticker feed polls the last price and publishes a market.v1.MarketDataEvent
// (a Trade), which tv-sync's MarkSource folds into live unrealized P&L.
func TestTicker_PublishesMarketTrade(t *testing.T) {
	f := newFakeBinance(t)
	f.tickerPrice = "50123.5"

	cap := &reconCapture{}
	feed := tickerFeedOver(tickerREST(t, f), cap, nil)
	feed.pollOnce(context.Background())

	var ev *marketpb.MarketDataEvent
	for _, e := range cap.events {
		if m, ok := e.Payload.(*marketpb.MarketDataEvent); ok {
			ev = m
		}
	}
	if ev == nil {
		t.Fatal("no MarketDataEvent published")
	}
	if ev.GetInstrumentId() != "BTC-USD" {
		t.Fatalf("instrument = %q, want BTC-USD (canonical, not the exchange symbol)", ev.GetInstrumentId())
	}
	if got := dec.FromProto(ev.GetTrade().GetPrice()); got.Cmp(big.NewRat(1002470, 20)) != 0 { // 50123.5
		t.Fatalf("price = %s, want 50123.5", got.RatString())
	}
	if e := cap.events[len(cap.events)-1]; e.Subject != "market.crypto.trade" {
		t.Fatalf("subject = %q, want market.crypto.trade", e.Subject)
	}
}

// refusingPublisher is the broker this repo's in-memory fakes never are: one
// that says no. reconCapture accepts everything, which is exactly why this feed
// discarding its publish error survived a green suite for as long as it did.
type refusingPublisher struct{ err error }

func (r refusingPublisher) Publish(context.Context, bus.Event) error { return r.err }

// #673: a tick the broker refuses must reach the counter the composition root
// wires. Before this, the error was assigned to _ and a refused subject looked
// exactly like an exchange with nothing to report.
func TestTicker_DroppedPublishIsObserved(t *testing.T) {
	f := newFakeBinance(t)
	f.tickerPrice = "50123.5"

	var dropped []string
	feed := tickerFeedOver(
		tickerREST(t, f),
		refusingPublisher{err: errors.New("envelope validation: tenant_id required")},
		func(mic, instrumentID string) { dropped = append(dropped, mic+"/"+instrumentID) },
	)
	feed.pollOnce(context.Background())

	if len(dropped) != 1 || dropped[0] != "BINANCE/BTC-USD" {
		t.Fatalf("dropped observations = %v, want exactly [BINANCE/BTC-USD]. A refused market-data publish "+
			"must be counted at the source: downstream only ever sees a mark that stopped arriving, which "+
			"is the same thing a quiet market looks like", dropped)
	}
}

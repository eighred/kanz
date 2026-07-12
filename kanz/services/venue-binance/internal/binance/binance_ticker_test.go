package binance

import (
	"context"
	"math/big"
	"testing"
	"time"

	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"

	"github.com/kanz-eng/kanz/internal/dec"
)

// The ticker feed polls the last price and publishes a market.v1.MarketDataEvent
// (a Trade), which tv-sync's MarkSource folds into live unrealized P&L.
func TestTicker_PublishesMarketTrade(t *testing.T) {
	f := newFakeBinance(t)
	f.tickerPrice = "50123.5"
	bucket := newWeightBucket(1200, time.Minute, nil)
	rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})

	cap := &reconCapture{}
	feed := &binanceTickerFeed{rest: rest, symbols: map[string]string{"BTC-USD": "BTCUSDT"}, mic: "BINANCE", pub: cap}
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

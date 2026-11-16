package okx

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"github.com/eighred/kanz/pkg/bus"
)

// THIS FILE EXISTS BECAUSE THE OKX TICKER FEED HAD NO TEST AT ALL (#673).
//
// The Binance feed had one and the OKX feed — the same code, in a second service
// — had none, which is the shape the issue is about: a repair, or a test, that
// lands on one venue and not the other. Both now publish through
// execution.MarkTickPublisher, and both are asserted here and in
// binance_ticker_test.go against the same two facts.

func okxTickerConnector(f *fakeOKX) *OKXConnector {
	bucket := NewWeightBucket(60, 2*time.Second, nil)
	rest := newOKXREST(okxRestConfig{
		BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p",
		Buckets: newOKXBuckets(bucket, nil), Mode: exchangeauth.OKXDemo,
	})
	return &OKXConnector{
		settings: VenueSettings{MIC: "OKX", Symbols: map[string]string{"BTC-USD": "BTC-USDT"}},
		rest:     rest,
	}
}

func TestOKXTicker_PublishesMarketTrade(t *testing.T) {
	f := newFakeOKX(t)
	f.tickerBody = `{"code":"0","data":[{"last":"50123.5"}]}`

	cap := &okxCapture{}
	c := okxTickerConnector(f)
	c.pollTicker(context.Background(), NewMarkTickPublisher(cap, nil, c.settings.MIC, nil))

	var ev *marketpb.MarketDataEvent
	for _, e := range cap.events {
		if m, ok := e.Payload.(*marketpb.MarketDataEvent); ok {
			ev = m
		}
	}
	if ev == nil {
		t.Fatal("no MarketDataEvent published")
	}
	if ev.GetInstrumentId() != "BTC-USD" || ev.GetMic() != "OKX" {
		t.Fatalf("identity = (%q,%q), want (BTC-USD,OKX) — the canonical instrument, not the exchange symbol",
			ev.GetInstrumentId(), ev.GetMic())
	}
	if got := dec.FromProto(ev.GetTrade().GetPrice()); got.Cmp(big.NewRat(1002470, 20)) != 0 { // 50123.5
		t.Fatalf("price = %s, want 50123.5", got.RatString())
	}
	if e := cap.events[len(cap.events)-1]; e.Subject != "market.crypto.trade" {
		t.Fatalf("subject = %q, want market.crypto.trade — one subject for both venues", e.Subject)
	}
}

// okxRefusingPublisher is the broker okxCapture never is: one that says no.
type okxRefusingPublisher struct{ err error }

func (r okxRefusingPublisher) Publish(context.Context, bus.Event) error { return r.err }

// #673, the OKX half: a tick the broker refuses reaches the counter the
// composition root wires. It used to be assigned to _.
func TestOKXTicker_DroppedPublishIsObserved(t *testing.T) {
	f := newFakeOKX(t)
	f.tickerBody = `{"code":"0","data":[{"last":"50123.5"}]}`

	var dropped []string
	c := okxTickerConnector(f)
	ticks := NewMarkTickPublisher(
		okxRefusingPublisher{err: errors.New("envelope validation: tenant_id required")},
		nil, c.settings.MIC,
		func(mic, instrumentID string) { dropped = append(dropped, mic+"/"+instrumentID) },
	)
	c.pollTicker(context.Background(), ticks)

	if len(dropped) != 1 || dropped[0] != "OKX/BTC-USD" {
		t.Fatalf("dropped observations = %v, want exactly [OKX/BTC-USD]. A refused market-data publish must "+
			"be counted at the source: downstream only sees a mark that stopped arriving, which is what a "+
			"quiet market looks like too", dropped)
	}
}

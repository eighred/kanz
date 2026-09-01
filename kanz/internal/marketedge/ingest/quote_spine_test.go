// THE QUOTE, THROUGH THE REAL SPINE, INTO THE FOLD THE OMS ACTUALLY WIRES.
//
// Every other test in this package publishes through a real bus.Producer over a
// FAKE client. That proves the envelope is well formed; it does not prove the
// broker accepts it, that the MARKET stream carries market.crypto.quote at all,
// or that a subscription shaped like the OMS's (market.*.quote, broadcast)
// delivers it. Those are the three ways #876 could be "fixed" and still leave
// the estate exactly where it was — a subject nothing is bound to is a silent
// publish failure, and a denied subscription is silent in the other direction
// (#787/#788).
//
// So this one runs the ingest Engine against a real NATS JetStream broker and
// reads the width back out of internal/marketdata/mark, which is the type
// services/oms/cmd/oms/main.go hands to order.WithArrivalMarks.
//
// SCOPED BY A PER-RUN INSTRUMENT ID, NEVER BY A COUNT. The MARKET stream has 24h
// retention and a developer broker is dirty, so a test that counted messages
// would pass once and fail forever after. Every assertion here is about the
// content held for ONE instrument id that has never existed before.
package ingest

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/marketedge/book"
	"github.com/eighred/kanz/internal/marketedge/depth"
	"github.com/eighred/kanz/pkg/bus"
)

// omsPriceMaxAge is the OMS's shipped OMS_PRICE_MAX_AGE default — the bound the
// width has to be inside when the arrival stamp reads it.
const omsPriceMaxAge = 30 * time.Second

func TestATopOfBookQuoteCrossesTheRealSpineAndBecomesALiveWidth(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the quote path over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	instrument := fmt.Sprintf("BTC-USD-%d", time.Now().UnixNano())

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	// The production topology provisions MARKET as market.> (infra/nats/
	// bootstrap-job.yaml), so this binds to the REAL stream and thereby proves
	// the subject is carried. Creating an overlapping stream on an estate
	// subject is refused by JetStream, which is the failure this helper exists
	// to avoid.
	bustest.EnsureSubjects(t, ctx, js, "MARKET_QUOTE_IT", []string{SubjectMarketCryptoQuote})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "market-ingest-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "market-ingest", ProducerVersion: "it", Tenant: "__system__",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}

	// THE CONSUMER IS THE OMS'S, not a convenience double: mark.New with the
	// OMS's own maxAge, folded by mark.Handle, subscribed on the OMS's default
	// price subject through SubscribeBroadcast — services/oms/cmd/oms/main.go
	// does exactly this for each of cfg.PriceSubjects.
	marks := mark.New(time.Now, omsPriceMaxAge)
	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	go func() { _ = consumer.SubscribeBroadcast(subCtx, "market.*.quote", marks.Handle) }()

	// The engine folds one venue book and publishes on its own ticker. It is the
	// real Engine over the real producer — the only thing scripted is the depth
	// feed, which is where the venue would be.
	venueTime := time.Now().UTC()
	src := &scriptedSource{updates: []depth.Update{
		{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId: instrument, LastUpdateSequence: 11, EventTime: tsOf(venueTime),
			Bids: []*marketpb.PriceLevel{lvl("50000", "3"), lvl("49999", "10")},
			Asks: []*marketpb.PriceLevel{lvl("50001", "2"), lvl("50002", "10")},
		}},
	}}
	eng := New(Config{
		Book: book.New(instrument, "BTCUSDT", "BINANCE"), Source: src, Publisher: producer,
		SnapshotInterval: 200 * time.Millisecond, SnapshotDepth: 10, Tenant: "__system__",
	})
	runCtx, stopEngine := context.WithCancel(ctx)
	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = eng.Run(runCtx) }()
	t.Cleanup(func() { stopEngine(); <-engDone })

	// THE MEASUREMENT, not merely the assertion: how old the width is by the time
	// the fold can answer for it. That number is what decides whether
	// OMS_PRICE_MAX_AGE is a sane bound for a WIDTH as opposed to a mark, and it
	// has to come from the wire rather than from arithmetic about the ticker.
	var bid, ask *big.Rat
	var asOf time.Time
	deadline := time.Now().Add(30 * time.Second)
	for {
		var ok bool
		bid, ask, asOf, ok = marks.Touch(instrument)
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no usable width for this instrument after 30s on a real broker. That is #876's " +
				"state reproduced: the publish was refused, the subject is not carried, or the " +
				"subscription never delivered — and all three look identical to a quiet market")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if bid.Cmp(big.NewRat(50000, 1)) != 0 || ask.Cmp(big.NewRat(50001, 1)) != 0 {
		t.Fatalf("Touch = %s/%s over the real spine, want 50000/50001", bid.RatString(), ask.RatString())
	}
	age := time.Since(asOf)
	t.Logf("MEASURED width age at the fold: %v (venue book time %s, OMS_PRICE_MAX_AGE %v, "+
		"engine snapshot interval %v)", age.Round(time.Millisecond), venueTime.Format(time.RFC3339Nano),
		omsPriceMaxAge, 200*time.Millisecond)
	if age > omsPriceMaxAge {
		t.Fatalf("the width was already %v old when the fold first answered for it — inside a %v "+
			"bound this is a coin flip, not a benchmark", age, omsPriceMaxAge)
	}

	// And the mid rides the same event, so the price and the width the arrival
	// stamp records describe ONE observation rather than two.
	if m := marks.Mark(instrument); m == nil || m.Cmp(big.NewRat(100001, 2)) != 0 {
		t.Fatalf("Mark = %v over the real spine, want the 50000.5 mid of the same quote", m)
	}
}

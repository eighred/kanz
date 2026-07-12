package ingest

// NATS + OMS + Ingest end-to-end integration (M1 certification over the real
// bus). Gated on TEST_NATS_URL — it requires a live NATS AND a running OMS
// (subscribed to order.order.submit with a SimVenue) on that bus, so it skips
// by default, mirroring the TEST_POSTGRES_URL-gated store tests. The core
// isolation invariant forbids importing the OMS internals in-process, so this
// drives the loop black-box: fan out real SubmitOrder commands onto NATS and
// assert the OMS's OrderFilled FACTs come back.
//
// Run it with the dev stack up:
//
//	./start.sh                 # NATS + Kafka + Postgres
//	go run ./services/oms/cmd/oms &     # OMS consuming order.order.submit
//	TEST_NATS_URL=nats://localhost:4222 go test -run TestIntegration_LoopOverNATS \
//	    ./services/webhook-ingest/internal/ingest/

import (
	"context"
	"math/big"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/internal/signal/translate"

	"github.com/kanz-eng/kanz/internal/dec"
	"github.com/kanz-eng/kanz/pkg/bus"
)

func TestIntegration_LoopOverNATS(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL (with the dev stack + OMS running) to run the end-to-end loop")
	}
	// This test needs TWO things: a live NATS *and* a running OMS consuming from it.
	// It gated on the first and assumed the second, so the moment a CI job provided
	// NATS without an OMS it did not skip — it FAILED, waiting 15s for fills from a
	// service that was never started. That is why NATS could not be added to CI, and
	// so why the real-bus guards (the EXEC-M7a publish test, the EXEC-M8 venue
	// publish-health test) have never once run there.
	//
	// A test must gate on ALL of its preconditions. This one now declares the OMS.
	if os.Getenv("TEST_OMS_ON_BUS") == "" {
		t.Skip("set TEST_OMS_ON_BUS=1 with an OMS consuming order.order.submit on TEST_NATS_URL to run the end-to-end loop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "webhook-ingest-it"})
	if err != nil {
		t.Fatalf("dial NATS: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Observe the OMS's fills coming back on the bus.
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	var mu sync.Mutex
	filled := map[string]*big.Rat{} // order_id -> filled qty
	fills := make(chan struct{}, 8)
	go func() {
		_ = consumer.Subscribe(ctx, "order.order.filled", "webhook-ingest-it", func(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
			var ev orderpb.OrderFilled
			if err := proto.Unmarshal(payload, &ev); err != nil {
				return nil
			}
			mu.Lock()
			filled[ev.GetOrderId()] = dec.FromProto(ev.GetState().GetFilledQuantity())
			mu.Unlock()
			fills <- struct{}{}
			return nil
		})
	}()
	time.Sleep(500 * time.Millisecond) // let the subscription establish

	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "webhook-ingest-it", ProducerVersion: "it"})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	// A limit order fills at the limit in SimVenue regardless of the OMS's price
	// config — so the loop produces fills deterministically.
	p, err := NewPipeline(Options{
		Auth:      NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now),
		Symbols:   StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{},
		Alloc: StaticAllocation{"fund-alpha": {
			{Venue: "XNAS", Weight: big.NewRat(6, 10)},
			{Venue: "XLON", Weight: big.NewRat(4, 10)},
		}},
		Publisher: producer,
		Gate:      translate.OpenGate(nil),
	})
	if err != nil {
		t.Fatal(err)
	}

	raw := `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
		`"action":"buy","size":"1","size_type":"absolute_qty","order_type":"limit","limit_price":"50000","nonce":"it-1"}`
	res, err := p.Process(ctx, []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(res.OrderIDs) != 2 {
		t.Fatalf("fanned out %d orders, want 2", len(res.OrderIDs))
	}

	// Wait for both fills to come back from the OMS.
	deadline := time.After(15 * time.Second)
	for got := 0; got < 2; {
		select {
		case <-fills:
			got++
		case <-deadline:
			t.Fatalf("timed out waiting for OMS fills; got %d/2 (is the OMS running on %s?)", got, url)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	total := new(big.Rat)
	for _, id := range res.OrderIDs {
		q, ok := filled[id]
		if !ok {
			t.Fatalf("no fill for order %s", id)
		}
		total.Add(total, q)
	}
	if total.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("total filled = %s, want 1 (fan-out must conserve size end to end)", total.RatString())
	}
}

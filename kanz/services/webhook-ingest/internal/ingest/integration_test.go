package ingest

// NATS + OMS + Ingest admission integration over the real bus. It requires a
// live NATS and a running funded OMS on that bus. The core
// isolation invariant forbids importing the OMS internals in-process, so this
// drives the loop black-box. Without authoritative cash coverage, the funded
// OMS must reject every fanned-out order before it reaches a simulator.
//
// Run it with the dev stack up:
//
//	./start.sh                 # NATS + Kafka + Postgres
//	go run ./services/oms/cmd/oms &     # OMS consuming order.order.submit
//	TEST_NATS_URL=nats://localhost:4222 go test -run TestIntegration_FundedAdmissionRefusesUncoveredFanout \
//	    ./services/webhook-ingest/internal/ingest/

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/bus"
)

func TestIntegration_FundedAdmissionRefusesUncoveredFanout(t *testing.T) {
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

	// Observe terminal refusals. No authoritative cash or reference publisher is
	// bootstrapped in this job; a fill would be a capital-control breach.
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	rejected := make(chan *orderpb.OrderRejected, 8)
	go func() {
		_ = consumer.Subscribe(ctx, "order.order.rejected", "webhook-ingest-it", func(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
			var ev orderpb.OrderRejected
			if err := proto.Unmarshal(payload, &ev); err != nil {
				return nil
			}
			rejected <- &ev
			return nil
		})
	}()
	time.Sleep(500 * time.Millisecond) // let the subscription establish

	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "webhook-ingest-it", ProducerVersion: "it"})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	// The producer still drives its real two-venue fan-out onto the broker.
	p, err := NewPipeline(Options{
		Authority: ingestTestAuthority(t),
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
		Gate:      halt.OpenGate(nil),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A FRESH nonce per run. It is the webhook's replay guard, and it also flows into
	// the deterministic order ids — so a fixed nonce makes every run publish the SAME
	// SubmitOrder, and the real EXECUTION stream's 2-minute duplicate window
	// (infra/nats/bootstrap-job.yaml) then SILENTLY DEDUPLICATES it at the broker.
	// The publish succeeds, the command never lands, and the OMS never sees an order
	// to fill. Exactly-once is doing its job; the test was replaying a command.
	// The `ts` is not decoration: #416 made it MANDATORY, so this literal without
	// one is refused at Process and the M1 certification never reaches the bus at
	// all. That is how it broke — the fixture predates the rule, and this is the
	// only alert literal in the tree that CI cannot catch on a machine without a
	// broker, because everything above gates on TEST_NATS_URL + TEST_OMS_ON_BUS.
	raw := fmt.Sprintf(`{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",`+
		`"action":"buy","size":"1","size_type":"absolute_qty","order_type":"limit","limit_price":"50000","nonce":"it-%d",`,
		time.Now().UnixNano()) + freshTS()
	res, err := p.Process(ctx, []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(res.OrderIDs) != 2 {
		t.Fatalf("fanned out %d orders, want 2", len(res.OrderIDs))
	}

	// Match only this run's ids; the durable stream also replays earlier runs.
	deadline := time.After(15 * time.Second)
	seen := map[string]string{}
	for {
		if len(seen) == len(res.OrderIDs) {
			break
		}
		select {
		case ev := <-rejected:
			for _, id := range res.OrderIDs {
				if ev.GetOrderId() == id {
					seen[id] = ev.GetErrorCode()
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for this run's OMS capital refusals; have %d/%d of %v (OMS on %s)", len(seen), len(res.OrderIDs), res.OrderIDs, url)
		}
	}
	for _, id := range res.OrderIDs {
		if seen[id] != "CAPITAL_TERMS_UNKNOWN" && seen[id] != "CAPITAL_REFUSED" && seen[id] != "CAPITAL_ACTIVATION_REQUIRED" {
			t.Fatalf("order %s rejected for %q, not missing authoritative capital input", id, seen[id])
		}
	}
}

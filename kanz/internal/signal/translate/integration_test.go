// The signal→order path against a REAL spine.
//
// Every other test in this package injects a fake Publisher that satisfies the
// interface and never validates an envelope — which is exactly why both publishes
// here shipped without a payload_schema_ref and were rejected by the first real
// broker, and why nobody noticed that no JetStream stream was even bound to
// strategy.> or order.>. A fake Publisher cannot fail the way a broker fails.
//
// This test is the guard: it drives Emit through a real bus.Producer over real
// NATS/JetStream and reads both events back off the wire.
//
//	docker run -d --name kanz-nats -p 4222:4222 nats:2 -js
//	TEST_NATS_URL=nats://localhost:4222 go test ./internal/signal/translate/...
package translate_test

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	signalpb "github.com/kanz-eng/kanz-schemas-go/signal/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/kanz-eng/kanz/internal/bustest"
	"github.com/kanz-eng/kanz/internal/signal/translate"
	"github.com/kanz-eng/kanz/pkg/bus"
)

func TestIntegration_EmitReachesTheWire(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the signal→order path over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mirrors infra/nats/bootstrap-job.yaml: the EXECUTION stream carries
	// execution.>, strategy.> AND order.>. Before that, neither subject was bound to
	// any stream, and bus.Publish is a JetStream publish — so these events had
	// nowhere to land.
	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	// t.Cleanup, NOT defer. A deferred Close runs BEFORE the registered cleanups, so
	// the DeleteStream below was firing on a connection this line had already torn
	// down: the stream survived the test, kept its messages, and poisoned the next
	// run — the test passed on a fresh broker and failed on every repeat. Cleanups
	// run last-registered-first, so registering the close FIRST makes it run LAST.
	t.Cleanup(setupConn.Close)
	js, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	// These are the REAL subjects — that is the whole point of this test, since the
	// EXEC-M7a bug was that no stream was bound to them at all. When the production
	// topology is provisioned (CI bootstraps the same script prod applies), this
	// binds to the real EXECUTION stream and proves the real binding.
	bustest.EnsureSubjects(t, ctx, js, "EXECUTION_IT", []string{"execution.>", "strategy.>", "order.>"})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "translate-it"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "translate-it", ProducerVersion: "test", Tenant: "eighred",
	})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	var mu sync.Mutex
	got := map[string]*envelopepb.Envelope{}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	collect := func(_ context.Context, env *envelopepb.Envelope, _ []byte) error {
		mu.Lock()
		defer mu.Unlock()
		got[env.GetEventType()] = env
		return nil
	}
	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	// A Subscribe error must not be swallowed. A subscription that never started is
	// indistinguishable from a subject nobody publishes to — which is the entire
	// class of bug this test exists to catch.
	subErr := make(chan error, 2)
	go func() {
		err := consumer.Subscribe(subCtx, translate.SubjectSignal, "translate-it-sig", collect)
		if err != nil && subCtx.Err() == nil {
			subErr <- fmt.Errorf("subscribe %s: %w", translate.SubjectSignal, err)
		}
	}()
	go func() {
		err := consumer.Subscribe(subCtx, translate.SubjectSubmit, "translate-it-cmd", collect)
		if err != nil && subCtx.Err() == nil {
			subErr <- fmt.Errorf("subscribe %s: %w", translate.SubjectSubmit, err)
		}
	}()
	select {
	case err := <-subErr:
		t.Fatal(err)
	case <-time.After(time.Second): // both subscriptions are up
	}

	tr, err := translate.New(translate.Options{
		Prices:    translate.StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    translate.StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: translate.StaticPositions{},
		Alloc: translate.StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(1, 1)},
		}},
		Publisher: producer,
		Gate:      translate.OpenGate(nil),
	})
	if err != nil {
		t.Fatalf("translate.New: %v", err)
	}

	// A FRESH signal id per run. The order command's idempotency key is derived from
	// it and is therefore deterministic — which is the point in production, and a
	// trap here: the real EXECUTION stream carries a 2-minute duplicate window
	// (infra/nats/bootstrap-job.yaml), so re-running this test with a fixed id had
	// the broker SILENTLY DEDUPLICATE the SubmitOrder. The publish "succeeded", the
	// command never landed, and the test timed out looking for it. The old test
	// deleted and recreated its own stream each run, which wiped the dedup state and
	// hid this entirely. Exactly-once is working; the test was replaying a command.
	res, err := tr.Emit(ctx, translate.Intent{
		SignalID:     translate.DeterministicID(fmt.Sprintf("it-signal-%d", time.Now().UnixNano())),
		StrategyID:   "momentum",
		FundID:       "fund-alpha",
		InstrumentID: "BTC-USD",
		Action:       signalpb.SignalAction_SIGNAL_ACTION_BUY,
		Size:         big.NewRat(1, 1),
		SizeType:     signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY,
		OrderType:    orderpb.OrderType_ORDER_TYPE_MARKET,
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_IOC,
		Source:       signalpb.SignalSource_SIGNAL_SOURCE_NATIVE_ENGINE,
	})
	if err != nil {
		// This is the failure the whole test exists to catch: an envelope the broker
		// rejects, or a subject with no stream to land on.
		t.Fatalf("Emit over a real spine = %v — the signal→order path does not reach the wire", err)
	}
	if len(res.OrderIDs) != 1 {
		t.Fatalf("OrderIDs = %v, want 1", res.OrderIDs)
	}

	waitFor(t, "both events to land", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2
	})

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []struct{ eventType, schemaRef string }{
		{translate.SubjectSignal, "signal.v1.StrategySignal:1"},
		{translate.SubjectSubmit, "order.v1.SubmitOrder:1"},
	} {
		env, ok := got[want.eventType]
		if !ok {
			t.Fatalf("%s never arrived", want.eventType)
		}
		// Derived by the producer from the payload — Validate requires it, and no
		// caller on this path sets it.
		if env.GetPayloadSchemaRef() != want.schemaRef {
			t.Errorf("%s payload_schema_ref = %q, want %q",
				want.eventType, env.GetPayloadSchemaRef(), want.schemaRef)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

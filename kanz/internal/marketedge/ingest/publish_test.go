// THE BOOK SNAPSHOT'S ENVELOPE, DRIVEN THROUGH A REAL PRODUCER (#245).
//
// A CORRECTION TO THIS PACKAGE'S ENTRY ON #245's LIST, which reads "capture
// double accepts any Event (engine_test.go)". It does not, and has not for
// some time: engine_test.go's capture ENFORCES the tenant rule explicitly, with
// a comment explaining that a double accepting an untenanted snapshot would let
// this engine's correctness depend on how each caller wires its producer.
// Somebody already hardened it.
//
// The gap is real but narrower than the entry says: the double checks the tenant
// and NOTHING ELSE. It never runs bus.Validate, so every other field of the
// envelope — class, domain, schema ref, partition key — was unproven.
//
// WHY THAT MATTERS HERE SPECIFICALLY. publishSnapshot LOGS its error and returns:
//
//	e.logger.Warn("book snapshot publish failed", ...)
//
// So an envelope the broker refuses does not stop the engine, does not fail a
// health check and does not surface anywhere except a warning line. The book
// keeps folding, the ticker keeps firing, and nothing downstream ever receives a
// snapshot. That is the same best-effort shape that hid the compliance/audit
// defect, where a missing EventTime meant every decision silently failed to
// publish.
//
// Tier-B: a real bus.Producer over a fake bus.Client.
package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketedge/book"
	"github.com/eighred/kanz/internal/marketedge/depth"
	"github.com/eighred/kanz/pkg/bus"
)

// captureClient records the framed wire bytes — the Tier-B helper from
// internal/risk/publish and services/accounting/internal/cashmove.
type captureClient struct {
	mu   chan struct{} // 1-buffered, used as a mutex the test can also wait on
	sent []bus.Message
}

func newCaptureClient() *captureClient {
	c := &captureClient{mu: make(chan struct{}, 1)}
	c.mu <- struct{}{}
	return c
}

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	<-c.mu
	c.sent = append(c.sent, m)
	c.mu <- struct{}{}
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func (c *captureClient) first() (bus.Message, bool) {
	<-c.mu
	defer func() { c.mu <- struct{}{} }()
	if len(c.sent) == 0 {
		return bus.Message{}, false
	}
	return c.sent[0], true
}

func (c *captureClient) count() int {
	<-c.mu
	defer func() { c.mu <- struct{}{} }()
	return len(c.sent)
}

// runUntilPublished drives the engine until the client has captured a message or
// the deadline passes.
func runUntilPublished(t *testing.T, tenant string) *captureClient {
	t.Helper()
	cc := newCaptureClient()
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "market-edge",
		ProducerVersion: "test",
		// NO Tenant fallback. This library is used by callers that wire their own
		// producer, and the whole reason Config.Tenant exists is so its
		// correctness does not depend on that wiring.
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	src := &scriptedSource{updates: []depth.Update{
		{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId: "BTC-USD", LastUpdateSequence: 1,
			Bids: []*marketpb.PriceLevel{lvl("50000", "1")},
			Asks: []*marketpb.PriceLevel{lvl("50001", "1")},
		}},
	}}
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: src, Publisher: prod,
		SnapshotInterval: 10 * time.Millisecond, SnapshotDepth: 10, Tenant: tenant,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()

	deadline := time.After(1500 * time.Millisecond)
	for {
		if cc.count() > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			return cc
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	return cc
}

// THE ONE THAT MATTERS. If the snapshot envelope is not legal, nothing
// downstream ever gets a book — and the engine says so only in a log line.
func TestBookSnapshotIsAValidEnvelope(t *testing.T) {
	cc := runUntilPublished(t, "test-tenant")

	m, ok := cc.first()
	if !ok {
		t.Fatal("no snapshot reached the transport through a real producer. publishSnapshot LOGS its " +
			"error and returns, so a refused envelope would look exactly like this: a healthy engine " +
			"folding a book nobody receives")
	}
	env, _, err := bus.Unframe(m.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("the snapshot envelope fails Validate: %v", err)
	}
	if got := env.GetEventType(); got != SubjectBookSnapshot {
		t.Errorf("event_type = %q, want %q", got, SubjectBookSnapshot)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT", got)
	}
	if got := env.GetTenantId(); got != "test-tenant" {
		t.Errorf("tenant_id = %q, want test-tenant", got)
	}
	if got := env.GetDomain(); got != "market" {
		t.Errorf("domain = %q, want market", got)
	}
	// DERIVED, not set by this package: the producer builds it from the payload's
	// proto full name and SchemaVersion. Validate requires it non-empty.
	if got := env.GetPayloadSchemaRef(); got != "market.v1.OrderBookSnapshot:1" {
		t.Errorf("payload_schema_ref = %q, want market.v1.OrderBookSnapshot:1 — this package sets "+
			"none, so the producer DERIVES it from the payload type", got)
	}
	if got := string(m.Key); got != "BTC-USD" {
		t.Errorf("partition key = %q, want the instrument id — one book's snapshots must stay "+
			"ordered against each other", got)
	}
}

// THE TENANT IS THE LIBRARY'S OWN, NOT THE CALLER'S PRODUCER CONFIG.
//
// Config.Tenant exists precisely so this engine does not depend on how each
// caller wires its ProducerConfig. With neither supplied, the real producer
// refuses — and because publishSnapshot only logs, the engine keeps running and
// nothing else reports it. This test is what makes that silence visible.
func TestASnapshotWithNoTenantNeverReachesTheTransport(t *testing.T) {
	cc := runUntilPublished(t, "") // Config.Tenant empty, producer has no fallback

	if n := cc.count(); n != 0 {
		t.Fatalf("%d snapshot(s) reached the transport with no tenant from either source. On the live "+
			"spine bus.Validate refuses these, so the engine would warn once per tick forever while "+
			"reporting healthy", n)
	}
}

// AN EMPTY BOOK PUBLISHES NOTHING. The guard at the top of publishSnapshot, kept
// honest: an empty snapshot is a legal envelope, so Validate would accept it, and
// a consumer folding one would see a book with no levels rather than no update.
func TestAnEmptyBookPublishesNothing(t *testing.T) {
	cc := newCaptureClient()
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source: "market-edge", ProducerVersion: "test",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: &scriptedSource{}, Publisher: prod,
		SnapshotInterval: 5 * time.Millisecond, SnapshotDepth: 10, Tenant: "test-tenant",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	<-ctx.Done()
	<-done

	if n := cc.count(); n != 0 {
		t.Errorf("%d snapshot(s) published from a book that folded nothing — a consumer would read an "+
			"empty book as a real state rather than as no update", n)
	}
}

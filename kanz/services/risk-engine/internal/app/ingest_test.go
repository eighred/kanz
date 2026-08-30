package app_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk-engine/internal/app"
)

// captureClient records framed messages by subject. Paired with
// bus.Producer it yields realistic wire bytes the fakeSub replays into
// the Consumer — the same EventFrame framing the real bus carries.
type captureClient struct {
	mu  sync.Mutex
	out map[string][]bus.Message
}

func newCapture() *captureClient { return &captureClient{out: map[string][]bus.Message{}} }

func (c *captureClient) Publish(_ context.Context, msg bus.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out[msg.Subject] = append(c.out[msg.Subject], msg)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error { return nil }
func (c *captureClient) Close() error                                                 { return nil }

// fakeSub replays captured messages for a subject then blocks until ctx
// is canceled — matching the Subscriber contract (Subscribe blocks until
// ctx done). Snapshots the slice so concurrent subscribes are race-free.
type fakeSub struct {
	msgs map[string][]bus.Message
}

// Subscribe matches subjects the way a broker does, not with `==`. The engine binds
// risk.position.> (EXEC-M20), and a fake that only compares strings would deliver nothing
// while a real broker delivered everything — the fake would pass and production would
// starve.
func (f *fakeSub) Subscribe(ctx context.Context, subject, _ string, h bus.Handler) error {
	for subj, ms := range f.msgs {
		if !subjectMatches(subject, subj) {
			continue
		}
		for _, m := range ms {
			_ = h(ctx, m)
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func money(coef int64, ccy string) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: coef}, CurrencyCode: ccy}
}

// produceState frames the risk state events into cc via a real
// bus.Producer (stamp + validate + frame), partition-keyed for ordering.
func produceState(t *testing.T, cc *captureClient) {
	t.Helper()
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "test/ingest", ProducerVersion: "v0", Tenant: "acme"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	now := time.Now()
	publish := func(subj, eventType, pk, schemaRef string, payload proto.Message) {
		t.Helper()
		if err := prod.Publish(context.Background(), bus.Event{
			Subject:          subj,
			EventType:        eventType,
			EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion:    1,
			Domain:           "risk",
			EventTime:        now,
			PartitionKey:     pk,
			PayloadSchemaRef: schemaRef,
			Payload:          payload,
		}); err != nil {
			t.Fatalf("publish %s: %v", subj, err)
		}
	}

	publish(ingest.EventTypePortfolioRevalued, ingest.EventTypePortfolioRevalued, "PORT-1", "domain.v1.PortfolioState:1", &domainpb.PortfolioState{
		PortfolioId: "PORT-1", BaseCurrency: "USD", AsOf: timestamppb.New(now),
	})
	publish(subject.PositionFor("acme", "PORT-1", "AAPL"), ingest.EventTypePositionChanged, "PORT-1", "domain.v1.PositionState:1", &domainpb.PositionState{
		PortfolioId: "PORT-1", InstrumentId: "AAPL", MarketValue: money(1000, "USD"), AsOf: timestamppb.New(now),
	})
}

func TestIngest_RoutesEventsIntoStore(t *testing.T) {
	cc := newCapture()
	produceState(t, cc)

	store := state.NewStore()
	consumer, err := bus.NewConsumer(&fakeSub{msgs: cc.out})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	ing, err := app.NewIngest(consumer, store, "", discardLogger())
	if err != nil {
		t.Fatalf("NewIngest: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ing.Run(ctx) }()

	// Poll until both the portfolio aggregate and its position landed.
	//
	// Snapshot, not Lookup: Lookup hands back the engine's LIVE portfolio, and its
	// doc comment requires the caller to hold the per-aggregate lock or clone before
	// reading. This poll runs while the ingest goroutine is applying, so reading the
	// live copy races the applier's SetPosition (domain.Portfolio is a plain map,
	// single-writer by design). Snapshot is the locked-read boundary — a deep clone
	// taken under the same per-aggregate lock the applier holds — and is what every
	// production reader (engine.go, recompute.go) already uses.
	waitFor(t, func() bool {
		p, ok := store.Snapshot("PORT-1")
		if !ok {
			return false
		}
		_, hasPos := p.Position("AAPL")
		return p.BaseCurrency() == "USD" && hasPos
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	p, _ := store.Snapshot("PORT-1")
	pos, _ := p.Position("AAPL")
	if pos.MarketValue.GetAmount().GetCoefficient() != 1000 {
		t.Errorf("position market value = %v want 1000", pos.MarketValue)
	}
}

func TestIngest_NilConsumerRejected(t *testing.T) {
	if _, err := app.NewIngest(nil, state.NewStore(), "", discardLogger()); err == nil {
		t.Fatal("expected error for nil consumer")
	}
}

func TestIngest_NilApplierRejected(t *testing.T) {
	consumer, _ := bus.NewConsumer(&fakeSub{msgs: map[string][]bus.Message{}})
	if _, err := app.NewIngest(consumer, nil, "", discardLogger()); err == nil {
		t.Fatal("expected error for nil applier")
	}
}

// Run returns nil on clean ctx cancellation (the graceful-shutdown path);
// per-subject context.Canceled is not surfaced as a failure.
func TestIngest_CleanShutdownNoError(t *testing.T) {
	consumer, _ := bus.NewConsumer(&fakeSub{msgs: map[string][]bus.Message{}})
	ing, _ := app.NewIngest(consumer, state.NewStore(), "", discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ing.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("clean shutdown returned %v want nil", err)
	}
}

// errSub fails one subject's Subscribe; Run must surface it (fail-fast)
// and cancel the siblings so Run returns rather than hanging.
type errSub struct{ failOn string }

func (e *errSub) Subscribe(ctx context.Context, subject, _ string, _ bus.Handler) error {
	if subject == e.failOn {
		return errBoom
	}
	<-ctx.Done()
	return ctx.Err()
}

var errBoom = errBoomT("boom")

type errBoomT string

func (e errBoomT) Error() string { return string(e) }

func TestIngest_SubscribeFailureSurfaces(t *testing.T) {
	consumer, _ := bus.NewConsumer(&errSub{failOn: subject.PositionAll}) // the subject the engine actually binds (EXEC-M20)
	ing, _ := app.NewIngest(consumer, state.NewStore(), "", discardLogger())

	done := make(chan error, 1)
	go func() { done <- ing.Run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected subscribe failure to surface")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after a subscribe failure (siblings not canceled)")
	}
}

// waitFor polls cond up to ~2s, failing the test on timeout. Avoids fixed
// sleeps — the goroutine applies asynchronously.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

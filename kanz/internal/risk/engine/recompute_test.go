package engine_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	risk "github.com/kanz-eng/kanz/internal/risk"
	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/engine"
	"github.com/kanz-eng/kanz/internal/risk/publish"
	"github.com/kanz-eng/kanz/internal/risk/state"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// emitCounter is a bus.Client that counts published messages per subject —
// enough to assert how many output FACTs a recompute emitted.
type emitCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newEmitCounter() *emitCounter { return &emitCounter{counts: map[string]int{}} }

func (e *emitCounter) Publish(_ context.Context, msg bus.Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counts[msg.Subject]++
	return nil
}
func (e *emitCounter) Subscribe(context.Context, string, string, bus.Handler) error { return nil }
func (e *emitCounter) Close() error                                                 { return nil }

func (e *emitCounter) count(subject string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.counts[subject]
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newPublisher(t *testing.T, cc bus.Client) *publish.Publisher {
	t.Helper()
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "risk-engine/test", ProducerVersion: "v0", Tenant: "acme"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pub, err := publish.NewPublisher(prod)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	return pub
}

// waitFor polls cond up to ~2s, failing on timeout. Avoids fixed sleeps —
// the debounce fires asynchronously on a timer goroutine.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within deadline: %s", msg)
}

func TestRecompute_CoalescesBurstAndEmits(t *testing.T) {
	store := state.NewStore()
	applyPosition(t, store, "PORT-1", "AAPL", 1000, time.Now())

	cc := newEmitCounter()
	cache := risk.NewCache()
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), cache, newPublisher(t, cc), 30*time.Millisecond, discard())
	defer r.Close()

	// A burst of triggers within the debounce window must collapse to one
	// recompute (one exposure FACT + one measures FACT).
	for i := 0; i < 6; i++ {
		r.Trigger("PORT-1")
	}

	waitFor(t, "exposure FACT emitted", func() bool {
		return cc.count(publish.EventTypeExposureRecomputed) == 1
	})
	// Give any erroneous extra fires a chance to land, then assert exactly one.
	time.Sleep(80 * time.Millisecond)
	if got := cc.count(publish.EventTypeExposureRecomputed); got != 1 {
		t.Errorf("exposure FACTs = %d want 1 (burst not coalesced)", got)
	}
	if got := cc.count(publish.EventTypeMeasuresComputed); got != 1 {
		t.Errorf("measures FACTs = %d want 1", got)
	}
	if _, ok := cache.LookupExposure("PORT-1"); !ok {
		t.Error("cache exposure not stored after recompute")
	}
	if _, ok := cache.LookupMeasures("PORT-1"); !ok {
		t.Error("cache measures not stored after recompute")
	}
}

func TestRecompute_CacheOnlyWhenNoPublisher(t *testing.T) {
	store := state.NewStore()
	applyPosition(t, store, "PORT-1", "AAPL", 1000, time.Now())

	cache := risk.NewCache()
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), cache, nil, 20*time.Millisecond, discard())
	defer r.Close()

	r.Trigger("PORT-1")
	waitFor(t, "cache populated", func() bool {
		_, ok := cache.LookupExposure("PORT-1")
		return ok
	})
}

func TestRecompute_TriggeringApplierFiresRecompute(t *testing.T) {
	store := state.NewStore()
	cache := risk.NewCache()
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), cache, nil, 20*time.Millisecond, discard())
	defer r.Close()

	ta := engine.NewTriggeringApplier(store, r)
	err := ta.ApplyPositionChanged(context.Background(),
		&envelopepb.Envelope{EventId: "e1", IdempotencyKey: "e1"},
		&domainpb.PositionState{
			PortfolioId: "PORT-1", InstrumentId: "AAPL",
			MarketValue: money(500, "USD"), AsOf: timestamppb.New(time.Now()),
		})
	if err != nil {
		t.Fatalf("ApplyPositionChanged: %v", err)
	}

	// The apply landed in the store, and the decorator triggered a recompute.
	if _, ok := store.Snapshot("PORT-1"); !ok {
		t.Fatal("apply did not reach the store")
	}
	waitFor(t, "recompute populated cache", func() bool {
		_, ok := cache.LookupExposure("PORT-1")
		return ok
	})
}

func TestRecompute_CloseStopsPendingTrigger(t *testing.T) {
	store := state.NewStore()
	applyPosition(t, store, "PORT-1", "AAPL", 1000, time.Now())

	cc := newEmitCounter()
	cache := risk.NewCache()
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), cache, newPublisher(t, cc), 50*time.Millisecond, discard())

	r.Trigger("PORT-1")
	r.Close() // stops the pending timer before it fires

	time.Sleep(120 * time.Millisecond)
	if got := cc.count(publish.EventTypeExposureRecomputed); got != 0 {
		t.Errorf("exposure FACTs = %d want 0 (Close should have stopped the timer)", got)
	}
	if _, ok := cache.LookupExposure("PORT-1"); ok {
		t.Error("cache populated despite Close stopping the trigger")
	}

	// Trigger after Close is a no-op.
	r.Trigger("PORT-1")
	time.Sleep(80 * time.Millisecond)
	if got := cc.count(publish.EventTypeExposureRecomputed); got != 0 {
		t.Errorf("exposure FACTs = %d want 0 (Trigger after Close must no-op)", got)
	}
}

// Drain flushes pending recomputes even when their debounce window has
// not elapsed — the graceful-shutdown path. A long debounce guarantees the
// recompute would NOT fire on its own within the test, so an emit proves
// Drain forced the flush, and Drain returns promptly (not after debounce).
func TestRecompute_DrainFlushesPending(t *testing.T) {
	store := state.NewStore()
	applyPosition(t, store, "PORT-1", "AAPL", 1000, time.Now())

	cc := newEmitCounter()
	cache := risk.NewCache()
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), cache, newPublisher(t, cc), 10*time.Second, discard())

	r.Trigger("PORT-1")

	start := time.Now()
	r.Drain()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Drain took %v — should flush immediately, not wait the 10s debounce", elapsed)
	}
	if got := cc.count(publish.EventTypeExposureRecomputed); got != 1 {
		t.Errorf("exposure FACTs = %d want 1 (Drain should flush the pending recompute)", got)
	}
	if _, ok := cache.LookupExposure("PORT-1"); !ok {
		t.Error("cache exposure not stored after Drain")
	}

	// Drain is idempotent and Trigger after Drain is a no-op.
	r.Drain()
	r.Trigger("PORT-1")
	if got := cc.count(publish.EventTypeExposureRecomputed); got != 1 {
		t.Errorf("exposure FACTs = %d want 1 (no new work after Drain)", got)
	}
}

func TestRecompute_EmptyPortfolioIDIgnored(t *testing.T) {
	store := state.NewStore()
	r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(), nil, 10*time.Millisecond, discard())
	defer r.Close()
	r.Trigger(v1.PortfolioID("")) // must not panic or schedule anything
	time.Sleep(30 * time.Millisecond)
}

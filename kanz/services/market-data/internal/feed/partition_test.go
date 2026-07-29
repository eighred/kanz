package feed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

// orderSink records, per instrument, the event_time seconds in arrival order,
// so a test can assert per-instrument ordering was preserved across lanes.
type orderSink struct {
	mu   sync.Mutex
	seen map[string][]int64
	n    int
}

func newOrderSink() *orderSink { return &orderSink{seen: map[string][]int64{}} }

func (o *orderSink) Publish(_ context.Context, ev *marketpb.MarketDataEvent) error {
	o.mu.Lock()
	id := ev.GetInstrumentId()
	o.seen[id] = append(o.seen[id], ev.GetEventTime().AsTime().Unix())
	o.n++
	o.mu.Unlock()
	return nil
}

func TestPartitioned_PreservesPerInstrumentOrder(t *testing.T) {
	const instruments, perInstrument = 32, 500
	rec := newOrderSink()
	p := NewPartitioned(rec, WithLanes(8), WithLaneBuffer(16))

	for j := 0; j < perInstrument; j++ {
		for i := 0; i < instruments; i++ {
			id := fmt.Sprintf("INST%02d", i)
			ev, _ := Trade(meta(id, int64(j), uint64(j+1)), dec(int64(j), 0), dec(1, 0), "")
			if err := p.Publish(context.Background(), ev); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if rec.n != instruments*perInstrument {
		t.Fatalf("received %d events, want %d", rec.n, instruments*perInstrument)
	}
	for i := 0; i < instruments; i++ {
		id := fmt.Sprintf("INST%02d", i)
		got := rec.seen[id]
		if len(got) != perInstrument {
			t.Fatalf("%s: got %d events, want %d", id, len(got), perInstrument)
		}
		for j := 1; j < len(got); j++ {
			if got[j] < got[j-1] {
				t.Fatalf("%s: order inverted at %d: %d < %d", id, j, got[j], got[j-1])
			}
		}
	}
}

// batchRecSink is a BatchSink that records how events arrived (single vs batch)
// so the batching path can be asserted.
type batchRecSink struct {
	mu       sync.Mutex
	evs      []*marketpb.MarketDataEvent
	batches  int
	maxBatch int
}

func (b *batchRecSink) Publish(_ context.Context, ev *marketpb.MarketDataEvent) error {
	b.mu.Lock()
	b.evs = append(b.evs, ev)
	b.mu.Unlock()
	return nil
}

func (b *batchRecSink) PublishBatch(_ context.Context, evs []*marketpb.MarketDataEvent) error {
	b.mu.Lock()
	b.evs = append(b.evs, evs...)
	b.batches++
	if len(evs) > b.maxBatch {
		b.maxBatch = len(evs)
	}
	// All events in one batch must share a lane ⇒ same instrument key onto one lane.
	b.mu.Unlock()
	return nil
}

func TestPartitioned_UsesBatchSinkAndCoalesces(t *testing.T) {
	rec := &batchRecSink{}
	// One lane + generous batch so a burst on one instrument coalesces into
	// multi-event batches (deterministic: single lane serializes everything).
	p := NewPartitioned(rec, WithLanes(1), WithLaneBuffer(4096), WithMaxBatch(64))

	const n = 4000
	for j := 0; j < n; j++ {
		ev, _ := Trade(meta("AAPL", int64(j), uint64(j+1)), dec(int64(j), 0), dec(1, 0), "")
		_ = p.Publish(context.Background(), ev)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(rec.evs) != n {
		t.Fatalf("received %d events, want %d", len(rec.evs), n)
	}
	if rec.batches == 0 {
		t.Fatal("expected the BatchSink path to be used")
	}
	if rec.maxBatch < 2 {
		t.Fatalf("expected some coalesced multi-event batches, max batch was %d", rec.maxBatch)
	}
	// Ordering within the single lane is total.
	for j := 0; j < n; j++ {
		if rec.evs[j].GetEventTime().AsTime().Unix() != int64(j) {
			t.Fatalf("event %d out of order: got %d", j, rec.evs[j].GetEventTime().AsTime().Unix())
		}
	}
}

// A downstream failure is recorded stickily and surfaced to the source on the
// next Publish (and at Close) — fail-fast without deadlocking the enqueue.
func TestPartitioned_StickyErrorSurfaces(t *testing.T) {
	boom := errors.New("downstream boom")
	fail := SinkFunc(func(_ context.Context, _ *marketpb.MarketDataEvent) error { return boom })
	p := NewPartitioned(fail, WithLanes(2), WithLaneBuffer(1), WithMaxBatch(1))

	var sawErr error
	for j := 0; j < 5000 && sawErr == nil; j++ {
		ev, _ := Trade(meta("AAPL", int64(j), uint64(j+1)), dec(int64(j), 0), dec(1, 0), "")
		sawErr = p.Publish(context.Background(), ev)
	}
	if sawErr == nil {
		// The worker may not have failed before the loop drained; Close still surfaces it.
		sawErr = p.Close()
	} else {
		_ = p.Close()
	}
	if !errors.Is(sawErr, boom) {
		t.Fatalf("expected sticky downstream error, got %v", sawErr)
	}
}

// Publish respects ctx cancellation while blocked on a full lane (backpressure
// is interruptible, not a hang).
func TestPartitioned_PublishRespectsContext(t *testing.T) {
	// A sink that blocks forever wedges the single lane; the buffer then fills
	// and Publish must return on ctx cancel rather than block indefinitely.
	block := make(chan struct{})
	defer close(block)
	slow := SinkFunc(func(ctx context.Context, _ *marketpb.MarketDataEvent) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil
	})
	p := NewPartitioned(slow, WithLanes(1), WithLaneBuffer(1), WithMaxBatch(1))

	ctx, cancel := context.WithCancel(context.Background())
	// Fill: 1 in-flight (wedged in the worker) + 1 buffered, then the 3rd blocks.
	for i := 0; i < 2; i++ {
		ev, _ := Trade(meta("AAPL", int64(i), uint64(i+1)), dec(1, 0), dec(1, 0), "")
		_ = p.Publish(context.Background(), ev)
	}
	done := make(chan error, 1)
	go func() {
		ev, _ := Trade(meta("AAPL", 99, 99), dec(1, 0), dec(1, 0), "")
		done <- p.Publish(ctx, ev)
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from a blocked Publish, got %v", err)
	}
}

func TestLaneFor_StableAndInRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("INST%d", i)
		l := laneFor(id, 16)
		if l < 0 || l >= 16 {
			t.Fatalf("lane %d out of range for %s", l, id)
		}
		if l != laneFor(id, 16) {
			t.Fatalf("lane not stable for %s", id)
		}
	}
}

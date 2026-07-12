package bus

// These tests encode the exact failure that motivated HealthPublisher: a producer
// whose every publish is rejected, while the service reports itself ready.

import (
	"context"
	"errors"
	"testing"
)

type fakePub struct {
	err   error
	calls int
}

func (f *fakePub) Publish(context.Context, Event) error {
	f.calls++
	return f.err
}

func TestHealthyBeforeAnyPublish(t *testing.T) {
	// A pod must be allowed to come up. Nothing published yet is not a failure.
	h := NewHealthPublisher(&fakePub{}, 3)
	if !h.Healthy() {
		t.Fatal("unhealthy before the first publish — the pod would never become ready")
	}
}

func TestTransientFailuresBelowThresholdStayHealthy(t *testing.T) {
	// A dropped connection must not flap a working producer out of its Service.
	inner := &fakePub{err: errors.New("nats: connection lost")}
	h := NewHealthPublisher(inner, 3)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_ = h.Publish(ctx, Event{})
	}
	if !h.Healthy() {
		t.Fatal("2 failures below a threshold of 3 marked the producer unhealthy — a single blip would flap the pod")
	}
}

func TestEveryPublishFailingMarksItUnhealthy(t *testing.T) {
	// THE market-ingest BUG. Every envelope rejected ("tenant_id required"); the
	// service folded its book perfectly and emitted nothing, reporting ready.
	inner := &fakePub{err: errors.New("envelope validation: tenant_id required")}
	h := NewHealthPublisher(inner, 3)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_ = h.Publish(ctx, Event{})
	}
	healthy, consecutive, lastErr := h.Status()
	if healthy {
		t.Fatal("a producer whose every publish is rejected still reports healthy — this is exactly the bug that shipped")
	}
	if consecutive != 3 {
		t.Fatalf("consecutive failures = %d, want 3", consecutive)
	}
	if lastErr == nil {
		t.Fatal("Status() must surface the failing error, or an operator has to go read logs to learn why")
	}
}

func TestOneSuccessRestoresHealth(t *testing.T) {
	// The bus came back. A producer that stayed condemned after recovering would
	// need a pod restart to rejoin its Service.
	inner := &fakePub{err: errors.New("nats: connection lost")}
	h := NewHealthPublisher(inner, 2)
	ctx := context.Background()

	_ = h.Publish(ctx, Event{})
	_ = h.Publish(ctx, Event{})
	if h.Healthy() {
		t.Fatal("still healthy after crossing the threshold")
	}

	inner.err = nil
	if err := h.Publish(ctx, Event{}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !h.Healthy() {
		t.Fatal("a successful publish did not restore health — the pod would stay out of its Service until restarted")
	}
	if h.LastSuccess().IsZero() {
		t.Fatal("LastSuccess not recorded")
	}
}

func TestPublishReturnsTheErrorUnchanged(t *testing.T) {
	// The wrapper OBSERVES. A publisher that swallowed the error to protect its own
	// health signal would be the very bug it exists to catch.
	want := errors.New("boom")
	inner := &fakePub{err: want}
	h := NewHealthPublisher(inner, 3)

	got := h.Publish(context.Background(), Event{})
	if !errors.Is(got, want) {
		t.Fatalf("Publish returned %v, want the inner error %v — the wrapper must not swallow it", got, want)
	}
	if inner.calls != 1 {
		t.Fatalf("inner publisher called %d times, want 1 — the wrapper must not intercept the publish", inner.calls)
	}
}

func TestCountsTrackBothOutcomes(t *testing.T) {
	inner := &fakePub{}
	h := NewHealthPublisher(inner, 3)
	ctx := context.Background()

	_ = h.Publish(ctx, Event{})
	inner.err = errors.New("down")
	_ = h.Publish(ctx, Event{})

	ok, failed := h.Counts()
	if ok != 1 || failed != 1 {
		t.Fatalf("counts = (%d ok, %d failed), want (1, 1)", ok, failed)
	}
}

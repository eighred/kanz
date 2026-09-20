package orderview

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
)

func TestPendingCloseSurvivesRestartRedeliveryAndTenantIsolation(t *testing.T) {
	f := newPGFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	a := NewPostgres(f.rawPool(t, "alpha"))
	ci := execution.CloseIntent{OrderID: "same-id", InstrumentID: "BTC-USD", RequestedAt: now.Add(-time.Hour)}
	if err := a.Track(ctx, ci); err != nil {
		t.Fatal(err)
	}
	// A separate pool and store model a process restart; no memory is shared.
	restarted := NewPostgres(f.rawPool(t, "alpha"))
	redelivery := ci
	redelivery.RequestedAt = now
	if err := restarted.Track(ctx, redelivery); err != nil {
		t.Fatal(err)
	}
	due, err := restarted.DueCloses(ctx, now, time.Second)
	if err != nil || len(due) != 1 || !due[0].RequestedAt.Equal(ci.RequestedAt) {
		t.Fatalf("restart/redelivery lost ownership: %v %v", due, err)
	}
	// Scheduling also survives restart; a rapid retry must not issue another query.
	if due, err := a.DueCloses(ctx, now, time.Second); err != nil || len(due) != 0 {
		t.Fatalf("retry flooded: %v %v", due, err)
	}
	b := NewPostgres(f.rawPool(t, "beta"))
	if err := b.Resolve(ctx, ci.OrderID); err != nil {
		t.Fatal(err)
	}
	if due, err := b.DueCloses(ctx, now.Add(time.Minute), 0); err != nil || len(due) != 0 {
		t.Fatalf("cross-tenant read: %v %v", due, err)
	}
	if due, err := restarted.DueCloses(ctx, now.Add(time.Minute), 0); err != nil || len(due) != 1 {
		t.Fatalf("cross-tenant deletion: %v %v", due, err)
	}
	conflict := ci
	conflict.InstrumentID = "ETH-USD"
	if err := restarted.Track(ctx, conflict); err == nil {
		t.Fatal("redelivery changed identity")
	}
	if err := restarted.Resolve(ctx, ci.OrderID); err != nil {
		t.Fatal(err)
	}
	if due, err := a.DueCloses(ctx, now.Add(time.Hour), 0); err != nil || len(due) != 0 {
		t.Fatalf("terminal close retained: %v %v", due, err)
	}
	if _, err := NewPostgres(f.rawPool(t, "")).DueCloses(ctx, now, 0); err == nil {
		t.Fatal("unscoped query succeeded")
	}
}

func TestPendingCloseConcurrentClaimsAndBoundedRetry(t *testing.T) {
	f := newPGFixture(t)
	store := NewPostgres(f.rawPool(t, "alpha"))
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.Track(ctx, execution.CloseIntent{OrderID: "o1", InstrumentID: "BTC-USD", RequestedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan int, 12)
	for range 12 {
		wg.Go(func() {
			due, err := store.DueCloses(ctx, now, 0)
			if err != nil {
				t.Error(err)
			}
			claims <- len(due)
		})
	}
	wg.Wait()
	close(claims)
	total := 0
	for n := range claims {
		total += n
	}
	if total != 1 {
		t.Fatalf("concurrent claims=%d, want 1", total)
	}
	for attempt := 2; attempt <= 12; attempt++ {
		delay := execution.CloseRetryDelay(attempt - 1)
		if due, err := store.DueCloses(ctx, now.Add(delay-time.Millisecond), 0); err != nil || len(due) != 0 {
			t.Fatalf("retry %d ran before its backoff: %v %v", attempt, due, err)
		}
		now = now.Add(delay)
		if due, err := store.DueCloses(ctx, now, 0); err != nil || len(due) != 1 {
			t.Fatalf("retry %d lost ownership: %v %v", attempt, due, err)
		}
		if due, err := store.DueCloses(ctx, now, 0); err != nil || len(due) != 0 {
			t.Fatalf("retry %d has no backoff: %v %v", attempt, due, err)
		}
	}
}

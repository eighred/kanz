package outbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

type drainPublishFunc func(context.Context, bus.Event) error

func (f drainPublishFunc) Publish(ctx context.Context, e bus.Event) error {
	return f(ctx, e)
}

type drainResult struct {
	sent int
	err  error
}

// Hold three inline publishers at the broker boundary. A background pass must
// skip their locked keys and reach the fourth publisher on the last session.
// Refuse one publication while the other three still hold their sessions: this
// forces MarkFailed, as well as Pending, to make progress without another slot.
func TestPostgresOutboxDrainsFourKeysWithinServicePool(t *testing.T) {
	const limit = 4
	pool := newLimitedPoolForSchema(t, testTenant, testSchema, limit)
	requireRestrictedRole(t, pool)
	applySchema(t)
	queue := NewPostgres(pool, ServiceOMS)
	observer := NewPostgres(newPool(t, testTenant), ServiceOMS)
	keys := []string{"fail", "cancel", "success", "background"}
	const event = "order.order.cancelled"
	for _, key := range keys {
		enqueueOwned(t, pool, ServiceOMS, fact(t, testCtx(), event, key))
	}

	ctx, stop := context.WithTimeout(testCtx(), 15*time.Second)
	defer stop()
	cancelCtx, cancelPublish := context.WithCancel(ctx)
	defer cancelPublish()
	entered := make(chan string, limit)
	release := make(map[string]chan struct{}, limit)
	for _, key := range keys {
		release[key] = make(chan struct{})
	}
	refusal := errors.New("broker rejected this publication")
	acknowledged := &recorder{}
	relay, err := NewRelay(queue, drainPublishFunc(func(ctx context.Context, e bus.Event) error {
		entered <- e.PartitionKey
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release[e.PartitionKey]:
			if e.PartitionKey == "fail" {
				return refusal
			}
			if err := acknowledged.Publish(ctx, e); err != nil {
				return err
			}
			if e.PartitionKey == "cancel" {
				// The broker already has the FACT. Cancellation prevents its
				// database mark, so the same durable row must be sent again.
				cancelPublish()
			}
			return nil
		}
	}), quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	var workers sync.WaitGroup
	defer func() { stop(); workers.Wait() }()
	results := make([]chan drainResult, limit)
	for i, key := range keys {
		result := make(chan drainResult, 1)
		results[i] = result
		workers.Add(1)
		go func() {
			defer workers.Done()
			var n int
			var err error
			switch key {
			case "background":
				n, err = relay.DrainOnce(ctx)
			case "cancel":
				n, err = relay.Flush(cancelCtx, key)
			default:
				n, err = relay.Flush(ctx, key)
			}
			result <- drainResult{n, err}
		}()
		select {
		case got := <-entered:
			if got != key {
				t.Fatalf("publisher for %s entered while awaiting %s", got, key)
			}
		case r := <-result:
			t.Fatalf("%s drain returned before publication: %+v", key, r)
		case <-ctx.Done():
			t.Fatalf("four-session liveness: %s never reached its publisher: %v", key, ctx.Err())
		}
	}
	if n := pool.Stat().AcquiredConns(); n != limit {
		t.Fatalf("expected four concurrent lock sessions, acquired %d", n)
	}
	for _, key := range keys {
		releaseLock, acquired, err := observer.LockKey(ctx, key, false)
		if acquired {
			releaseLock()
		}
		if err != nil || acquired {
			t.Fatalf("external LockKey acquired %s during publish: acquired=%v err=%v", key, acquired, err)
		}
	}

	close(release["fail"])
	r := <-results[0]
	if r.sent != 0 || !errors.Is(r.err, refusal) {
		t.Fatalf("failed publication: sent=%d err=%v", r.sent, r.err)
	}
	rows, err := observer.Pending(ctx, "fail", 1)
	if err != nil || len(rows) != 1 || rows[0].Attempts != 1 || rows[0].LastError != refusal.Error() {
		t.Fatalf("expected one durable recorded refusal, rows=%v err=%v", rows, err)
	}
	close(release["cancel"])
	r = <-results[1]
	if r.sent != 0 || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled publication: sent=%d err=%v", r.sent, r.err)
	}
	for _, i := range []int{2, 3} {
		close(release[keys[i]])
		r := <-results[i]
		if r.sent != 1 || r.err != nil {
			t.Fatalf("%s: sent=%d err=%v", keys[i], r.sent, r.err)
		}
	}

	retry, err := NewRelay(queue, acknowledged, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		unlock, acquired, err := observer.LockKey(ctx, key, false)
		if err != nil || !acquired {
			t.Fatalf("lock remained held for %s: %v", key, err)
		}
		unlock()
		n, err := retry.Flush(ctx, key)
		want := 0
		if i < 2 {
			want = 1
		}
		if n != want || err != nil {
			t.Fatalf("retry %s: sent=%d want=%d err=%v", key, n, want, err)
		}
	}
	if pending, err := queue.PendingKeys(ctx, limit); err != nil || len(pending) != 0 {
		t.Fatalf("after retry: pending=%v err=%v", pending, err)
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("drain retains %d connections after cleanup", n)
	}
	var replays int
	for _, event := range acknowledged.got {
		if event.PartitionKey == "cancel" {
			replays++
		}
	}
	if replays != 2 || len(acknowledged.got) != limit+1 {
		t.Fatalf("expected only the unmarked publication twice: replayed=%d total=%d", replays, len(acknowledged.got))
	}
}

func TestPostgresOutboxFourWaitersDrainOneKeyInOrder(t *testing.T) {
	const limit = 4
	const key = "contended"
	pool := newLimitedPoolForSchema(t, testTenant, testSchema, limit)
	requireRestrictedRole(t, pool)
	applySchema(t)
	ctx, cancel := context.WithTimeout(testCtx(), 15*time.Second)
	defer cancel()
	want := []string{"order.order.accepted", "order.order.cancelled"}
	enqueueOwned(t, pool, ServiceOMS, fact(t, ctx, want[0], key), fact(t, ctx, want[1], key))
	pub := &recorder{}
	relay, err := NewRelay(NewPostgres(pool, ServiceOMS), pub, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	// A separate session holds the same legacy LockKey until all four inline
	// drains occupy their pool. Releasing it must let the winner read and mark
	// in order even though the other three are waiting for that same key.
	observer := NewPostgres(newPool(t, testTenant), ServiceOMS)
	unlock, acquired, err := observer.LockKey(ctx, key, true)
	if err != nil || !acquired {
		t.Fatalf("seed LockKey: acquired=%v err=%v", acquired, err)
	}
	var release sync.Once
	defer release.Do(unlock)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	results := make(chan drainResult, limit)
	for range limit {
		workers.Add(1)
		go func() {
			defer workers.Done()
			n, err := relay.Flush(ctx, key)
			results <- drainResult{n, err}
		}()
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for pool.Stat().AcquiredConns() != limit {
		select {
		case <-ctx.Done():
			t.Fatalf("four waiters did not occupy the four-session pool: %v", ctx.Err())
		case <-ticker.C:
		}
	}
	release.Do(unlock)
	var sent int
	for range limit {
		r := <-results
		if r.err != nil {
			t.Fatalf("four-session liveness: contended flush failed: %v", r.err)
		}
		sent += r.sent
	}
	if sent != len(want) || !slices.Equal(pub.types(), want) {
		t.Fatalf("contended flush published %d records in order %v, want %v", sent, pub.types(), want)
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("after joining four waiters the pool retains %d sessions", n)
	}
}

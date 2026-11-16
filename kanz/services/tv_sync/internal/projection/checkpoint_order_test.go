package projection

import (
	"context"
	"sync"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Delay a committed Append response, not the insert itself. A different subject
// handler can otherwise append and fold the next sequence before this returns.
type delayedAppendLog struct {
	Log
	mu      sync.Mutex // memLog is not concurrent; the real database also uses this wrapper.
	entered chan struct{}
	release chan struct{}
}

func (l *delayedAppendLog) Append(ctx context.Context, fact Fact) (bool, int64, error) {
	l.mu.Lock()
	fresh, seq, err := l.Log.Append(ctx, fact)
	l.mu.Unlock()
	if fact.EventID == "delayed-buy" && fresh && err == nil {
		close(l.entered)
		select {
		case <-l.release:
		case <-ctx.Done():
			return false, 0, ctx.Err()
		}
	}
	return fresh, seq, err
}

func TestConcurrentAppendCannotSkipCheckpointPrefix(t *testing.T) {
	checkpointDuringAppend(t, newMemLog())
}

func TestPostgresConcurrentAppendCannotSkipCheckpointPrefix(t *testing.T) {
	pool := newPool(t, testTenant)
	var unsafe bool
	if err := pool.QueryRow(context.Background(), "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&unsafe); err != nil || unsafe {
		t.Fatalf("requires non-superuser, non-bypass PostgreSQL role: unsafe=%v err=%v", unsafe, err)
	}
	freshSchema(t, pool)
	checkpointDuringAppend(t, NewPostgresLog(pool))
}

func checkpointDuringAppend(t *testing.T, source Log) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	log := &delayedAppendLog{Log: source, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(log.release) }) }
	var handlers sync.WaitGroup
	defer func() {
		unblock()
		cancel()
		handlers.Wait()
	}()
	p := New(time.Now, nil, WithLog(log, testTenant))
	deliver(t, p, "seed", evtFilled, filledOrder("seed", "fund", "BTC", orderpb.Side_SIDE_BUY, 1, 100))
	buy := mustMarshal(t, filledOrder("buy", "fund", "BTC", orderpb.Side_SIDE_BUY, 2, 110))
	sell := mustMarshal(t, filledOrder("sell", "fund", "BTC", orderpb.Side_SIDE_SELL, 1, 150))
	first, second := make(chan error, 1), make(chan error, 1)
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		first <- p.Handle(ctx, factEnv("delayed-buy", evtFilled), buy)
	}()
	select {
	case <-log.entered:
	case <-ctx.Done():
		t.Fatal("first insert did not reach the committed response boundary")
	}
	started := make(chan struct{})
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		close(started)
		second <- p.Handle(ctx, factEnv("sell", evtFilled), sell)
	}()
	<-started
	// The old implementation completes the second handler while the first is
	// held. The fixed implementation keeps it pending. Both paths then take the
	// same checkpoint and prove its replay result, not merely mutex ownership.
	var secondFinished bool
	select {
	case err := <-second:
		secondFinished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
	}
	checkpoint := make(chan error, 1)
	go func() { checkpoint <- p.Checkpoint(ctx) }()
	select {
	case err := <-checkpoint:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("checkpoint/read lock blocked behind pending database append")
	}
	unblock()
	for _, done := range []<-chan error{first, second} {
		if done == second && secondFinished {
			continue
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("handler did not finish after the database response was released")
		}
	}
	_, checkpointSeq, exists, err := source.LoadCheckpoint(ctx)
	if err != nil || !exists {
		t.Fatalf("missing checkpoint: exists=%v err=%v", exists, err)
	}
	// The seed is the only completed prefix while the first response is held.
	// A checkpoint at 3 omits committed fact 2 forever on restart.
	if checkpointSeq != 1 {
		t.Errorf("checkpoint claims sequence %d while the first new fact is not folded; want seed prefix 1", checkpointSeq)
	}
	restarted := New(time.Now, nil, WithLog(source, testTenant))
	if err := restarted.Rehydrate(ctx); err != nil {
		t.Fatal(err)
	}
	positions, err := restarted.Positions(testTenant, "fund", time.Time{})
	if err != nil || len(positions) != 1 || positions[0].Qty != "2" {
		t.Errorf("checkpoint + tail lost a fill: positions=%+v err=%v; want 1+2-1=2", positions, err)
	}
	p.mu.RLock()
	last := p.seq
	p.mu.RUnlock()
	if last != 3 {
		t.Errorf("completed fold watermark regressed to %d; want 3", last)
	}
}

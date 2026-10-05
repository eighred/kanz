package compute

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/factormodel"
)

func TestLiveModelCommitPrecedesConcurrentVisibility(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	p := NewLiveModelProvider(factormodel.Config{Type: factormodel.Statistical, StatFactors: 2, Window: 40},
		func(context.Context, time.Time) ([]string, error) { return []string{"A", "B", "C"}, nil },
		factormodel.Providers{Returns: trendReturns{"A": .001, "B": -.0005, "C": .0002}},
		WithFitObserver(func(context.Context, *factormodel.Model) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-release
			return nil
		}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan *factormodel.Model, 32)
	wg.Go(func() { m, _ := p.Model(ctx, liveAsOf); results <- m })
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fit did not reach commit")
	}
	// A cancelled waiter must escape an in-flight commit without seeing it.
	waitCtx, stop := context.WithCancel(ctx)
	stop()
	if m, ok := p.Model(waitCtx, liveAsOf); ok || m != nil {
		t.Fatal("cancelled waiter saw uncommitted model")
	}
	for range 31 {
		wg.Go(func() { m, _ := p.Model(ctx, liveAsOf); results <- m })
	}
	close(release)
	wg.Wait()
	close(results)
	var first *factormodel.Model
	for m := range results {
		if m == nil {
			t.Fatal("fit unavailable after successful commit")
		}
		if first == nil {
			first = m
		}
		if first != m {
			t.Fatal("concurrent requests received different instances")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("commits = %d, want 1", calls.Load())
	}
}

func TestLiveModelFailedCommitIsNotCached(t *testing.T) {
	var observed []*factormodel.Model
	calls := 0
	p := cadenceProvider(t, time.Hour, &observed, &calls)
	attempts := 0
	p.onFit = func(context.Context, *factormodel.Model) error {
		attempts++
		if attempts == 1 {
			return errors.New("durable storage unavailable")
		}
		return nil
	}
	if m, ok := p.Model(context.Background(), liveAsOf); ok || m != nil {
		t.Fatal("failed commit returned model")
	}
	if len(p.cache) != 0 {
		t.Fatal("failed commit populated cache")
	}
	if _, ok := p.Model(context.Background(), liveAsOf); !ok {
		t.Fatal("retry did not recover")
	}
	if attempts != 2 {
		t.Fatalf("commit attempts = %d", attempts)
	}
}

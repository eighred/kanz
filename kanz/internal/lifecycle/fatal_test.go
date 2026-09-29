package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestFatalCleanShutdownIsZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFatal(cancel)

	if got := f.Code(); got != 0 {
		t.Fatalf("a Fatal that was never raised must be a clean shutdown: Code() = %d, want 0", got)
	}
	if err := f.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	// A nil raise is a stop without a failure — the caller could not tell whether
	// its shutdown trigger was an error, and must not be charged a 1 for it.
	f.Raise(nil)
	<-ctx.Done()
	if got := f.Code(); got != 0 {
		t.Fatalf("Raise(nil) must stop without marking a failure: Code() = %d, want 0", got)
	}
}

func TestFatalRaiseStopsAndReportsOne(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFatal(cancel)

	boom := errors.New("sink could neither handle nor dead-letter a message")
	f.Raise(boom)

	// Raising must actually bring the service down, not merely record. If it did
	// not cancel, a composition root blocked on <-ctx.Done() would hang forever
	// holding a fatal nobody ever reads.
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Raise did not cancel the context — the service would never shut down")
	}
	if !errors.Is(f.Err(), boom) {
		t.Fatalf("Err() = %v, want %v", f.Err(), boom)
	}
	if got := f.Code(); got != 1 {
		t.Fatalf("a raised fatal must not exit 0 — that is what makes it indistinguishable "+
			"from a graceful SIGTERM (#266): Code() = %d, want 1", got)
	}
}

func TestFatalKeepsTheFirstError(t *testing.T) {
	f := NewFatal(func() {})

	first := errors.New("the cause")
	second := errors.New("a consequence of the cancellation the first one triggered")
	f.Raise(first)
	f.Raise(second)
	f.Raise(nil)

	if !errors.Is(f.Err(), first) {
		t.Fatalf("the first raised error must win — later ones describe the shutdown, not its "+
			"cause: Err() = %v, want %v", f.Err(), first)
	}
}

// TestFatalConcurrentRaise exercises the mutex. It cannot PROVE race-freedom
// without -race, which does not run on the usual development box (see AGENTS.md),
// so treat a pass as "the ordering is written down", not as a concurrency proof.
func TestFatalConcurrentRaise(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFatal(cancel)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Raise(errors.New("concurrent"))
		}()
	}
	wg.Wait()

	if f.Err() == nil {
		t.Fatal("Err() = nil after 32 concurrent Raise calls")
	}
	if got := f.Code(); got != 1 {
		t.Fatalf("Code() = %d, want 1", got)
	}
}

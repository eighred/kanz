package app_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	risk "github.com/kanz-eng/kanz/internal/risk"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/engine"
	"github.com/kanz-eng/kanz/internal/risk/publish"
	"github.com/kanz-eng/kanz/internal/risk/state"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/app"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/server"
)

// recordingCloser records how many times Close was called.
type recordingCloser struct {
	mu     sync.Mutex
	closed int
}

func (c *recordingCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}
func (c *recordingCloser) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// TestApp_FullChainThenGracefulShutdown drives the whole ORCH-01 chain:
// bus delivery → Ingestor → TriggeringApplier → Store apply + recompute →
// publish, then a signal-driven graceful shutdown that flushes and closes.
func TestApp_FullChainThenGracefulShutdown(t *testing.T) {
	// Input: framed state events the fakeSub replays into the consumer.
	inputCC := newCapture()
	produceState(t, inputCC)
	consumer, err := bus.NewConsumer(&fakeSub{msgs: inputCC.out})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	// Output: the recomputer's publisher emits risk FACTs here.
	outputCC := newCapture()
	prod, err := bus.NewProducer(outputCC, bus.ProducerConfig{Source: "risk-engine/test", ProducerVersion: "v0"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	publisher, err := publish.NewPublisher(prod)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	store := state.NewStore()
	// baseCtx is app-scoped (Background), NOT the run ctx — so Drain can
	// still publish after the shutdown signal cancels the run ctx.
	recomputer := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(),
		risk.NewCache(), publisher, 30*time.Millisecond, discardLogger())
	triggering := engine.NewTriggeringApplier(store, recomputer)

	ingest, err := app.NewIngest(consumer, triggering, "", discardLogger())
	if err != nil {
		t.Fatalf("NewIngest: %v", err)
	}

	readiness := &server.Readiness{}
	closer := &recordingCloser{}
	var checkpointed bool
	a := &app.App{
		Readiness:       readiness,
		Ingest:          ingest,
		Recomputer:      recomputer,
		Closers:         []io.Closer{closer},
		Checkpoint:      func(context.Context) error { checkpointed = true; return nil },
		ShutdownTimeout: 2 * time.Second,
		Logger:          discardLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	// Becomes ready, and the events flow through into the store.
	waitFor(t, func() bool { return readiness.Ready() })
	waitFor(t, func() bool {
		p, ok := store.Snapshot("PORT-1")
		if !ok {
			return false
		}
		_, hasPos := p.Position("AAPL")
		return hasPos
	})

	cancel() // signal graceful shutdown

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after shutdown signal")
	}

	if readiness.Ready() {
		t.Error("readiness still true after shutdown")
	}
	if !checkpointed {
		t.Error("checkpoint hook not called during shutdown")
	}
	if closer.count() != 1 {
		t.Errorf("transport Close called %d times, want 1", closer.count())
	}
	// Drain flushed the recompute → at least one exposure FACT published.
	if outputCC.count(publish.EventTypeExposureRecomputed) < 1 {
		t.Error("no exposure FACT published — recompute/drain did not emit")
	}
}

// captureClient.count helper for the output assertions.
func (c *captureClient) count(subject string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.out[subject])
}

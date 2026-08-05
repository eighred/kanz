package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/services/risk-engine/internal/server"
)

// DefaultShutdownTimeout bounds the graceful-shutdown sequence so a wedged
// broker can't hold the pod past its k8s terminationGracePeriod — if drain
// overruns, the process exits and SIGKILL backstops it.
//
// IT IS ALSO THE LARGEST SINGLE TERM IN THE ESTATE'S DERIVED DRAIN BUDGET, and
// it is not spelled in cmd/risk-engine/main.go. Until #264 the guard that
// derives every manifest's terminationGracePeriodSeconds read only the mains,
// so this 20s was invisible: raising it to 120 moved the derived budget by zero
// seconds while the pod would have spent every one of them. It is now declared
// in serviceShutdownExtras in test/arch/pod_disruption_and_drain_test.go, which
// reads the number from THIS line — so changing it moves the floor every
// manifest is checked against, and renaming it fails that guard rather than
// silently dropping the term.
const DefaultShutdownTimeout = 20 * time.Second

// App is the risk-engine's runtime lifecycle: it starts ingestion, flips
// the readiness gate, and on shutdown runs the ordered drain — fail
// readiness → stop consumers → drain in-flight applies → flush debounced
// recomputes → checkpoint → flush+close transports.
//
// # Why this order
//
//  1. Readiness off first so the load balancer/k8s endpoint controller
//     pulls the pod from rotation before anything is torn down — in-flight
//     work finishes, but no NEW external traffic arrives.
//  2. Stop consumers (cancel the ingest ctx). bus.Consumer dispatches one
//     delivery at a time synchronously, so a returning Subscribe means the
//     in-flight apply already completed — that is the "drain applies" step.
//  3. Drain recomputes: with no more applies arriving, flush the debounced
//     work so the LAST settled state is published (not dropped). The
//     Recomputer's baseCtx must outlive this, so it is constructed with an
//     app-scoped context, not the signal context.
//  4. Checkpoint durable state (PERS-01 seam; nil until that lands).
//  5. Close transports last so the publisher stays usable through step 3
//     and Kafka batches flush on close.
type App struct {
	Readiness  *server.Readiness
	Ingest     *Ingest
	Recomputer *engine.Recomputer
	// Closers are the transports (bus client(s)) closed last; Close flushes
	// any batched writes.
	Closers []io.Closer
	// Checkpoint persists durable state on shutdown. nil ⇒ skipped (the
	// PERS-01 seam — risk-engine state is in-memory until then).
	Checkpoint func(context.Context) error
	// ShutdownTimeout bounds the drain; ≤0 ⇒ DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
}

// Run starts ingestion, marks the service ready, and blocks until ctx is
// canceled (shutdown signal) or ingestion fails. Either way it runs the
// graceful drain before returning. The returned error is the ingestion
// failure, if any; a clean signal-driven shutdown returns nil.
func (a *App) Run(ctx context.Context) error {
	if a.Logger == nil {
		a.Logger = slog.Default()
	}
	timeout := a.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}

	ingestErr := make(chan error, 1)
	go func() { ingestErr <- a.Ingest.Run(ctx) }()

	a.Readiness.Set(true)
	a.Logger.Info("risk-engine ready")

	var runErr error
	select {
	case <-ctx.Done():
		a.Logger.Info("shutdown signal received")
	case err := <-ingestErr:
		runErr = err
		ingestErr = nil // already drained
		a.Logger.Error("ingestion stopped unexpectedly", "err", err)
	}

	a.gracefulShutdown(ingestErr, timeout)
	return runErr
}

// gracefulShutdown runs the ordered drain under a watchdog timeout.
// ingestErr is non-nil only when ingestion is still running (the
// signal-driven path) — we wait for it to stop before draining recomputes.
func (a *App) gracefulShutdown(ingestErr chan error, timeout time.Duration) {
	a.Readiness.Set(false) // 1. fail readiness — stop receiving new traffic

	done := make(chan struct{})
	go func() {
		defer close(done)

		// 2. consumers were signaled to stop via the canceled ctx; wait for
		//    Subscribe to return, which means in-flight applies have drained.
		if ingestErr != nil {
			if err := <-ingestErr; err != nil && !errors.Is(err, context.Canceled) {
				a.Logger.Error("ingestion shutdown error", "err", err)
			}
		}

		// 3. flush debounced recomputes so the last settled state publishes.
		a.Recomputer.Drain()

		// 4. checkpoint durable state (PERS-01 seam).
		if a.Checkpoint != nil {
			cctx, cancel := context.WithTimeout(context.Background(), timeout)
			if err := a.Checkpoint(cctx); err != nil {
				a.Logger.Error("checkpoint failed", "err", err)
			}
			cancel()
		}

		// 5. flush + close transports.
		for _, c := range a.Closers {
			if err := c.Close(); err != nil {
				a.Logger.Error("transport close error", "err", err)
			}
		}
	}()

	select {
	case <-done:
		a.Logger.Info("graceful shutdown complete")
	case <-time.After(timeout):
		a.Logger.Warn("graceful shutdown timed out; exiting", "timeout", timeout)
	}
}

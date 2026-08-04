// market-ingest is the native-alpha edge of the platform (Milestone 5
// foundation). It dials exchange L2 depth feeds, folds them into per-instrument
// in-memory books off the bus (the hot path), and publishes bounded periodic
// OrderBookSnapshots for durable replay and audit. It is process-isolated from
// the OMS: the raw full-rate depth feed lives and dies inside this process; only
// snapshots — and, in a later slice, the native engines' signal.v1.
// StrategySignals — cross the bus. The default binary is vendor-free and tracks
// each configured instrument with a deterministic simulator; real venue depth
// sources bind behind per-venue build tags, exactly like the OMS connectors.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/alpha"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/market-ingest/internal/config"
)

func main() {
	// The lifecycle lives in run() because os.Exit skips defers: every defer
	// run() registers fires before this line. The non-zero code is what makes a
	// fatal halt distinguishable from a graceful SIGTERM — both otherwise exit 0
	// with reason "Completed" in the pod's termination record (#266).
	// 2 = startup failure, 1 = run loop died after startup, 0 = clean shutdown.
	os.Exit(run())
}

func run() int {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Records WHY we are stopping so the exit code can say so. Every
	// fatal.Raise call below already brought the process down via stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "market-ingest",
		ServiceVersion: version.String(),
		OTLPEndpoint:   cfg.OTLPEndpoint,
		SampleRatio:    1,
	}, base)
	if err != nil {
		slog.Default().Error("observability init failed", "err", err)
		return 2
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("spiffe source init failed", "err", err)
		return 2
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		logger.Error("bus dial failed", "err", err)
		return 2
	}
	defer func() { _ = client.Close() }()

	rawProducer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version.String(),
		// MT-01b: without this, bus.Validate rejects every envelope
		// ("tenant_id required") and the service publishes NOTHING while still
		// reporting ready. The book folds; nothing leaves.
		Tenant:  cfg.Tenant,
		Metrics: bus.NewBusMetrics(obs.Registry),
	})
	if err != nil {
		logger.Error("producer init failed", "err", err)
		return 2
	}

	// Every FACT this service emits goes through here, so this is what /readyz
	// asks: is publishing actually WORKING? A market-ingest that folds its book
	// perfectly and lands nothing on the bus is not ready, however alive it looks.
	publishHealth := bus.NewHealthPublisher(rawProducer, bus.DefaultPublishFailureThreshold)

	// Health/readiness endpoint for the orchestrator.
	ready := &readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           healthMux(ready, publishHealth, obs.MetricsHandler()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("market-ingest health listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	if len(cfg.Instruments) == 0 {
		logger.Warn("no instruments configured — idling (set MARKET_INGEST_INSTRUMENTS)")
	}

	// The alpha Runner owns the edge: it folds each feed's depth into an in-memory
	// book and its trades into a tape (both off the bus), publishes the bounded
	// periodic snapshots, and ticks whatever engines are registered.
	//
	// THIS binary registers NONE. It is the open edge: it folds, it snapshots, and
	// it decides nothing. The proprietary engines live in the restricted layer,
	// which imports pkg/alpha and runs this same Runner with its engines supplied —
	// so the code that touches the market is identical either way, and only the
	// decision-making differs.
	// A market-ingest with nothing real to ingest is a HARD failure now, not a
	// silent swap to generated prices (see feeds()).
	srcs, err := feeds(cfg, logger)
	if err != nil {
		// Refusing to start beats starting and publishing invented prices that
		// risk, NAV and pricing will all mark against.
		logger.Error("market-ingest cannot start", "err", err)
		return 2
	}
	runner, err := alpha.New(alpha.Config{
		Feeds:            srcs,
		Publisher:        publishHealth,
		TradeRetention:   cfg.TradeRetention,
		SnapshotInterval: cfg.SnapshotInterval,
		SnapshotDepth:    cfg.SnapshotDepth,
		Logger:           logger,
		// Book snapshots publish off a ticker with no inbound envelope, so they
		// carry no tenant unless one is stamped here. This is the same value the
		// producer's own fallback supplies today, made explicit so the snapshot
		// path stops depending on how this service happens to wire its producer.
		MarketDataTenant: cfg.Tenant,
	})
	if err != nil {
		logger.Error("alpha runner init failed", "err", err)
		return 2
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := runner.Run(ctx); err != nil {
			logger.Error("market edge stopped", "err", err)
			fatal.Raise(err)
		}
	}()
	ready.set(true)

	<-ctx.Done()
	ready.set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
	wg.Wait()

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	return fatal.Code()
}

type readiness struct {
	mu sync.RWMutex
	ok bool
}

func (r *readiness) set(v bool) { r.mu.Lock(); r.ok = v; r.mu.Unlock() }
func (r *readiness) get() bool  { r.mu.RLock(); defer r.mu.RUnlock(); return r.ok }

// healthMux serves the probes. /readyz means "this service is DOING ITS JOB", not
// merely "the process is up".
//
// health is the publish-health tracker. A market-ingest whose every publish is
// rejected folds its book perfectly and emits NOTHING — and that is exactly how it
// shipped: readyz answered 200 while not one FACT reached the bus. Its FACTs are
// the only reason it exists, so if none of them land, it is not ready, and the
// kubelet should pull it out of its Service and let someone notice.
//
// health may be nil (before the producer is built), which reads as "no publish
// problem observed".
func healthMux(ready *readiness, health *bus.HealthPublisher, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	// Liveness is still just "the process is up" — restarting a pod does not fix a
	// bad tenant or a missing stream, and a crash-loop would only hide the reason.
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.get() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready: starting up")
			return
		}
		if health != nil {
			if ok, consecutive, lastErr := health.Status(); !ok {
				// Say WHY. A bare 503 sends someone to the logs to find what this
				// endpoint already knows.
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, "not ready: %d consecutive publish failures — this service is ingesting and emitting NOTHING. last error: %v",
					consecutive, lastErr)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ready")
	})
	if metrics != nil {
		mux.Handle("/metrics", metrics)
	}
	return mux
}

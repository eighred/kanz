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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/book"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/ingest"
)

func main() {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "market-ingest",
		ServiceVersion: version(),
		OTLPEndpoint:   cfg.OTLPEndpoint,
		SampleRatio:    1,
	}, base)
	if err != nil {
		slog.Default().Error("observability init failed", "err", err)
		os.Exit(2)
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		logger.Error("bus dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version(),
		Metrics:         bus.NewBusMetrics(obs.Registry),
	})
	if err != nil {
		logger.Error("producer init failed", "err", err)
		os.Exit(2)
	}

	// Health/readiness endpoint for the orchestrator.
	ready := &readiness{}
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: healthMux(ready, obs.MetricsHandler()), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("market-ingest health listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	if len(cfg.Instruments) == 0 {
		logger.Warn("no instruments configured — idling (set MARKET_INGEST_INSTRUMENTS)")
	}

	// One book + engine per instrument PER VENUE. Depth is per-venue and books are
	// never merged across venues, so a build carrying both exchange tags folds two
	// independent books for the same instrument — the shape the cross-venue engines
	// read. The default (vendor-free) build gets the deterministic simulator.
	var wg sync.WaitGroup
	for _, instrument := range cfg.Instruments {
		for _, vs := range depthSources(cfg, instrument, logger) {
			b := book.New(instrument, instrument, vs.mic)
			eng := ingest.New(ingest.Config{
				Book: b, Source: vs.src, Publisher: producer, Logger: logger,
				SnapshotInterval: cfg.SnapshotInterval, SnapshotDepth: cfg.SnapshotDepth,
			})
			wg.Add(1)
			go func(inst, mic string) {
				defer wg.Done()
				if err := eng.Run(ctx); err != nil {
					logger.Error("depth engine stopped", "instrument", inst, "venue", mic, "err", err)
				}
			}(instrument, vs.mic)
		}
	}
	ready.set(true)

	<-ctx.Done()
	ready.set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
	wg.Wait()
}

type readiness struct {
	mu sync.RWMutex
	ok bool
}

func (r *readiness) set(v bool) { r.mu.Lock(); r.ok = v; r.mu.Unlock() }
func (r *readiness) get() bool  { r.mu.RLock(); defer r.mu.RUnlock(); return r.ok }

func healthMux(ready *readiness, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.get() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if metrics != nil {
		mux.Handle("/metrics", metrics)
	}
	return mux
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "dev"
}

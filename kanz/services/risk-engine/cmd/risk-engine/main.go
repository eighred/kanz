// risk-engine binary entrypoint. Wires the ingestion → recompute → publish
// pipeline (ORCH-01b/c/d) behind the readiness gate and graceful-shutdown
// lifecycle (ORCH-01a/e), plus durable state: a restart restores the latest
// snapshot + replays the log (PERS-01d) and a periodic snapshotter keeps the
// durable copy fresh (PERS-01c). With no RISK_ENGINE_DATABASE_URL, state is
// in-memory and re-baselines from the live spine on restart.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	risk "github.com/kanz-eng/kanz/internal/risk"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/engine"
	"github.com/kanz-eng/kanz/internal/risk/publish"
	"github.com/kanz-eng/kanz/internal/risk/state"
	"github.com/kanz-eng/kanz/internal/risk/state/persist"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/app"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/config"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Telemetry foundation (OBS-01a): Prometheus registry + OTel tracer +
	// trace-correlated logger. Spans export to RISK_ENGINE_OTLP_ENDPOINT when
	// set; metrics are scraped from /metrics below.
	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    cfg.Source,
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

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("risk-engine listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	if cfg.NATSURL != "" {
		if err := runEngine(ctx, cfg, readiness, logger, obs); err != nil {
			logger.Error("engine stopped with error", "err", err)
		}
	} else {
		// No broker configured — serve probes only.
		readiness.Set(true)
		logger.Warn("no RISK_ENGINE_NATS_URL set — running http-only (no ingestion)")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// runEngine builds the ingestion→recompute→publish pipeline over the live
// NATS spine and runs it under the graceful-shutdown lifecycle. Returns
// when ctx is canceled (signal) or ingestion fails.
func runEngine(ctx context.Context, cfg config.Config, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)
	riskMetrics := engine.NewMetrics(obs.Registry)

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		return err
	}

	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: cfg.Source, ProducerVersion: version(), Metrics: busMetrics})
	if err != nil {
		return err
	}
	publisher, err := publish.NewPublisher(producer)
	if err != nil {
		return err
	}

	store := state.NewStore()
	// Recomputer baseCtx is app-scoped (Background), not the signal ctx, so
	// the shutdown Drain can still publish the last settled state after the
	// signal cancels ingestion.
	recomputer := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(),
		risk.NewCache(), publisher, engine.DefaultDebounceInterval, logger, engine.WithMetrics(riskMetrics))

	a := &app.App{
		Readiness:  readiness,
		Recomputer: recomputer,
		Closers:    []io.Closer{client},
		Logger:     logger,
	}

	// Durable state (PERS-01): restore + replay before live ingestion, and
	// run the periodic snapshotter + final-checkpoint drain. Skipped when no
	// database is configured — state stays in-memory.
	if cfg.DatabaseURL != "" {
		pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		sink := persist.NewPostgres(pool)

		// Bootstrap applies through the BARE store (no recompute storm).
		boot, err := app.NewBootstrap(store, sink, store, cfg.KafkaBrokers, logger)
		if err != nil {
			return err
		}
		if err := boot.Run(ctx); err != nil {
			return err
		}

		snap := engine.NewSnapshotter(store, sink, nil, 0, logger)
		go func() { _ = snap.Run(ctx) }()
		a.Checkpoint = snap.Checkpoint
	}

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics))
	if err != nil {
		return err
	}
	ingest, err := app.NewIngest(consumer, engine.NewTriggeringApplier(store, recomputer), "", logger)
	if err != nil {
		return err
	}
	a.Ingest = ingest

	return a.Run(ctx)
}

// version is the producer_version stamped on emitted events. Hardcoded
// until the build injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }

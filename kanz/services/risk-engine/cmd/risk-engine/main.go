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
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	risk "github.com/kanz-eng/kanz/internal/risk"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/engine"
	"github.com/kanz-eng/kanz/internal/risk/ingest"
	"github.com/kanz-eng/kanz/internal/risk/publish"
	"github.com/kanz-eng/kanz/internal/risk/state"
	"github.com/kanz-eng/kanz/internal/risk/state/persist"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/pkg/transport"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/app"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/config"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/grpcsrv"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/server"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/shard"
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

	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: cfg.Source, ProducerVersion: version(), Tenant: cfg.Tenant, Metrics: busMetrics})
	if err != nil {
		return err
	}
	publisher, err := publish.NewPublisher(producer)
	if err != nil {
		return err
	}

	store := state.NewStore()
	// Cache + registry are shared between the recompute path and the query
	// EngineImpl below, so a query sees the live store plus the same
	// last-known-good cache the recomputer fills (degraded fallback).
	cache := risk.NewCache()
	registry := compute.DefaultRegistry()
	// Recomputer baseCtx is app-scoped (Background), not the signal ctx, so
	// the shutdown Drain can still publish the last settled state after the
	// signal cancels ingestion.
	recomputer := engine.NewRecomputer(context.Background(), store, registry,
		cache, publisher, engine.DefaultDebounceInterval, logger, engine.WithMetrics(riskMetrics))

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
		// MT-01d: every connection carries the engine's tenant as the
		// `app.tenant_id` GUC, so Postgres RLS scopes all state reads/writes to
		// it (the authenticated-session-GUC pattern). The engine is single-tenant
		// per deployment (cfg.Tenant); a non-superuser DB role is required for
		// FORCE RLS to apply.
		poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
		if err != nil {
			return err
		}
		tenant := cfg.Tenant
		poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
			return err
		}
		pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
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

		snap := engine.NewSnapshotter(store, sink, nil, cfg.SnapshotInterval, logger)
		go func() { _ = snap.Run(ctx) }()
		a.Checkpoint = snap.Checkpoint
	}

	// PARITY-05c: under N replicas, switch cross-pod dedup to the shared Redis
	// store when configured (redis build + RISK_ENGINE_REDIS_URL); otherwise the
	// per-instance in-memory window stands. nil Deduper is ignored by WithDeduper.
	deduper, dedupCloser := newDeduper(cfg, logger)
	if dedupCloser != nil {
		a.Closers = append(a.Closers, dedupCloser)
	}
	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDeduper(deduper))
	if err != nil {
		return err
	}
	// PARITY-05a: shard the recompute fan-out. Each replica owns a
	// consistent-hash slice of portfolios; the ShardFilter drops events for
	// portfolios it does not own so their RISK-05 state + recompute stay pinned
	// to one replica. Sharded replicas must each SEE every event, so they
	// subscribe under a per-replica (broadcast) consumer group rather than the
	// shared partition-balanced one. Unsharded (no members / no self) ⇒ owns
	// everything on the shared group, exactly the pre-05a path.
	var applier ingest.Applier = engine.NewTriggeringApplier(store, recomputer)
	group := ""
	assign := shard.NewAssignment(shard.NewRing(cfg.ShardMembers, 0), cfg.ShardSelf)
	if assign.Sharded() && cfg.ShardSelf != "" {
		applier = app.NewShardFilter(applier, assign)
		group = app.DefaultConsumerGroup + "-" + cfg.ShardSelf
		logger.Info("risk-engine sharding enabled", "self", cfg.ShardSelf, "members", cfg.ShardMembers)
	}
	ingest, err := app.NewIngest(consumer, applier, group, logger)
	if err != nil {
		return err
	}
	a.Ingest = ingest

	// Risk query gRPC server (API-01b): a read surface over the concrete
	// EngineImpl, sharing the live store + cache + registry. Started only when
	// an address is configured; stopped gracefully when ctx is canceled.
	if cfg.GRPCListen != "" {
		engineImpl := engine.New(store, registry, cache, risk.NewDetector())
		stopGRPC, err := serveQueryGRPC(ctx, cfg, engineImpl, logger)
		if err != nil {
			return err
		}
		defer stopGRPC()
	}

	return a.Run(ctx)
}

// serveQueryGRPC starts the risk query gRPC server on cfg.GRPCListen and
// returns a stop function that gracefully drains it. When cfg.SPIFFESocket is
// set, the listener requires mTLS with an in-mesh peer SVID (SEC-01b);
// otherwise it serves plaintext (local/dev). Serve runs on its own goroutine;
// a bind failure is returned synchronously so startup fails loudly.
func serveQueryGRPC(ctx context.Context, cfg config.Config, eng *engine.EngineImpl, logger *slog.Logger) (func(), error) {
	var opts []grpc.ServerOption
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			return nil, err
		}
		opts = append(opts, transport.ServerOption(src, transport.AuthorizeMesh()))
		logger.Info("risk query gRPC: mTLS enabled", "socket", cfg.SPIFFESocket)
	} else {
		logger.Warn("risk query gRPC: serving plaintext (no RISK_ENGINE_SPIFFE_SOCKET)")
	}

	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		return nil, err
	}
	grpcSrv := grpc.NewServer(opts...)
	grpcsrv.New(eng).Register(grpcSrv)

	go func() {
		logger.Info("risk query gRPC listening", "addr", cfg.GRPCListen)
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Error("risk query gRPC server failed", "err", err)
		}
	}()
	return grpcSrv.GracefulStop, nil
}

// version is the producer_version stamped on emitted events. Hardcoded
// until the build injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }

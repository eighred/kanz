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

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/eighred/kanz/internal/marketdata/returns"
	mdstore "github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/prediction"
	risk "github.com/eighred/kanz/internal/risk"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/pricing/livequote"
	"github.com/eighred/kanz/internal/risk/pricing/schedule"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/risk-engine/internal/app"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
	"github.com/eighred/kanz/services/risk-engine/internal/grpcsrv"
	"github.com/eighred/kanz/services/risk-engine/internal/server"
	"github.com/eighred/kanz/services/risk-engine/internal/shard"
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
		ServiceVersion: version.String(),
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

	// SEC-M3: ONE workload identity for everything in this process that speaks
	// mTLS — the NATS spine below and the query gRPC listener (serveQueryGRPC).
	// Built once because an X509Source is a live rotation watcher, not a value.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return err
	}
	defer func() { _ = mesh.Close() }()
	// Whether a pod is on mTLS must be visible, not inferred: a plaintext dial
	// against the production broker fails at the handshake, and this line is what
	// says why. mesh.Client is nil when disabled — the dev/plaintext path.
	logger.Info("bus transport", "mtls", mesh.Enabled())

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		return err
	}

	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: cfg.Source, ProducerVersion: version.String(), Tenant: cfg.Tenant, Metrics: busMetrics})
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
	// RISK-12: with a market-data price store configured, override the RISK-07
	// 1%×gross VaR99 placeholder with the real historical-simulation model, read
	// point-in-time-correct off the shared price history (MODEL-01b) the
	// market-data service writes. The registry is shared by the recomputer below
	// and the query EngineImpl, so both serve the same data-driven VaR once
	// registered. Bound over an app-scoped ctx (Background), matching the
	// recomputer's baseCtx, so the debounced/async recompute + shutdown drain can
	// still read the store. Without a DSN the placeholder stands — the honest
	// no-market-data fallback (varmodel keeps compute.VaR99).
	if cfg.MarketDataURL != "" {
		pricePool, err := pgxpool.New(ctx, cfg.MarketDataURL)
		if err != nil {
			return err
		}
		defer pricePool.Close()
		priceStore := mdstore.NewPostgres(pricePool)
		if err := priceStore.Ping(ctx); err != nil {
			return err
		}
		provider := returns.NewStoreReturnsProvider(priceStore, returns.ReturnsConfig{})
		varmodel.Register(context.Background(), registry, provider, varmodel.Config{})
		logger.Info("RISK-12/RISK-M1: historical-simulation VaR99 + ES99 + MaxDrawdown(+Amount) registered off market-data price store")
	} else {
		logger.Warn("no RISK_ENGINE_MARKETDATA_DATABASE_URL — VaR99 serves the RISK-07 1%×gross placeholder")
	}
	// THE AI LAYER GETS ITS INPUT (AI-M1).
	//
	// internal/prediction shipped a feature publisher, a resilient inference client and a
	// model registry — and had ZERO IMPORTERS outside its own tests. Nothing computed a
	// feature, nothing published one, nothing consumed a prediction. The platform's whole AI
	// capability was a well-specified contract with no traffic on it, and the brief's next
	// fifteen layers were all to be built on top of it.
	//
	// KANZ_BRAIN's hybrid design says features are computed in GO — the engine holds the
	// source state — and scored in PYTHON, where the model serving stack lives. This is the
	// Go half, finally connected: every recompute turns the engine's OWN measures
	// (GrossExposure, VaR99) into a FeatureVector and publishes it on
	// inference.feature.computed, which is the subject the Python streaming worker has
	// always subscribed to.
	//
	// It is an OBSERVER, not a dependency. It runs after the risk FACTs are emitted, and a
	// publish failure is logged and dropped — a model that cannot be scored must never stop
	// the engine from computing, publishing and serving the risk numbers the platform
	// actually trades on. A prediction is an observation, never an order.
	recomputeOpts := []engine.RecomputerOption{engine.WithMetrics(riskMetrics)}
	features, err := prediction.NewPublisher(producer)
	if err != nil {
		return err
	}
	recomputeOpts = append(recomputeOpts,
		engine.WithMeasureObserver(app.PublishFeaturesOn(features, logger)))
	logger.Info("AI-M1: publishing risk features for scoring",
		"subject", prediction.EventTypeFeatureComputed,
		"feature_set", app.FeatureSetPortfolioRisk)

	// Recomputer baseCtx is app-scoped (Background), not the signal ctx, so
	// the shutdown Drain can still publish the last settled state after the
	// signal cancels ingestion.
	recomputer := engine.NewRecomputer(context.Background(), store, registry,
		cache, publisher, engine.DefaultDebounceInterval, logger, recomputeOpts...)

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
		pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
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
		// Restore+replay lands STATE but never triggers a recompute (by design —
		// see Bootstrap's doc comment). Without this, the pushed risk.* FACT for
		// whatever changed right before a crash is lost: Ingestor.Handler already
		// acked that bus delivery when the state mutation committed, and the
		// debounced recompute that would have emitted its FACT never got to run.
		// Arm exactly one recompute per restored portfolio now that replay has
		// settled — see ArmPostBootstrapRecomputes for why this is not the
		// per-event storm the bare-store replay avoids.
		app.ArmPostBootstrapRecomputes(store, recomputer, logger)

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
	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDeduper(deduper), bus.WithDLQ(client))
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

	// Calibration scheduler (WIRE-01c): the risk module's composition root owns
	// the live curve calibration loop. Subscribe the market quote spine into a
	// latest-quote cache and drive curve.Calibrator.Refresh on a nightly +
	// intraday cadence; a failed calibration deny-on-garbages (prior curve keeps
	// serving). Auxiliary to core risk ingest — a market-subscription failure
	// degrades calibration (no fresh curve), it does not bring the engine down.
	if cfg.CalibrationInterval > 0 && cfg.CalibrationRates != "" {
		if err := startCalibration(ctx, cfg, client, busMetrics, logger); err != nil {
			logger.Error("calibration scheduler disabled", "err", err)
		}
	}

	// Risk query gRPC server (API-01b): a read surface over the concrete
	// EngineImpl, sharing the live store + cache + registry. Started only when
	// an address is configured; stopped gracefully when ctx is canceled.
	if cfg.GRPCListen != "" {
		engineImpl := engine.New(store, registry, cache, risk.NewDetector())
		stopGRPC, err := serveQueryGRPC(ctx, cfg, mesh.Source, engineImpl, logger)
		if err != nil {
			return err
		}
		defer stopGRPC()
	}

	return a.Run(ctx)
}

// startCalibration wires the WIRE-01c curve-calibration loop and launches it on
// background goroutines under ctx: a latest-quote cache fed by the market quote
// spine, a curve.Calibrator over a Snapshot-backed QuoteSource, and a
// schedule.Scheduler driving Refresh per currency on the intraday + nightly
// cadence. It returns an error only on a setup failure (bad reference spec /
// consumer) — the caller logs it and continues, since calibration is auxiliary
// to core risk ingestion.
func startCalibration(ctx context.Context, cfg config.Config, client *bus.NATSClient, busMetrics *bus.BusMetrics, logger *slog.Logger) error {
	instruments, err := livequote.ParseRateInstruments(cfg.CalibrationRates)
	if err != nil {
		return err
	}
	if len(instruments) == 0 {
		return errors.New("calibration enabled but RISK_ENGINE_CALIBRATION_RATES is empty")
	}

	// A dedicated consumer + broadcast group: the cache must SEE every market
	// event (it is a last-value cache, not a work queue), so it subscribes under
	// a per-source group distinct from any load-balanced market-data consumer.
	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))
	if err != nil {
		return err
	}
	cache := livequote.New()
	group := cfg.Source + "-calibration"
	for _, subject := range cfg.MarketSubjects {
		go func(subject string) {
			logger.Info("calibration subscribing market quotes", "subject", subject, "group", group)
			if err := consumer.Subscribe(ctx, subject, group, cache.Handler); err != nil && !errors.Is(err, context.Canceled) {
				// Degrade, don't crash: without fresh quotes the calibrator
				// deny-on-garbages and the prior curve keeps serving.
				logger.Error("calibration market subscription failed", "subject", subject, "err", err)
			}
		}(subject)
	}

	src := livequote.NewSnapshotRateSource(cache, instruments)
	cal := &curve.Calibrator{Source: src, Store: curve.NewStore(), Interp: curve.LinearZero}
	var jobs []schedule.Job
	for _, ccy := range src.Currencies() {
		ccy := ccy
		refresh := func(ctx context.Context, asOf time.Time) error {
			_, err := cal.Refresh(ctx, ccy, asOf)
			return err
		}
		jobs = append(jobs,
			schedule.Job{Name: "curve/" + ccy + "/intraday", Interval: cfg.CalibrationInterval, Refresh: refresh},
			schedule.Job{Name: "curve/" + ccy + "/nightly", Interval: cfg.CalibrationNightly, Refresh: refresh},
		)
	}
	sched := schedule.New(jobs, schedule.WithLogger(logger))
	logger.Info("calibration scheduler enabled",
		"jobs", sched.Jobs(), "currencies", src.Currencies(),
		"intraday", cfg.CalibrationInterval, "nightly", cfg.CalibrationNightly)
	go func() { _ = sched.Run(ctx) }()
	return nil
}

// serveQueryGRPC starts the risk query gRPC server on cfg.GRPCListen and
// returns a stop function that gracefully drains it. When src is non-nil the
// listener requires mTLS with an in-mesh peer SVID (SEC-01b); otherwise it
// serves plaintext (local/dev). Serve runs on its own goroutine; a bind failure
// is returned synchronously so startup fails loudly.
//
// src is INJECTED rather than built here (SEC-M3a): the caller owns the one
// workload identity this process has, because the bus needs the same one.
func serveQueryGRPC(ctx context.Context, cfg config.Config, src transport.Source, eng *engine.EngineImpl, logger *slog.Logger) (func(), error) {
	var opts []grpc.ServerOption
	if src != nil {
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
	// The engine is single-tenant per deployment (cfg.Tenant); grpcsrv stamps it
	// as the owning tenant of every served portfolio (WIRE-02a owner_tenant).
	grpcsrv.New(eng, cfg.Tenant).Register(grpcSrv)

	go func() {
		logger.Info("risk query gRPC listening", "addr", cfg.GRPCListen)
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Error("risk query gRPC server failed", "err", err)
		}
	}()
	return grpcSrv.GracefulStop, nil
}

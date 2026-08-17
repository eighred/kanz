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

	"google.golang.org/grpc"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/marketdata/returns"
	mdstore "github.com/eighred/kanz/internal/marketdata/store"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/marketdata/terms"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/prediction"
	risk "github.com/eighred/kanz/internal/risk"
	"github.com/eighred/kanz/internal/risk/bookuniverse"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/liquiditysource"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/pricing/livequote"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
	"github.com/eighred/kanz/internal/risk/termsource"
	"github.com/eighred/kanz/internal/schedule"
	"github.com/eighred/kanz/internal/validation"
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
	// The lifecycle lives in run() because os.Exit skips defers: every defer
	// run() registers fires before this line. The non-zero code is what makes a
	// fatal halt distinguishable from a graceful SIGTERM — both otherwise exit 0
	// with reason "Completed" in the pod's termination record (#266).
	// 2 = startup failure, 1 = run loop died after startup, 0 = clean shutdown.
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Records WHY we are stopping so the exit code can say so. Every site that
	// calls fatal.Raise below already brought the process down with stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

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
		return 2
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	readiness := &server.Readiness{}
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())), httpserver.Standard())
	go func() {
		logger.Info("risk-engine listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	var runErr error
	if cfg.NATSURL != "" {
		if err := runEngine(ctx, cfg, readiness, logger, obs); err != nil {
			logger.Error("engine stopped with error", "err", err)
			runErr = err
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

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	fatal.Raise(runErr)
	return fatal.Code()
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
	// THE CALIBRATED CURVE, OWNED HERE (#509 item 2).
	//
	// It used to be built inline inside startCalibration — `Store: curve.NewStore()`
	// as an anonymous argument — so the scheduler bootstrapped a curve every
	// interval into a store nothing else could reach. A DEAD WRITE that reported
	// healthy: kanz_risk_calibration_scheduled said 1 and the composition root
	// logged "calibration scheduler enabled", while the output went nowhere.
	//
	// Owning it here is what lets the FI measures read it. It is created
	// unconditionally and cheaply (an empty in-memory map); whether anything
	// FILLS it is the calibration gate below.
	curveStore := curve.NewStore()

	// FI IS REGISTERED ONLY WHEN A CURVE CAN EXIST, and that condition is the
	// interesting part of this change.
	//
	// Registering DV01/Duration/Convexity/SpreadDuration against an empty curve
	// store would serve a DV01 of zero for every portfolio — indistinguishable
	// from a book holding no bonds. Absent is the honest answer there, and
	// MeasurePosture already reports an absent family as dark with its name.
	calibrationEnabled := cfg.CalibrationInterval > 0 && cfg.CalibrationRates != ""

	// A BOND THAT FALLS OUT OF THE RATE RISK IS COUNTED. Both of these move the
	// measured risk DOWN — a skipped bond contributes 0 to DV01 and nothing to a
	// duration average — so an unexplained fall in DV01 has a number beside it
	// rather than looking like a book that de-risked.
	fiTermsMissing := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_risk_fi_terms_missing_total",
		Help: "Instruments whose bond terms could not be resolved when the FI measures asked. " +
			"Rising means contract terms were never loaded, or are present and unusable (an " +
			"unspecified day count, a maturity at or before issue) — either way those bonds are " +
			"absent from DV01 and every duration average (#509).",
	})
	fiSkipped := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_fi_position_skipped_total",
		Help: "Bond positions excluded from the FI measures, by reason. no_curve means the bond " +
			"is known and its currency has no calibrated discount curve — the calibration gap, " +
			"not a data gap. A skipped bond makes the book's measured rate risk SMALLER, and a " +
			"DV01 of zero is indistinguishable from holding no bonds (#509).",
	}, []string{"reason"})
	factorSkipped := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_factor_skipped_total",
		Help: "Factor-measure evaluations that reported zero because the model could not cover " +
			"what was asked. no_model = the whole book's factor risk was zero for that evaluation; " +
			"not_in_model = one holding sits outside the fitted universe and contributes to " +
			"neither the systematic nor the specific half. Both shrink measured risk, in the " +
			"direction that makes a limit pass (#509).",
	}, []string{"reason"})
	factorLookahead := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_risk_factor_universe_lookahead_total",
		Help: "Factor fits whose as-of predates the newest state in the book. The estimation " +
			"universe comes from the LIVE store, so a historical recompute is fitted over today's " +
			"holdings — a survivorship universe that excludes everything since sold. Non-zero " +
			"means a run was scored against a book it could not have known (#509).",
	})
	obs.Registry.MustRegister(fiTermsMissing, fiSkipped, factorSkipped, factorLookahead)
	for _, r := range []string{compute.SkipNoModel, compute.SkipNotInModel} {
		factorSkipped.WithLabelValues(r).Add(0)
	}
	// EVERY REASON GETS A SERIES AT ZERO. A counter that only appears on first
	// increment reads as no-data to an alert, so the alert cannot fire on the
	// transition from none to some — which is the transition that matters.
	for _, r := range []string{compute.SkipNoTerms, compute.SkipNoCurve} {
		fiSkipped.WithLabelValues(r).Add(0)
	}

	// THE BAR SERIES THE LIQUIDITY MEASURES READ, hoisted out of the branch below
	// so that "a venue was named and there is no store to read it at" is a case
	// registerLiquidityRisk can REFUSE. Left inside, that deployment would take
	// the no-market-data path and look identical to one that never asked for
	// liquidity at all. Nil interface, never a typed nil: only the concrete
	// price store is ever assigned to it.
	var barStore liquiditysource.BarStore

	if cfg.MarketDataURL != "" {
		// The SECOND pool this pod opens (the tenant-scoped state pool is below),
		// and the estate's connection budget counts it as such — see
		// test/arch/pool_budget_test.go, where risk-engine is charged twice at its
		// KEDA ceiling of 12. Global, not tenant-scoped: it reads market-data's
		// price_observations, which deliberately carries no RLS.
		pricePool, err := pg.NewGlobalPool(ctx, cfg.MarketDataURL,
			"market-data's price_observations is universal market data with no RLS — the same tick "+
				"prices every tenant's book, and this pool only ever reads it")
		if err != nil {
			return err
		}
		defer pricePool.Close()
		priceStore := mdstore.NewPostgres(pricePool)
		if err := priceStore.Ping(ctx); err != nil {
			return err
		}
		barStore = priceStore
		provider := returns.NewStoreReturnsProvider(priceStore, returns.ReturnsConfig{})
		varmodel.Register(context.Background(), registry, provider, varmodel.Config{})
		logger.Info("RISK-12/RISK-M1: historical-simulation VaR99 + ES99 + MaxDrawdown(+Amount) registered off market-data price store")

		// THE FACTOR MEASURES (#509). The last missing piece was the estimation
		// UNIVERSE — the returns provider above is the same one the factor model
		// reads, so no second history and no risk of the two disagreeing.
		universe, err := bookuniverse.FromStore(store,
			// A BACKTEST MUST NOT QUIETLY ACQUIRE A SURVIVORSHIP UNIVERSE. The
			// state store answers "what the book holds NOW" for any asOf, so a
			// recompute as of last year is fitted over today's holdings — which
			// excludes everything since sold. Counted rather than refused,
			// because the live path (asOf = now) is correct and is the common
			// one; a non-zero counter is what says a historical run happened.
			bookuniverse.WithLookaheadObserver(func(time.Time, time.Time) {
				factorLookahead.Inc()
			}))
		if err != nil {
			return err
		}
		modelProvider := compute.NewLiveModelProvider(
			// STATISTICAL, EXPLICITLY, and the explicitness is the point: the zero
			// value of factormodel.ModelType is FUNDAMENTAL, which needs a
			// CharacteristicProvider this deployment does not have — so an
			// omitted Type would select the one model that cannot fit, and every
			// factor measure would report zero for a reason nothing states.
			//
			// PCA over the return covariance needs only returns, which is why it
			// is the model that can be wired today. Fundamental and Blend become
			// reachable through compute.NewStoreCharacteristicProvider (it derives
			// momentum and volatility from this same returns provider); its
			// industry factor additionally needs a sector classifier, whose
			// production source is not built.
			factormodel.Config{Type: factormodel.Statistical},
			universe,
			factormodel.Providers{Returns: provider},
		)
		compute.RegisterFactorRisk(context.Background(), registry, compute.FactorProviders{
			Model: modelProvider,
			// COUNTED BY REASON, NOT BY INSTRUMENT — one series per instrument
			// ever held is unbounded cardinality. Note the two reasons answer
			// different questions: no_model means the whole book's factor risk is
			// zero for that evaluation, not_in_model means one holding is outside
			// the fitted universe. A single number would merge "we measured
			// nothing" with "we measured most of it".
			OnSkip: func(_, reason string) { factorSkipped.WithLabelValues(reason).Inc() },
		})
		logger.Info("FACTOR-01c: FactorVaR99 + SystematicRisk + SpecificRisk registered over a "+
			"statistical (PCA) model fitted on the live book",
			"min_universe", bookuniverse.DefaultMinInstruments)

		// THE FIXED-INCOME MEASURES (#509). Their two seams both exist now: bond
		// terms resolve through the contract-terms store this pool already
		// reaches, and the discount curve is the store owned above.
		if calibrationEnabled {
			bondTerms := termsource.NewProvider(terms.NewPostgres(pricePool),
				termsource.WithMissingTermsObserver(func(string) { fiTermsMissing.Inc() }))
			compute.RegisterFIRisk(context.Background(), registry, compute.FIProviders{
				Terms: bondTerms,
				Curve: curveStore,
				// COUNTED, NOT LABELLED BY INSTRUMENT. An instrument id label is
				// unbounded cardinality — one series per bond ever held — and the
				// question an operator has is "are bonds falling out of the rate
				// risk", which a count answers. Which ones is a log's job.
				OnSkip: func(_, reason string) { fiSkipped.WithLabelValues(reason).Inc() },
			})
			logger.Info("FI-01d: DV01 + Duration + Convexity + SpreadDuration registered off the "+
				"contract-terms store and the calibrated curve",
				"curve_currencies", cfg.CalibrationRates)
		} else {
			// NOT SILENT. Four implemented measures are absent, and the reason is
			// a config gate rather than a defect — an operator who wants DV01
			// needs to know that turning on rate calibration is what produces it.
			logger.Warn("FI-01d NOT registered: DV01, Duration, Convexity and SpreadDuration need "+
				"a discount curve, and rate calibration is off. Registering them against an empty "+
				"curve store would serve a DV01 of zero for every portfolio, which reads exactly "+
				"like a book holding no bonds",
				"fix", "set RISK_ENGINE_CALIBRATION_RATES and RISK_ENGINE_CALIBRATION_INTERVAL",
				"gauge", "kanz_risk_measure_live{family=\"fixed_income\"}=0")
		}
	} else {
		logger.Warn("no RISK_ENGINE_MARKETDATA_DATABASE_URL — VaR99 serves the RISK-07 1%×gross placeholder")
	}

	// THE LIQUIDITY MEASURES (#509) — the last dark compute seam. OUTSIDE the
	// market-data branch above so an unreadable configuration is refused rather
	// than silently taking the no-liquidity path, and NOT gated on
	// calibrationEnabled: that gate is about the discount curve, which liquidity
	// never touches. The wiring, the two observers and the argument for
	// registering only one of the two measures are in liquidity.go.
	if err := registerLiquidityRisk(context.Background(), cfg, registry, barStore, obs.Registry, logger); err != nil {
		return err
	}

	// WHICH MEASURES THIS ENGINE ACTUALLY SERVES (#509).
	//
	// REPORTED HERE, the instant the registry is final, rather than beside the
	// other postures 180 lines down — for the reason CalibrationPosture states
	// about itself: the case that matters most is the one where startup does not
	// get that far. A pod that dies wiring the bus still has a complete answer to
	// "what would this build have served", and that answer is a static fact about
	// the binary rather than about the run.
	//
	// Passed the SAME registry the query EngineImpl uses, deliberately — a posture
	// computed from a fresh registry would describe an engine nobody talks to.
	//
	// THE COUNT IS NOT A FIXED PROPERTY OF THE BINARY — it is what the config
	// above happens to unlock, which is exactly why the gauge exists rather than a
	// number in a comment. It used to say "EIGHT of twenty-six" and was stale
	// within the day: FACTOR-01c, FI-01d and LIQ-01d each added to it. A fully
	// configured pod today serves sixteen of the twenty-six catalogued measures;
	// one with no market-data DSN serves five. The query path DROPS the rest —
	// engine.filterMeasures discards unknown names — so they are absent from a 200
	// rather than refused, which is indistinguishable from a portfolio that holds
	// none of that instrument.
	app.MeasurePosture(obs.Registry, logger, registry)
	// THE AI LAYER GETS ITS INPUT (AI-M1).
	//
	// internal/prediction shipped a feature publisher, a resilient inference client and a
	// model registry — and had ZERO IMPORTERS outside its own tests. Nothing computed a
	// feature, nothing published one, nothing consumed a prediction. The platform's whole AI
	// capability was a well-specified contract with no traffic on it, and the brief's next
	// fifteen layers were all to be built on top of it.
	//
	// THE HYBRID SPLIT: features are computed in GO — the engine holds the
	// source state — and scored in PYTHON, where the model serving stack lives. The
	// document that first stated it was deleted on 2026-07-29; the split is observable now
	// in the two halves themselves: internal/prediction publishes, kanz-py/kanz_inference
	// scores. This is the
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
	sharded := assign.Sharded() && cfg.ShardSelf != ""
	if sharded {
		applier = app.NewShardFilter(applier, assign)
		group = app.DefaultConsumerGroup + "-" + cfg.ShardSelf
	}
	// WHETHER THIS FLEET PINS A PORTFOLIO TO A REPLICA (#110). The log line that
	// used to sit inside the branch above announced the good case and said
	// nothing about the other one — so the state the estate is actually in was
	// the silent one.
	app.ShardPosture(obs.Registry, logger, sharded, cfg.ShardMembers, cfg.ShardSelf)
	ingest, err := app.NewIngest(consumer, applier, group, logger)
	if err != nil {
		return err
	}
	a.Ingest = ingest

	// A BREACH NOW REACHES THE UNWIND DECIDER (#74). internal/risk/unwind could
	// size what a breached portfolio would have to shed since M4, and nothing ever
	// called it — the arithmetic was built, tested and unreachable, which is
	// indistinguishable from broken to anyone reading the issue.
	//
	// IT IS WIRED HERE RATHER THAN IN COMPLIANCE, where the breach is detected and
	// the wire would have been shorter, because test/arch/risk_boundary_test.go
	// admits only this service to the risk module's impl packages. That is the
	// RISK-02 boundary doing its job; the alternative was a hole in it.
	//
	// AUXILIARY, LIKE CALIBRATION BELOW: a failed subscription costs the unwind
	// PROPOSAL, which nothing executes anyway. It must not take down the engine
	// that is computing the measures the breach came from.
	unwindWatch := app.NewUnwindWatch(obs.Registry, cfg.Tenant, logger)
	go func() {
		unwindGroup := app.DefaultConsumerGroup + "-unwind"
		logger.Info("risk-engine subscribing compliance breaches for unwind proposals",
			"subject", compliance.SubjectBreach, "group", unwindGroup, "executes", false)
		if err := consumer.Subscribe(ctx, compliance.SubjectBreach, unwindGroup, unwindWatch.Handle); err != nil &&
			!errors.Is(err, context.Canceled) {
			logger.Error("risk-engine unwind subscription failed — breaches will be detected but "+
				"nothing will size what would clear them", "err", err)
		}
	}()

	// Calibration scheduler (WIRE-01c): the risk module's composition root owns
	// the live curve calibration loop. Subscribe the market quote spine into a
	// latest-quote cache and drive curve.Calibrator.Refresh on a nightly +
	// intraday cadence; a failed calibration deny-on-garbages (prior curve keeps
	// serving). Auxiliary to core risk ingest — a market-subscription failure
	// degrades calibration (no fresh curve), it does not bring the engine down.
	// WHICH CALIBRATIONS ARE ACTUALLY RUNNING (#113).
	//
	// This service implements three — curve, volsurface and credit — and
	// schedules at most ONE. "calibration scheduler enabled" reads as though
	// calibration is on; it says nothing about the two that are built, tested and
	// idle, and a surface nobody refreshes prices a position exactly like a fresh
	// one.
	//
	// STATED HERE RATHER THAN INSIDE startCalibration, because the case that
	// matters most is the one where startCalibration is never called at all: with
	// calibration disabled the old code said nothing whatsoever, so "no curve
	// either" was indistinguishable from a healthy service.
	scheduledCalibrations := map[string]string{}
	if cfg.CalibrationInterval > 0 && cfg.CalibrationRates != "" {
		if err := startCalibration(ctx, cfg, client, busMetrics, logger, curveStore); err != nil {
			logger.Error("calibration scheduler disabled", "err", err)
		} else {
			scheduledCalibrations["curve"] = "rates"
		}
	}
	app.CalibrationPosture(obs.Registry, logger, scheduledCalibrations)

	// MODEL VALIDATION (#471). The gate is built here and the benchmark evidence
	// this build carries is recorded into it, so kanz_risk_analytics_validated
	// answers "how much of this book is priced by something nobody checked".
	//
	// A failure here is NOT a failed benchmark — those are recorded and reported
	// as such. It means the benchmark case sets themselves are malformed, so this
	// build cannot state its own validation posture, and a risk engine that
	// cannot say what it has validated must not start as though it had.
	validationGate := validation.NewGate(nil)
	if err := app.LoadValidations(validationGate, func() time.Time { return time.Now().UTC() }); err != nil {
		return err
	}
	app.AnalyticsPosture(obs.Registry, logger, validationGate)

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
func startCalibration(ctx context.Context, cfg config.Config, client *bus.NATSClient, busMetrics *bus.BusMetrics, logger *slog.Logger, store *curve.Store) error {
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
	cal := &curve.Calibrator{Source: src, Store: store, Interp: curve.LinearZero}
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

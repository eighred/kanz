// regulatory filing service entrypoint (WIRE-01e). It serves the delivered
// PARITY-06 filings — FRTB capital, Form PF, AIFMD leverage, TCFD/SFDR climate
// disclosures — assembled from request-supplied inputs, signed, and served
// point-in-time + completeness-gated. It owns no analytics; each endpoint drives
// a delivered File* assembler through the injected Signer.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/httpserver"
	regulatorypublish "github.com/eighred/kanz/internal/regulatory/publish"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/regulatory/internal/config"
	"github.com/eighred/kanz/services/regulatory/internal/recording"
	"github.com/eighred/kanz/services/regulatory/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
)

// chainDurable reports durable context-chain storage in both signature modes.
// The original hash signature still makes no original-chain claim. A missing
// database refuses startup rather than serving an ephemeral recording path.
var chainDurable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_regulatory_chain_durable",
	Help: "1 when mandatory filing/context-chain recording is backed by Postgres; missing storage refuses startup.",
})

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

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "regulatory",
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

	// Register before opening recording dependencies. HTTP serving starts only
	// after the durable recorder is ready.
	obs.Registry.MustRegister(chainDurable)

	pool, err := openRecordingPool(ctx, cfg)
	if err != nil {
		logger.Error("filing recording database unavailable")
		return 2
	}
	defer pool.Close()
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("filing workload identity unavailable")
		return 2
	}
	defer func() { _ = mesh.Close() }()
	busMetrics := bus.NewBusMetrics(obs.Registry)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: "regulatory", TLSConfig: mesh.Client, Metrics: busMetrics})
	if err != nil {
		logger.Error("filing broker unavailable")
		return 2
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "regulatory", ProducerVersion: version.String(), Tenant: cfg.Tenant, Metrics: busMetrics})
	if err != nil {
		logger.Error("filing producer configuration refused")
		return 2
	}
	publisher, err := regulatorypublish.New(producer, cfg.Tenant)
	if err != nil {
		logger.Error("filing publisher unavailable")
		return 2
	}
	recorder, err := recording.New(pool, cfg.Tenant, cfg.Signer, publisher, logger)
	if err != nil {
		logger.Error("filing recorder configuration refused")
		return 2
	}
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() {
		err := recorder.Run(relayCtx)
		if err != nil && !errors.Is(err, context.Canceled) {
			fatal.Raise(err)
		}
		relayDone <- err
	}()
	defer func() {
		stopRelay()
		if err := <-relayDone; err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("filing relay stopped with failure")
		}
	}()

	// The instrument classifier the ESG screen resolves SECTOR and ISSUER through
	// (#751). Absent, those dimensions are UNRESOLVABLE and the shared COMP-01
	// engine REFUSES rather than passing (#640) — so an exclusion policy naming a
	// sector comes back "cannot be verified" with the cause named, never as a
	// clean pass over a book nobody could classify. LogPosture states which of
	// the two postures this pod is in, at WARN when it is the unarmed one.
	refCache, rerr := cfg.RefData.NewCache(cfg.Tenant, "svc:regulatory")
	if rerr != nil {
		logger.Error("regulatory: the instrument classifier refused its configuration", "err", rerr)
		return 2
	}
	cfg.RefData.LogPosture(logger, "regulatory")

	readiness := &server.Readiness{}
	opts := []server.Option{server.WithMetrics(obs.MetricsHandler()), server.WithRecorder(cfg.Tenant, version.String(), recorder)}
	if refCache != nil {
		opts = append(opts, server.WithClassifier(refCache.Compliance()))
	}
	api := server.New(readiness, logger, opts...)

	// TWO LISTENERS, AND THE SEPARATION IS THE SECURITY BOUNDARY (#765).
	//
	// allow-observability-scrape selects every pod in kanz-services and admits
	// :8083 across all of them. Until this split that port carried the filing
	// routes, and a filing is not a read — it is signed and appends a link to the
	// AUDIT-01 hash chain — so any pod in the kanz-observability namespace could
	// write to the compliance record, against a service that reads no principal
	// header and therefore has no authentication step to fail.
	//
	// /metrics stays on :8083 because audit already publishes metrics there and
	// the rule admits it anyway; the API moved instead. That is the same trade
	// accounting made in #447: admitting a NEW port for metrics would widen the
	// rule for no gain, so it is the API that leaves.
	metricsMux := http.NewServeMux()
	if h := api.MetricsHandler(); h != nil {
		metricsMux.Handle("GET /metrics", h)
	}
	metricsSrv := httpserver.New(cfg.MetricsListen, metricsMux, httpserver.Standard())
	go func() {
		logger.Info("regulatory metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	httpSrv := httpserver.New(cfg.Listen, api, httpserver.Standard())
	go func() {
		logger.Info("regulatory listening", "addr", cfg.Listen, "signer", cfg.Signer)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()
	readiness.Set(true)

	<-ctx.Done()
	readiness.Set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics shutdown error", "err", err)
	}
	return fatal.Code()
}

// openRecordingPool requires durable artifact/outbox storage in BOTH signature
// modes. HashSigner still makes a bare-content-hash claim; durable recording is
// an independent requirement. The global audit table has no RLS, so the scoped
// pool still sees the one estate chain while artifact/outbox tables are isolated.
func openRecordingPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		return nil, errors.New("REGULATORY_DATABASE_URL is required for atomic filing artifacts and FACT recovery in both signature modes; REGULATORY_ALLOW_EPHEMERAL_CHAIN cannot disable recording")
	}
	if !filing.BoundedIdentifier(cfg.Tenant) || cfg.NATSURL == "" || (cfg.Signer != "chain" && cfg.Signer != "hash") {
		return nil, errors.New("filing recording needs an explicit tenant, broker and supported signature mode")
	}
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	chainDurable.Set(1)
	return pool, nil
}

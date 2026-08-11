// optimization binary entrypoint (OPT-01e). Optimizes target weights and builds
// rebalance proposals (mean-variance / risk-parity under COMP-01 mandate
// constraints), and materializes an approved proposal into OMS-01 order commands
// — the portfolio-manager's forward workflow (ROI #29). A stateless construction
// service: market inputs arrive in the request, so it boots ready; the bus
// publisher + pre-trade gate are wired behind the bridge seams at the
// composition root.
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

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/optimization/internal/config"
	"github.com/eighred/kanz/services/optimization/internal/publish"
	"github.com/eighred/kanz/services/optimization/internal/server"
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

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "optimization",
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

	// THE AUTO-PUBLISH SWITCH (#409). Off unless asked for by name.
	//
	// With it off this service builds order commands and hands them back: the
	// optimizer proposes, a human approves, and nothing reaches the bus. With it
	// on the platform trades on its own recommendation, and that is a different
	// product — so it is stated here in as many words, at every boot, and counted.
	// A deployment must never acquire this behaviour by omission.
	autoPublished := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_optimization_auto_published_orders_total",
		Help: "Order commands published from a materialized rebalance proposal without a human " +
			"approval step. Non-zero means this deployment trades on its own recommendation.",
	})
	obs.Registry.MustRegister(autoPublished)
	factsLost := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_optimization_materialization_facts_lost_total",
		Help: "ProposalMaterialized FACTs that never reached the bus. Each one is a capital action " +
			"whose record of who authorized it was lost; the orders themselves went out regardless.",
	})
	obs.Registry.MustRegister(factsLost)

	var serverOpts []server.Option
	producer, closeBus, err := buildBus(ctx, cfg, logger)
	if err != nil {
		logger.Error("bus init failed", "err", err)
		return 2
	}
	defer closeBus()
	switch {
	case cfg.AutoPublish:
		logger.Warn("AUTO-PUBLISH IS ARMED — a materialized rebalance proposal's orders go straight to "+
			"the bus with no human approval step. Every order is still attributed to the "+
			"gateway-authenticated caller and re-checked by the OMS's pre-trade gate on admission",
			"broker", cfg.NATSURL)
		armed := publish.NewMaterializer(producer, autoPublished, factsLost)
		serverOpts = append(serverOpts, server.WithAutoPublish(
			func(tenant string) server.Materializer { return armed.ForTenant(tenant) }))
	case producer != nil:
		// A broker with the switch off is a legitimate, and the DEFAULT, posture:
		// the FACT still gets recorded, so a dry run is visible rather than
		// invisible. Say which mode this is so the two are never guessed at.
		logger.Info("auto-publish is OFF — materialized proposals are returned to the caller and " +
			"nothing is sent to the bus (set OPTIMIZATION_AUTO_PUBLISH=true to change that)")
		recorder := publish.NewRecorder(producer, factsLost)
		serverOpts = append(serverOpts, server.WithAutoPublish(
			func(tenant string) server.Materializer { return recorder.ForTenant(tenant) }))
	default:
		logger.Info("no broker configured — materialized proposals are returned to the caller and " +
			"nothing is recorded on the bus")
	}

	readiness := &server.Readiness{}

	// TWO LISTENERS, AND THE SPLIT IS THE POINT (#409).
	//
	// The API port carries the routes that materialize a rebalance proposal into
	// order commands, and it decides whose name goes on them from the principal
	// header the api-gateway injects. A NetworkPolicy admits only the gateway to
	// it. That policy is only worth anything while nothing ELSE forces the port
	// open — and /metrics on the same mux does exactly that, because
	// allow-observability-scrape must admit whatever port serves it.
	//
	// Every other header-trusting service on this platform shares one port and is
	// therefore reachable from kanz-observability with a self-chosen principal
	// (#232, which named the fix as a code change in each service). This is the
	// first service to make it, because a trading surface is the one place the
	// platform cannot afford to inherit that gap.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", obs.MetricsHandler())
	metricsSrv := httpserver.New(cfg.MetricsListen, metricsMux, httpserver.Standard())
	go func() {
		logger.Info("optimization metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Telemetry is not the trading path: a dead metrics listener must not
			// take the service down, but it must not be silent either — a scrape
			// target that vanishes reads as a healthy service nobody is watching.
			logger.Error("metrics server failed — this pod is now unmonitored", "err", err)
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutCtx)
	}()

	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, serverOpts...), httpserver.Standard())
	go func() {
		logger.Info("optimization listening", "addr", cfg.Listen)
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

	return fatal.Code()
}

// buildBus dials the spine and returns a command producer, or nil when no broker
// is configured.
//
// NO ProducerConfig.Tenant, DELIBERATELY, and for the reason the api-gateway's
// own bus states: this producer carries order COMMANDs, and a fallback tenant
// would stamp a materialized rebalance with the SERVICE's tenant when the
// caller's principal carried none — attributing a customer's capital command to
// the platform, under a value that is valid and not theirs. The tenant comes
// from the authenticated caller or the publish is refused.
//
// VerifyCommandIssuer is injected so the bus re-checks the issuer against the
// principal on ctx (AUTH-01c). This service already refuses a body-supplied
// issuer at the handler; this is the second, independent check, and it is what
// makes the refusal structural rather than a handler's good manners.
func buildBus(ctx context.Context, cfg config.Config, logger *slog.Logger) (*bus.Producer, func(), error) {
	if cfg.NATSURL == "" {
		return nil, func() {}, nil
	}
	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return nil, func() {}, err
	}
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		_ = mesh.Close()
		return nil, func() {}, err
	}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:              cfg.Source,
		ProducerVersion:     version.String(),
		VerifyCommandIssuer: auth.VerifyCommandIssuer,
	})
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, func() {}, err
	}
	return producer, func() { _ = client.Close(); _ = mesh.Close() }, nil
}

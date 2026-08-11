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

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/optimization/internal/config"
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

	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger), httpserver.Standard())
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

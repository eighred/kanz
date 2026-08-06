// Schema-registry binary entrypoint. Skeleton only — schema HTTP endpoints
// arrive in EVT-16b (ingest) and EVT-16c (resolve).
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
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/schema-registry/internal/config"
	"github.com/eighred/kanz/services/schema-registry/internal/server"
	"github.com/eighred/kanz/services/schema-registry/internal/storage"
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

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Records WHY we are stopping so the exit code can say so. Every site that
	// calls fatal.Raise below already brought the process down with stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

	pool, err := pg.NewGlobalPool(ctx, cfg.DatabaseURL,
		"a payload schema is PLATFORM metadata, not a tenant's: 0001_init.sql declares no RLS, and every "+
			"tenant's envelope is validated against the same registered version. Scoping it per tenant "+
			"would let two tenants register incompatible version 1s of one subject")
	if err != nil {
		logger.Error("postgres connect failed", "err", err)
		return 2
	}
	defer pool.Close()

	// Telemetry (#61). The registry is on the ingest path for every payload schema
	// the bus validates against, and it ran with no metrics surface at all.
	// OTLPEndpoint is deliberately empty: spans are still created and trace
	// context still propagates, and startup never blocks on a collector.
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "schema-registry",
		ServiceVersion: version.String(),
		SampleRatio:    1,
	}, slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err != nil {
		logger.Error("telemetry init failed", "err", err)
		return 2
	}
	defer func() {
		if err := obs.Shutdown(context.Background()); err != nil {
			logger.Error("telemetry shutdown failed", "err", err)
		}
	}()

	srv := server.New(storage.NewPostgres(pool), logger)

	// /metrics is mounted OUTSIDE the registry handler rather than inside the
	// server package: the registry's routes are the EVT-16 API surface, and a
	// telemetry endpoint is not part of that contract. Wrapping keeps the two
	// separable — server.New stays testable without a Prometheus registry, and
	// the scrape surface cannot accidentally shadow an API route.
	root := http.NewServeMux()
	root.Handle("GET /metrics", obs.MetricsHandler())
	root.Handle("/", srv)

	httpSrv := httpserver.New(cfg.Listen, root, httpserver.Standard())

	go func() {
		logger.Info("schema-registry listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown requested")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	return fatal.Code()
}

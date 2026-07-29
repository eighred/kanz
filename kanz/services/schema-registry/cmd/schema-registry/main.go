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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/schema-registry/internal/config"
	"github.com/eighred/kanz/services/schema-registry/internal/server"
	"github.com/eighred/kanz/services/schema-registry/internal/storage"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		os.Exit(2)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("postgres connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Telemetry (#61). The registry is on the ingest path for every payload schema
	// the bus validates against, and it ran with no metrics surface at all.
	// OTLPEndpoint is deliberately empty: spans are still created and trace
	// context still propagates, and startup never blocks on a collector.
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "schema-registry",
		ServiceVersion: version(),
		SampleRatio:    1,
	}, slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err != nil {
		logger.Error("telemetry init failed", "err", err)
		os.Exit(2)
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

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("schema-registry listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown requested")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// version is the ServiceVersion stamped on this process's OTel resource. See the
// note on the identical function in services/operator: these copies are numerous
// and do not agree, and unifying them is its own change.
func version() string { return "dev" }

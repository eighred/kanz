// audit binary entrypoint (AUDIT-01). Consumes the event backbone and
// materializes every decision, command, outcome, data-quality event, and
// AUTH-01d authz decision into an append-only, hash-chained, queryable audit log
// (AUDIT-01a/b), and serves the lineage (AUDIT-01c) + reporting (AUDIT-01d) API.
// With AUDIT_DATABASE_URL set the store is the durable WORM Postgres log;
// without it the store is in-memory (local/dev only — an audit log that doesn't
// survive a restart is not an audit log).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/pkg/transport"
	"github.com/kanz-eng/kanz/services/audit/internal/audit"
	"github.com/kanz-eng/kanz/services/audit/internal/config"
	"github.com/kanz-eng/kanz/services/audit/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	store, closeStore, err := openStore(ctx, cfg)
	if err != nil {
		logger.Error("store open failed", "err", err)
		os.Exit(2)
	}
	defer closeStore()
	if err := store.Ping(ctx); err != nil {
		logger.Error("store ping failed", "err", err)
		os.Exit(2)
	}

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler()), server.WithStore(store)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("audit listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	if cfg.NATSURL != "" {
		if err := runProjection(ctx, cfg, store, readiness, logger, obs); err != nil {
			logger.Error("projection stopped with error", "err", err)
		}
	} else {
		readiness.Set(true)
		logger.Warn("no AUDIT_NATS_URL set — serving the read API only (no projection)")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// runProjection subscribes the configured subjects and folds each event into the
// audit store. Each subject runs on its own goroutine (Subscribe blocks); the
// first non-cancellation error cancels the siblings and is returned — fail-fast,
// so a broken subscription brings the projection down rather than silently
// leaving an audit gap.
func runProjection(ctx context.Context, cfg config.Config, store audit.Store, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)
	projector := audit.NewProjector(store, time.Now)

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return err
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for _, subject := range cfg.Subjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			logger.Info("audit subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, projector.Handle)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(subject)
	}
	readiness.Set(true)
	wg.Wait()
	readiness.Set(false)
	return firstErr
}

// openStore selects the durable Postgres WORM store when a DSN is set, otherwise
// the in-memory store. Returns a close func (a no-op for in-memory).
func openStore(ctx context.Context, cfg config.Config) (audit.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		return audit.NewMemory(), func() {}, nil
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	return audit.NewPostgres(pool), pool.Close, nil
}

// version is the service version stamped on telemetry. Hardcoded until the build
// injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }

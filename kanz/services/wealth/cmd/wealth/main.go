// wealth (advisory) binary entrypoint (WEALTH-01b). It aggregates a household's
// accounts into a virtual portfolio and serves the household-level exposure view
// — Aladdin Wealth atop the institutional engine (ROI #40). The household store
// is in-memory by default; a durable backend and the bus consumer that feeds
// account/holding state are wired behind the book.Store seam at the composition
// root (PERS-01/DEBT-02), where the risk-engine recompute over the virtual
// portfolio is driven through the risk api/v* surface.
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

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/wealth/internal/book"
	"github.com/eighred/kanz/services/wealth/internal/config"
	"github.com/eighred/kanz/services/wealth/internal/consume"
	"github.com/eighred/kanz/services/wealth/internal/server"
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
		ServiceName:    "wealth",
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

	// SEC-M3: one workload identity for the process, shared by the consumer's
	// bus dial. The production broker requires a client SVID; a nil TLSConfig is
	// a plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("spiffe source init failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())

	store, closeStore, err := openStore(ctx, cfg)
	if err != nil {
		logger.Error("store init failed", "err", err)
		os.Exit(2)
	}
	defer closeStore()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, store, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("wealth listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	// Household-valuation consumer (WEALTH-01b): fold live HouseholdValued FACTs
	// into the same book.Store the server reads, so the household exposure view
	// reflects live valuations. Runs concurrently with the server; both share
	// ctx so SIGTERM stops them together. Empty WEALTH_NATS_URL ⇒ read-only
	// (default), the same stance accounting/alternatives take for their
	// consumers — this is the Folder.Handle seam that had been bus-handler-
	// shaped and unreachable since it was written, because nothing ever called
	// bus.NewConsumer in this service.
	if cfg.NATSURL != "" {
		go func() {
			if err := runConsumer(ctx, cfg, store, mesh, logger, obs); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("household consumer stopped with error", "err", err)
				stop()
			}
		}()
	} else {
		logger.Info("no WEALTH_NATS_URL set — serving read endpoints only (no valuation folding)")
	}
	readiness.Set(true)

	<-ctx.Done()
	readiness.Set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// openStore selects the durable Postgres household book when a DSN is set,
// otherwise the in-memory store. Returns a close func that tears down the pool
// (a no-op for the in-memory store). Both satisfy book.Store, so the server is
// identical either way.
//
// book.Postgres has existed since PARITY-02b, tested and unused: nothing ever
// constructed it, so every household this service served lived in a map and
// vanished on restart.
func openStore(ctx context.Context, cfg config.Config) (book.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		return book.NewMemoryStore(), func() {}, nil
	}
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC, so Postgres RLS scopes all reads/writes to it. A
	// non-superuser DB role is required for FORCE RLS to apply.
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, err
	}
	return book.NewPostgres(pool), pool.Close, nil
}

// runConsumer folds live wealth.v1.HouseholdValued FACTs into store until ctx
// is canceled. It mirrors accounting's fill-folding consumer: one bus.Consumer,
// each subject subscribed on its own goroutine (Subscribe blocks), fail-fast —
// the first non-cancellation error cancels the siblings and is returned, so a
// broken subscription brings folding down rather than running silently
// degraded (a stale household valuation must be loud, not a quietly wrong
// exposure view). Unlike alternatives, wealth carries one message type on one
// subject, so a single consume.NewFolder(cfg.Tenant, store, consume.DecodeProto) suffices
// — no per-subject factory is needed.
func runConsumer(ctx context.Context, cfg config.Config, store book.Store, mesh *transport.Mesh, logger *slog.Logger, obs *observability.Provider) error {
	folder, err := consume.NewFolder(cfg.Tenant, store, consume.DecodeProto)
	if err != nil {
		return err
	}

	busMetrics := bus.NewBusMetrics(obs.Registry)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// WithDLQ is not optional: test/arch/bus_dlq_test.go fails the build without
	// it, and for good reason — a malformed valuation FACT must land somewhere
	// inspectable rather than vanish on nack.
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
			logger.Info("wealth subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, folder.Handle)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(subject)
	}
	wg.Wait()
	return firstErr
}

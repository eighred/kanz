// accounting (IBOR) binary entrypoint (IBOR-01). It folds OMS-01 fills and
// cash/corporate-action events into the investment book-of-record and serves
// point-in-time NAV and custodian reconciliation — the system-of-record beneath
// every number (ROI #36). The journal store is in-memory by default; a durable,
// replayable backend and the bus consumer that feeds the journal are wired behind
// the ledger.Store seam at the composition root (PERS-01/DEBT-02), so the default
// boot serves the read/reconcile endpoints without a broker.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/accounting/internal/config"
	"github.com/kanz-eng/kanz/services/accounting/internal/consume"
	"github.com/kanz-eng/kanz/services/accounting/internal/ledger"
	"github.com/kanz-eng/kanz/services/accounting/internal/server"
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
		ServiceName:    "accounting",
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
		logger.Error("store init failed", "err", err)
		os.Exit(2)
	}
	defer closeStore()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, store, cfg.BaseCurrency, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("accounting listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	// Fill-folding consumer (WIRE-01b): fold live order.v1 fill FACTs into the
	// same journal the server reads, so NAV/positions reflect live execution.
	// Runs concurrently with the server; both share ctx so SIGTERM stops them
	// together. Empty ACCOUNTING_NATS_URL ⇒ read/reconcile only (default).
	if cfg.NATSURL != "" {
		go func() {
			if err := runConsumer(ctx, cfg, store, logger, obs); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("fill consumer stopped with error", "err", err)
				stop()
			}
		}()
	} else {
		logger.Info("no ACCOUNTING_NATS_URL set — serving read/reconcile only (no fill folding)")
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

// openStore selects the durable Postgres journal when a DSN is set (PARITY-02a),
// otherwise the in-memory store. Returns a close func that tears down the pool
// (a no-op for the in-memory store). Both satisfy ledger.Store, so the server
// and the fill consumer share one journal regardless of backend.
func openStore(ctx context.Context, cfg config.Config) (ledger.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		return ledger.NewMemoryStore(), func() {}, nil
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	return ledger.NewPostgres(pool), pool.Close, nil
}

// runConsumer folds live order.v1 fill FACTs into store until ctx is canceled.
// It mirrors the market-data / risk-engine ingest pattern: one bus.Consumer,
// each fill subject subscribed on its own goroutine (Subscribe blocks), fail-
// fast — the first non-cancellation error cancels the siblings and is returned,
// so a broken subscription brings folding down rather than running silently
// degraded (book-of-record data loss must be loud). Idempotency is handled below
// this layer (consumer dedup + ledger.Store.Append on the entry id).
func runConsumer(ctx context.Context, cfg config.Config, store ledger.Store, logger *slog.Logger, obs *observability.Provider) error {
	folder, err := consume.NewFolder(store, cfg.BaseCurrency)
	if err != nil {
		return err
	}

	busMetrics := bus.NewBusMetrics(obs.Registry)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics))
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
	for _, subject := range cfg.FillSubjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			logger.Info("accounting subscribing", "subject", subject, "group", cfg.ConsumerGroup)
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

// version reads the build's VCS revision for the service-version label.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "dev"
}

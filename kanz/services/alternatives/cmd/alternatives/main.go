// alternatives (private-markets) binary entrypoint (ALT-01b). It folds commitment
// lifecycle events — capital calls, distributions, NAV marks — into an
// event-sourced fund position and serves position summaries + private-asset
// metrics (IRR/TVPI/DPI/RVPI), the alternative-assets sleeve a whole-portfolio
// view requires (ROI #38). The journal store is in-memory by default; a durable,
// replayable backend and the bus consumer that feeds the journal are wired behind
// the fund.Store seam at the composition root (PERS-01/DEBT-02).
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
	"github.com/eighred/kanz/services/alternatives/internal/config"
	"github.com/eighred/kanz/services/alternatives/internal/consume"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
	"github.com/eighred/kanz/services/alternatives/internal/server"
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
		ServiceName:    "alternatives",
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
		logger.Info("alternatives listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	// Commitment-lifecycle consumer (ALT-01b): fold live capital-call,
	// distribution and NAV-mark FACTs into the same fund.Store the server reads,
	// so position summaries and IRR/TVPI/DPI/RVPI reflect live activity. Runs
	// concurrently with the server; both share ctx so SIGTERM stops them
	// together. Empty ALTERNATIVES_NATS_URL ⇒ read-only (default), the same
	// stance accounting takes for its fill consumer.
	if cfg.NATSURL != "" {
		go func() {
			if err := runConsumer(ctx, cfg, store, mesh, logger, obs); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("commitment consumer stopped with error", "err", err)
				stop()
			}
		}()
	} else {
		logger.Info("no ALTERNATIVES_NATS_URL set — serving read endpoints only (no lifecycle folding)")
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

// openStore selects the durable Postgres commitment journal when a DSN is set,
// otherwise the in-memory store. Returns a close func that tears down the pool
// (a no-op for the in-memory store). Both satisfy fund.Store, so the server and
// the fold are identical either way.
//
// fund.Postgres has existed since PARITY-02b, tested and unused: nothing ever
// constructed it, so the append-only journal this service is built around lived
// in a map and vanished on restart.
func openStore(ctx context.Context, cfg config.Config) (fund.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		return fund.NewMemoryStore(), func() {}, nil
	}
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC, so Postgres RLS scopes all reads/writes to it. A
	// non-superuser DB role is required for FORCE RLS to apply.
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, err
	}
	return fund.NewPostgres(pool), pool.Close, nil
}

// runConsumer folds the live commitment-lifecycle FACTs into store until ctx is
// canceled. It mirrors accounting's fill-folding consumer: one bus.Consumer,
// each subject subscribed on its own goroutine (Subscribe blocks), fail-fast —
// the first non-cancellation error cancels the siblings and is returned, so a
// broken subscription brings folding down rather than running silently
// degraded (a lost capital call or NAV mark must be loud, not a quietly wrong
// IRR/TVPI). Idempotency is handled below this layer (Folder.Handle dedups on
// the event id via fund.Store.Append).
func runConsumer(ctx context.Context, cfg config.Config, store fund.Store, mesh *transport.Mesh, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// WithDLQ is not optional: test/arch/bus_dlq_test.go fails the build without
	// it, and for good reason — a malformed lifecycle FACT must land somewhere
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
	subscribe := func(subject string, handler bus.EventHandler) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("alternatives subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, handler)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}()
	}
	// The journal folder, one per subject: the decoder is bound to the payload
	// type its subject carries, because nothing in the bytes says which message
	// they are. A single Folder shared across all four subjects would decode
	// every message as one type — e.g. every distribution as a capital call —
	// and silently corrupt the fund position rather than error.
	for _, subject := range cfg.Subjects {
		folder, err := consume.NewFolder(store, consume.DecodeProto(subject))
		if err != nil {
			return err
		}
		subscribe(subject, folder.Handle)
	}
	wg.Wait()
	return firstErr
}

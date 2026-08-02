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
	"sync"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	accounting "github.com/eighred/kanz/services/accounting/internal"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/fxfeed"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/server"
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

	// SEC-M3: ONE workload identity for the process, shared by all three bus
	// dials below (consumer, cash publisher, FX feed). The production broker
	// requires a client SVID; a nil TLSConfig is a plaintext client it refuses at
	// the handshake. Built here rather than per-dial because an X509Source is a
	// live rotation watcher — three of them would be three watchers for one
	// identity.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("spiffe source init failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())

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

	// Live FX for multi-currency NAV (WIRE-01d): build the latest-rate cache and
	// the default instrument→currency join from config, and hand the server a
	// live FX provider so NAV values a multi-currency book with no `fx` in the
	// request. A bad reference spec fails loud at startup. Off when unconfigured.
	opts := []server.Option{server.WithMetrics(obs.MetricsHandler())}
	liveFX, err := buildLiveFX(cfg, &opts)
	if err != nil {
		logger.Error("FX config invalid", "err", err)
		os.Exit(2)
	}

	// Cash-movement producer (WIRE-01f): with a broker configured, mount the
	// cash-movement endpoint whose posts are EMITTED as accounting.v1 FACTs and
	// folded back by the consumer below — the event-sourced path, so booking a
	// subscription/redemption/fee is replayable. A dial failure is fatal (a
	// configured broker that won't connect is a misconfiguration).
	if cfg.NATSURL != "" {
		pub, closePub, err := buildCashPublisher(ctx, cfg, mesh)
		if err != nil {
			logger.Error("cash publisher init failed", "err", err)
			os.Exit(2)
		}
		defer closePub()
		opts = append(opts, server.WithCashPublisher(pub))
	}

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, store, cfg.BaseCurrency, opts...),
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
			if err := runConsumer(ctx, cfg, store, mesh, logger, obs); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("fill consumer stopped with error", "err", err)
				stop()
			}
		}()
		// Live FX feed (WIRE-01d): fold market.v1 FX quotes into the rate cache.
		// Auxiliary to fill folding — a subscription failure degrades multi-
		// currency NAV (a stale/missing rate fails that valuation loudly) rather
		// than bringing the service down, so it does not stop() on error.
		if liveFX != nil {
			go func() {
				if err := runFXFeed(ctx, cfg, liveFX, mesh, logger, obs); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("FX feed stopped with error", "err", err)
				}
			}()
		}
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
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC. 0001_ledger.sql runs FORCE ROW LEVEL SECURITY with a
	// policy on it, and Append inserts VALUES (current_setting('app.tenant_id'),
	// ...). Without this, against the non-superuser role production requires, every
	// write ERRORS and every read returns ZERO ROWS — the IBOR silently holds
	// nothing. The Postgres tests set the GUC in their own pool and passed; this
	// composition root never did, which is exactly why it went unnoticed.
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
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
func runConsumer(ctx context.Context, cfg config.Config, store ledger.Store, mesh *transport.Mesh, logger *slog.Logger, obs *observability.Provider) error {
	folder, err := consume.NewFolder(cfg.Tenant, store, cfg.BaseCurrency)
	if err != nil {
		return err
	}

	busMetrics := bus.NewBusMetrics(obs.Registry)
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
	// Two folded FACT streams into the one journal: order.v1 fills → EntryTrade
	// (WIRE-01b) and accounting.v1 cash movements → EntryCash/Fee (WIRE-01f).
	subscribe := func(subject string, handler bus.EventHandler) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("accounting subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, handler)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}()
	}
	for _, subject := range cfg.FillSubjects {
		subscribe(subject, folder.Handle)
	}
	for _, subject := range cfg.CashSubjects {
		subscribe(subject, folder.HandleCash)
	}
	wg.Wait()
	return firstErr
}

// buildCashPublisher dials a producer connection and builds the WIRE-01f
// cash-movement publisher. It returns a close func for the producer client.
func buildCashPublisher(ctx context.Context, cfg config.Config, mesh *transport.Mesh) (*cashmove.Publisher, func(), error) {
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source + "-producer", TLSConfig: mesh.Client})
	if err != nil {
		return nil, nil, err
	}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: cfg.Source, ProducerVersion: version.String()})
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	pub, err := cashmove.NewPublisher(producer, nil)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return pub, func() { _ = client.Close() }, nil
}

// buildLiveFX parses the WIRE-01d FX configuration and, when configured, builds
// the live FX cache and appends the server options that make NAV value a
// multi-currency book without `fx` in the request: WithLiveFX (the point-in-time
// converter) + WithInstrumentCurrency (the security-master join). It returns the
// cache (nil when FX is unconfigured) for the composition root to subscribe. A
// malformed reference spec is an error — a mistyped FX/currency map must fail
// loud at startup, not silently mis-value.
func buildLiveFX(cfg config.Config, opts *[]server.Option) (*fxfeed.LiveFX, error) {
	instrCcy, err := fxfeed.ParsePairs(cfg.InstrumentCurrency)
	if err != nil {
		return nil, err
	}
	if len(instrCcy) > 0 {
		*opts = append(*opts, server.WithInstrumentCurrency(accounting.InstrumentCurrency(instrCcy)))
	}

	pairs, err := fxfeed.ParsePairs(cfg.FXPairs)
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, nil // live FX disabled
	}
	liveFX := fxfeed.New(cfg.BaseCurrency, pairs)
	*opts = append(*opts, server.WithLiveFX(liveFX.Converter))
	return liveFX, nil
}

// runFXFeed subscribes the market.v1 FX subjects and folds each quote into the
// live rate cache until ctx is canceled. It mirrors the fill consumer but is
// non-fatal: the cache is a last-value store the multi-currency NAV path reads,
// so a subscription failure degrades that path (a missing rate fails the
// valuation, completeness-gated) rather than stopping the service. The cache
// must SEE every FX event, so it subscribes under a per-source broadcast group
// distinct from the fill consumer's load-balanced group.
func runFXFeed(ctx context.Context, cfg config.Config, liveFX *fxfeed.LiveFX, mesh *transport.Mesh, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source + "-fx", TLSConfig: mesh.Client})
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

	group := cfg.Source + "-fx"
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for _, subject := range cfg.FXSubjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			logger.Info("accounting subscribing FX", "subject", subject, "group", group)
			err := consumer.Subscribe(ctx, subject, group, liveFX.Handler)
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

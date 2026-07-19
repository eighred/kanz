// market-data binary entrypoint (MODEL-01b). Consumes market.v1 events off the
// live NATS spine and folds them into the bitemporal price-history store. With
// MARKET_DATA_DATABASE_URL set the store is Postgres/Timescale (durable);
// without it the store is in-memory (local/dev — history is lost on restart).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/internal/marketdata"
	"github.com/kanz-eng/kanz/internal/marketdata/store"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/pkg/transport"
	"github.com/kanz-eng/kanz/services/market-data/internal/config"
	"github.com/kanz-eng/kanz/services/market-data/internal/feed"
	"github.com/kanz-eng/kanz/services/market-data/internal/server"
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

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("market-data listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	// Optional publisher (WIRE-01a): stream normalized market events ONTO the
	// spine. Runs concurrently with the consumer below; both share ctx so SIGTERM
	// stops them together. Empty MARKET_DATA_FEED ⇒ consumer-only (default).
	if cfg.Feed != "" && cfg.NATSURL != "" {
		go func() {
			if err := runFeed(ctx, cfg, logger, obs); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("feed publisher stopped with error", "err", err)
			}
		}()
	} else if cfg.Feed != "" {
		logger.Warn("MARKET_DATA_FEED set but no MARKET_DATA_NATS_URL — publisher disabled")
	}

	if cfg.NATSURL != "" {
		if err := runIngest(ctx, cfg, readiness, logger, obs); err != nil {
			logger.Error("ingestion stopped with error", "err", err)
		}
	} else {
		readiness.Set(true)
		logger.Warn("no MARKET_DATA_NATS_URL set — running http-only (no ingestion)")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// runIngest builds the store, subscribes every configured market subject over
// the live NATS spine, and blocks until ctx is canceled or a subscription
// fails. Each subject runs on its own goroutine (bus.Consumer.Subscribe
// blocks); the first non-cancellation error cancels the siblings and is
// returned — fail-fast, so a broken subscription brings ingestion down rather
// than running silently degraded.
func runIngest(ctx context.Context, cfg config.Config, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)

	st, closeStore, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeStore()
	if err := st.Ping(ctx); err != nil {
		return err
	}

	ingestor, err := marketdata.NewIngestor(st)
	if err != nil {
		return err
	}

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
			logger.Info("market-data subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, ingestor.Handler)
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

// runFeed builds the market-data PUBLISHER (WIRE-01a) and streams normalized
// events onto the spine until ctx is canceled. The pipeline is
// adapter → Gate (DQ, PARITY-01g) → BusSink (PARITY-01a) → bus.Producer, the
// same seam a live vendor adapter binds — only the Adapter differs. Today the
// dependency-free SimAdapter replays a synthetic session (offline/local feed);
// a real Bloomberg/Refinitiv/ICE Source plugs in here where its SDK exists.
func runFeed(ctx context.Context, cfg config.Config, logger *slog.Logger, obs *observability.Provider) error {
	if cfg.Feed != "sim" {
		return fmt.Errorf("market-data: unknown MARKET_DATA_FEED %q (want \"sim\" or empty)", cfg.Feed)
	}
	instruments := cfg.FeedInstruments
	if len(instruments) == 0 {
		instruments = []string{"AAPL", "MSFT", "GOOG"}
	}

	busMetrics := bus.NewBusMetrics(obs.Registry)
	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return err
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source + "-feed", TLSConfig: mesh.Client})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: cfg.Source + "-feed", ProducerVersion: version(), Metrics: busMetrics,
	})
	if err != nil {
		return err
	}
	sink, err := feed.NewBusSink(producer, cfg.FeedAssetClass)
	if err != nil {
		return err
	}
	// Staleness disabled (budget 0): the synthetic session carries fixed
	// timestamps, so wall-clock staleness would false-drop it. The Gate stays in
	// the path to prove the composition and still enforce ordering/gap; a single
	// monotonic pass trips neither. A live vendor feed sets a real budget + loops.
	gated := feed.NewGate(sink, 0, func(b feed.Breach) {
		logger.Warn("feed dq breach", "kind", b.Kind, "instrument", b.InstrumentID, "detail", b.Detail)
	})

	// The SimAdapter dumps its session at full speed (no rate limit) — so this is
	// a bounded synthetic BURST that smoke-tests the publish path end-to-end, then
	// the service continues serving/consuming. A real adapter streams continuously
	// via Reconnect; only the Adapter differs, the Gate→BusSink→Producer tail is
	// identical.
	adapter := &feed.SimAdapter{
		Name:    "SIM",
		Session: feed.SyntheticSession(time.Now(), time.Second, instruments, 100),
	}
	logger.Info("market-data feed publisher starting",
		"adapter", adapter.Vendor(), "instruments", instruments, "asset_class", cfg.FeedAssetClass)
	return adapter.Run(ctx, instruments, gated)
}

// openStore selects the durable Postgres/Timescale store when a DSN is set,
// otherwise the in-memory store. Returns a close func that tears down the pool
// (a no-op for the in-memory store).
func openStore(ctx context.Context, cfg config.Config) (store.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		return store.NewMemory(), func() {}, nil
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	return store.NewPostgres(pool), pool.Close, nil
}

// version is the service version stamped on telemetry. Hardcoded until the
// build injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }

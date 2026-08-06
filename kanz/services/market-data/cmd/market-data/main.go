// market-data binary entrypoint (MODEL-01b). Consumes market.v1 events off the
// live NATS spine and folds them into the bitemporal price-history store. With
// MARKET_DATA_DATABASE_URL set the store is Postgres/Timescale (durable);
// without it the store is in-memory, which openStore WARNS about and reports as
// kanz_market_data_price_history_durable=0 — correct on a one-replica dev rig
// and wrong on the shipped manifest, which runs two pods or more (#261).
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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/marketdata"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/market-data/internal/config"
	"github.com/eighred/kanz/services/market-data/internal/feed"
	"github.com/eighred/kanz/services/market-data/internal/server"
)

// priceHistoryDurable reports whether the price history survives a restart: 1
// when it is the Postgres/Timescale store, 0 when it is the in-memory one.
//
// The gauge is what makes the degraded posture ALERTABLE rather than merely
// readable — the start-up WARN in openStore scrolls off, while
// `kanz_market_data_price_history_durable == 0` stays true for as long as it is
// true. Deliberately the same contract as kanz_venue_orderview_durable
// (INFRA-M7a-2) and kanz_audit_log_durable (#236), because it is the same
// question asked of a third store.
var priceHistoryDurable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_market_data_price_history_durable",
	Help: "1 if the price history is backed by Postgres/Timescale (survives a restart), 0 if in-memory.",
})

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
		ServiceName:    cfg.Source,
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

	// Registered before openStore so the posture is on /metrics from the first
	// scrape, including the degraded one. A gauge nobody registered is a Set call
	// into the void — exactly the silence openStore's warning is about.
	obs.Registry.MustRegister(priceHistoryDurable)

	readiness := &server.Readiness{}
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())), httpserver.Standard())
	go func() {
		logger.Info("market-data listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
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

	var runErr error
	if cfg.NATSURL != "" {
		if err := runIngest(ctx, cfg, readiness, logger, obs); err != nil {
			logger.Error("ingestion stopped with error", "err", err)
			runErr = err
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

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	fatal.Raise(runErr)
	return fatal.Code()
}

// runIngest builds the store, subscribes every configured market subject over
// the live NATS spine, and blocks until ctx is canceled or a subscription
// fails. Each subject runs on its own goroutine (bus.Consumer.Subscribe
// blocks); the first non-cancellation error cancels the siblings and is
// returned — fail-fast, so a broken subscription brings ingestion down rather
// than running silently degraded.
func runIngest(ctx context.Context, cfg config.Config, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)

	st, closeStore, err := openStore(ctx, cfg, logger)
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

	producer, err := bus.NewProducer(client, feedProducerConfig(cfg, busMetrics))
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

// feedProducerConfig is the FEED publisher's producer identity, extracted from
// runFeed so it has a seam a test can reach: runFeed dials a broker before it
// builds anything, so the config it passes was untested by construction — which
// is exactly how the missing Tenant below survived (#245).
//
// Tenant is LOAD-BEARING, not decoration. This producer publishes from a
// SimAdapter/vendor loop, so there is no inbound delivery whose tenant the ctx
// could carry, and feed.BusSink sets no per-event Event.TenantID. cfg.Tenant is
// the only one of bus.Producer's three tenant sources available here; without it
// bus.Validate refuses every tick with "tenant_id required", the feed publishes
// nothing, and /readyz stays 200 the whole time.
func feedProducerConfig(cfg config.Config, metrics *bus.BusMetrics) bus.ProducerConfig {
	return bus.ProducerConfig{
		Source:          cfg.Source + "-feed",
		ProducerVersion: version.String(),
		Tenant:          cfg.Tenant,
		Metrics:         metrics,
	}
}

// openStore selects the durable Postgres/Timescale store when a DSN is set,
// otherwise the in-memory store. Returns a close func that tears down the pool
// (a no-op for the in-memory store).
//
// THIS ONE WARNS WHERE ITS PEERS REFUSE, and the difference is worth stating
// because five sibling services took the other decision in the same change
// (#261). accounting, alternatives, datamaster, regulatory and wealth all
// REFUSE an unasked-for in-memory store, because each of them holds something
// that exists NOWHERE ELSE — a ledger entry, a capital call, a named human's
// signed override, a hash-chain link, a household book — folded off a stream
// that ages out and read by a durable consumer that resumes at its last ack and
// never replays. Losing that store loses the only copy.
//
// This store is not that. Every price_observation in it arrived as a market.v1
// FACT off the spine, and the same FACT is archived to Kafka (EVT-09), which is
// the log of record. The in-memory store therefore loses HISTORY, not the only
// copy, and the live feed refills current marks within one tick of a restart. A
// single pod holding every observation since process start is a real, if
// degraded, operating posture — the same call the OMS makes (openStores) and for
// the same reason.
//
// THE PRECONDITION IS EXACTLY ONE REPLICA, AND THE SHIPPED MANIFEST BREAKS IT.
// All pods share one durable consumer group, so a FACT is delivered to ONE of
// them. With a durable store that is the whole point — the idempotent upsert
// makes any pod's fold equivalent. With a map per pod it means each replica
// holds a DISJOINT SLICE of the history, and a query is answered by whichever
// pod the Service happened to pick: not stale, WRONG, and differently wrong on
// each retry. infra/deploy/market-data-deploy.yaml sets replicas: 2 as a floor
// and market-data-scaledobject.yaml scales it 2..16, so the degraded posture is
// only ever correct on a dev rig. That is what the warning has to say, and why
// the gauge matters more than the log line: the risk engine marks positions
// against this store.
//
// By the time openStore runs, config.Load has already refused to start if the
// _FILE mount was DECLARED but unreadable (secret.Read), so an empty DSN here
// can only mean no DSN was ever configured.
func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (store.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		logger.Warn("PRICE HISTORY IS IN-MEMORY — no MARKET_DATA_DATABASE_URL (or _FILE mount). Everything "+
			"folded so far is DISCARDED on the next restart, rollout or eviction, and the risk engine marks "+
			"positions against this store. This deployment MUST run exactly ONE replica: pods share one "+
			"durable consumer group, so N pods hold N DISJOINT slices of the history and a query is answered "+
			"by whichever one the Service picked",
			"fix", "set MARKET_DATA_DATABASE_URL; the shipped manifest runs replicas: 2 and KEDA scales it to 16",
			"gauge", "kanz_market_data_price_history_durable=0")
		priceHistoryDurable.Set(0)
		return store.NewMemory(), func() {}, nil
	}
	pool, err := pg.NewGlobalPool(ctx, cfg.DatabaseURL,
		"price_observations is UNIVERSAL MARKET DATA and deliberately carries no RLS — a EURUSD mid is "+
			"the same fact for every tenant, and the risk engine marks every tenant's positions against "+
			"this one store. Scoping it per tenant would shard the price history by whoever happened to "+
			"observe the tick")
	if err != nil {
		return nil, nil, err
	}
	priceHistoryDurable.Set(1)
	return store.NewPostgres(pool), pool.Close, nil
}

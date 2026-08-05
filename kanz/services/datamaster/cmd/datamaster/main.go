// datamaster (golden-source) binary entrypoint (MASTER-01). It resolves a golden
// security master across vendor feeds (survivorship + identifier crosswalk),
// arbitrates multi-source prices with tolerance/staleness checks, and serves a
// pricing-oversight exception queue with a human-override audit trail — the
// trusted-data foundation every analytic silently assumes (ROI #41).
//
// This is the composition root: it opens the durable stores, builds the vendor
// feeds, and starts the projector that folds the feeds into the golden store.
//
// The vendors are mounted file drops (DATA-M8c) — DATAMASTER_REF_FILES and
// DATAMASTER_PRICE_FILES — which is how Bloomberg Data License / Refinitiv
// DataScope actually deliver reference data. A vendor with a REST reference API
// implements the same feed.RefSource / feed.PriceSource seams beside them and
// nothing downstream changes. With no vendor configured the projector has nothing
// to project and the master stays empty: an empty master, not an invented one.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/datamaster/internal/config"
	"github.com/eighred/kanz/services/datamaster/internal/feed"
	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
	"github.com/eighred/kanz/services/datamaster/internal/projector"
	"github.com/eighred/kanz/services/datamaster/internal/server"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

// masterDurable reports whether the golden master and its pricing-oversight
// exception queue survive a restart: 1 when they are the Postgres stores, 0 when
// they are the in-memory pair an operator opted into with
// DATAMASTER_ALLOW_EPHEMERAL_MASTER.
//
// The gauge is what makes the degraded posture ALERTABLE rather than merely
// readable, and it carries a second fact besides durability: the in-memory path
// also has no per-tenant cycle lock, so 0 here means every replica is refreshing
// against a metered vendor API. Same contract as kanz_audit_log_durable (#236).
var masterDurable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_datamaster_master_durable",
	Help: "1 if the golden master and exception queue are backed by Postgres (survive a restart), 0 if in-memory.",
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
		ServiceName:    "datamaster",
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

	feeds, err := buildFeeds(cfg)
	if err != nil {
		logger.Error("vendor feed configuration is invalid", "err", err)
		return 2
	}
	if err := guardSim(feeds, cfg.AllowSim); err != nil {
		logger.Error("refusing to start", "err", err)
		return 2
	}
	for _, f := range feeds {
		logger.Info("vendor feed wired", "vendor", f.Vendor())
	}

	// Registered before openStores so the posture is on /metrics from the first
	// scrape, including the degraded one.
	obs.Registry.MustRegister(masterDurable)

	golden, exceptions, cycleLock, closeStores, err := openStores(ctx, cfg, logger)
	if err != nil {
		logger.Error("store init failed", "err", err)
		return 2
	}
	defer closeStores()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, cfg.Tenant, golden, exceptions, feeds, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("datamaster listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	if len(feeds) > 0 {
		// The cycle lock elects ONE replica per cycle. Concurrent projection is
		// harmless to the data (idempotent, content-identical writes) but not to the
		// vendor: N pods would make N rounds of metered API calls for one cycle's
		// worth of information. Nil for the in-memory store — a lone process
		// projecting into its own map has nobody to race.
		opts := []projector.Option{}
		if cycleLock != nil {
			opts = append(opts, projector.WithCycleLock(cycleLock))
		}
		proj := projector.New(feeds, golden, exceptions, logger, opts...)
		// Project once before serving, so the first reader sees a mastered book
		// rather than a cold store. A failure here does NOT stop the service: the
		// durable store still holds the last good projection, and serving that is
		// strictly better than serving nothing. It never serves an invented one.
		if err := proj.Refresh(ctx); err != nil {
			logger.Error("initial golden projection failed; serving whatever was last mastered", "err", err)
		}
		go proj.Run(ctx, cfg.RefreshInterval)
	} else {
		logger.Warn("no vendor feeds configured; the golden master will not be refreshed (set DATAMASTER_REF_FILES / DATAMASTER_PRICE_FILES to a mounted vendor drop)")
	}
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

// openStores selects the durable Postgres golden store + exception queue when a
// DSN is set, and otherwise REFUSES TO START unless the deployment has said out
// loud that it accepts an ephemeral master. Returns a close func that tears down
// the pool (a no-op in memory). Both satisfy the store seams, so the projector
// and the server are identical either way.
//
// store.PostgresGolden and store.PostgresExceptions have existed, tested, since
// MASTER-01: nothing ever constructed them. Until now every operator override — a
// named human's signed decision to accept a price the system flagged — lived in a
// map and vanished on the next restart, silently (#261).
//
// WHY THIS ONE REFUSES. The deploy manifest already gives the reason and had no
// way to enforce it — infra/deploy/datamaster-deploy.yaml's own header says the
// exception queue "is an audit surface", that an override without a DSN "is GONE
// on the next restart — a compliance record silently deleted by a rolling
// update", and that "the in-memory store is a laptop convenience, never a
// deployment". That last sentence was a comment; this is the same sentence as
// code. Unlike the folded stores in accounting and wealth, an override does not
// even arrive on the bus: it originates at this service's own API, so there is
// no stream to rebuild it from, ever.
//
// AND THE CYCLE LOCK DISAPPEARS WITH IT, which is the part no log line would
// have shown. This function returns a nil CycleLock on the in-memory path, so
// run() never applies projector.WithCycleLock and EVERY replica runs EVERY
// vendor refresh. The manifest is explicit that concurrent projection was never
// a data hazard but a BILLING and RATE-LIMIT one — "vendor reference data is
// metered per call, and N pods made N rounds of API requests for one cycle's
// worth of information" — and it runs replicas: 2. So a forgotten DSN quietly
// doubles a metered vendor bill on top of discarding the audit surface.
//
// config.Load has already refused to start if the _FILE mount was DECLARED but
// unreadable (secret.Read), so an empty DSN here can only mean none was ever
// configured.
func openStores(ctx context.Context, cfg config.Config, logger *slog.Logger) (store.GoldenStore, store.ExceptionStore, projector.CycleLock, func(), error) {
	if cfg.DatabaseURL == "" {
		if !cfg.AllowEphemeralMaster {
			return nil, nil, nil, nil, errors.New("no DATAMASTER_DATABASE_URL (or _FILE mount): the golden " +
				"master and the pricing-oversight EXCEPTION QUEUE would be IN-MEMORY. Every operator " +
				"override — a named human's signed decision to accept a price the system flagged — would be " +
				"DISCARDED by the next restart or rolling update, and it originates here rather than on the " +
				"bus, so nothing can rebuild it. The per-tenant cycle lock would also be absent, so every " +
				"replica would run every refresh against a vendor API metered per call. Set " +
				"DATAMASTER_DATABASE_URL, or set DATAMASTER_ALLOW_EPHEMERAL_MASTER=true to accept it — in " +
				"which case this deployment MUST run exactly one replica and is not an audit surface")
		}
		logger.Warn("GOLDEN MASTER AND EXCEPTION QUEUE ARE IN-MEMORY — DATAMASTER_ALLOW_EPHEMERAL_MASTER "+
			"accepted an ephemeral audit surface. Every operator override is DISCARDED on the next restart "+
			"and cannot be rebuilt. The per-tenant cycle lock is ABSENT on this path, so this deployment "+
			"MUST run exactly ONE replica: every pod would otherwise run every refresh against a vendor API "+
			"that bills per call",
			"fix", "set DATAMASTER_DATABASE_URL (or its _FILE mount); the shipped manifest runs replicas: 2",
			"gauge", "kanz_datamaster_master_durable=0")
		masterDurable.Set(0)
		return store.NewMemoryGoldenStore(), store.NewQueueStore(pricing.NewQueue()), nil, func() {}, nil
	}
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC, so Postgres RLS scopes all reads/writes to it. A
	// non-superuser DB role is required for FORCE RLS to apply.
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// The cycle lock is keyed on the TENANT: a shared key would let one tenant's
	// deployment starve every other tenant's projector forever.
	masterDurable.Set(1)
	return store.NewPostgresGolden(pool), store.NewPostgresExceptions(pool),
		store.NewPostgresCycleLock(pool, cfg.Tenant), pool.Close, nil
}

// buildFeeds assembles the vendor feeds.
//
// The REAL feeds are the mounted vendor drops (DATA-M8c): DATAMASTER_REF_FILES
// gives each vendor's reference extract and DATAMASTER_PRICE_FILES its price
// extract, which is how Bloomberg Data License / Refinitiv DataScope actually
// deliver. Reference and price are separate sources because they are separate
// files with separate cadences — a vendor may supply either or both.
//
// The canned SimFeed set is built ONLY under DATAMASTER_ALLOW_SIM, and only when
// no real vendor is configured: a laptop with no vendor account still needs to run
// the resolve→arbitrate→persist path. It is deliberately not clean — three vendors
// quote SIM1 at 100, 101 and 130, so the consensus is 101 and the third breaches
// tolerance. A simulator that never raises a break cannot exercise the surface it
// exists to exercise.
func buildFeeds(cfg config.Config) ([]feed.VendorFeed, error) {
	var feeds []feed.VendorFeed

	for _, vendor := range sortedVendors(cfg.RefFiles) {
		src, err := feed.NewFileRefSource(vendor, cfg.VendorPriority[vendor], cfg.RefFiles[vendor])
		if err != nil {
			return nil, err
		}
		feeds = append(feeds, feed.NewReferenceAdapter(src, nil))
	}
	for _, vendor := range sortedVendors(cfg.PriceFiles) {
		src, err := feed.NewFilePriceSource(vendor, cfg.PriceFiles[vendor])
		if err != nil {
			return nil, err
		}
		feeds = append(feeds, feed.NewPriceAdapter(src, nil))
	}
	if len(feeds) > 0 {
		return feeds, nil
	}
	return simFeeds(cfg), nil
}

// sortedVendors keeps feed construction deterministic (a map range is not).
func sortedVendors(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// simFeeds is the canned developer book. Nil unless DATAMASTER_ALLOW_SIM.
func simFeeds(cfg config.Config) []feed.VendorFeed {
	if !cfg.AllowSim {
		return nil
	}
	now := time.Now()
	return []feed.VendorFeed{
		feed.SimFeed{
			Name: "SIM_A",
			VRecords: []master.VendorRecord{{
				Vendor: "SIM_A", InstrumentID: "SIM1", Priority: 0,
				Identifiers:  master.Identifiers{ISIN: "US0000000001", FIGI: "BBG000000001"},
				AssetClass:   "EQUITY",
				CurrencyCode: "USD",
				AsOf:         now,
			}},
			Candidates: []pricing.Candidate{{InstrumentID: "SIM1", Source: "SIM_A", Price: dec.Rat("100"), AsOf: now}},
		},
		feed.SimFeed{
			Name: "SIM_B",
			VRecords: []master.VendorRecord{{
				Vendor: "SIM_B", InstrumentID: "SIM1", Priority: 1,
				Identifiers:  master.Identifiers{ISIN: "US0000000001", SEDOL: "0000001"},
				Description:  "Simulated Instrument 1",
				CurrencyCode: "USD",
				AsOf:         now,
			}},
			Candidates: []pricing.Candidate{{InstrumentID: "SIM1", Source: "SIM_B", Price: dec.Rat("101"), AsOf: now}},
		},
		feed.SimFeed{
			Name:       "SIM_C",
			Candidates: []pricing.Candidate{{InstrumentID: "SIM1", Source: "SIM_C", Price: dec.Rat("130"), AsOf: now}},
		},
	}
}

// guardSim refuses to start with a simulated vendor feed unless it was asked for
// explicitly.
//
// THE FABRICATION TRAP. A SimFeed's canned records are, once resolved and written
// to golden_records, indistinguishable from mastered vendor data: they carry
// provenance, they are served from the same endpoint, and every analytic
// downstream treats the golden record as the trusted identity of an instrument.
// The market-ingest incident was exactly this shape — a simulator reachable by
// forgetting a flag, publishing invented numbers that nothing downstream could
// tell apart from real ones. A simulated master must be an explicit, loud choice,
// never a default that survives into an environment nobody re-checked.
func guardSim(feeds []feed.VendorFeed, allowSim bool) error {
	if allowSim {
		return nil
	}
	for _, f := range feeds {
		if _, ok := f.(feed.SimFeed); ok {
			return errors.New("a simulated vendor feed (" + f.Vendor() + ") is wired but DATAMASTER_ALLOW_SIM is not set: " +
				"refusing to master a security book out of invented data")
		}
	}
	return nil
}

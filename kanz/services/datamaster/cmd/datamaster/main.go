// datamaster (golden-source) binary entrypoint (MASTER-01). It resolves a golden
// security master across vendor feeds (survivorship + identifier crosswalk),
// arbitrates multi-source prices with tolerance/staleness checks, and serves a
// pricing-oversight exception queue with a human-override audit trail — the
// trusted-data foundation every analytic silently assumes (ROI #41).
//
// This is the composition root: it opens the durable stores, builds the vendor
// feeds, and starts the projector that folds the feeds into the golden store. A
// real Bloomberg/Refinitiv/ICE adapter wires in behind the feed.RefSource seam
// here — feed.NewReferenceAdapter already normalizes such a source into the
// VendorFeed contract; nothing implements RefSource yet, so with no feeds
// configured the projector has nothing to project and the master stays empty.
// That is the honest state: an empty master, not an invented one.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/datamaster/internal/config"
	"github.com/kanz-eng/kanz/services/datamaster/internal/feed"
	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
	"github.com/kanz-eng/kanz/services/datamaster/internal/projector"
	"github.com/kanz-eng/kanz/services/datamaster/internal/server"
	"github.com/kanz-eng/kanz/services/datamaster/internal/store"
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
		ServiceName:    "datamaster",
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

	feeds := buildFeeds(cfg)
	if err := guardSim(feeds, cfg.AllowSim); err != nil {
		logger.Error("refusing to start", "err", err)
		os.Exit(2)
	}

	golden, exceptions, closeStores, err := openStores(ctx, cfg)
	if err != nil {
		logger.Error("store init failed", "err", err)
		os.Exit(2)
	}
	defer closeStores()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, golden, exceptions, feeds, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("datamaster listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	if len(feeds) > 0 {
		proj := projector.New(feeds, golden, exceptions, logger)
		// Project once before serving, so the first reader sees a mastered book
		// rather than a cold store. A failure here does NOT stop the service: the
		// durable store still holds the last good projection, and serving that is
		// strictly better than serving nothing. It never serves an invented one.
		if err := proj.Refresh(ctx); err != nil {
			logger.Error("initial golden projection failed; serving whatever was last mastered", "err", err)
		}
		go proj.Run(ctx, cfg.RefreshInterval)
	} else {
		logger.Warn("no vendor feeds configured; the golden master will not be refreshed (implement feed.RefSource to wire a vendor)")
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

// openStores selects the durable Postgres golden store + exception queue when a
// DSN is set, otherwise the in-memory pair. Returns a close func that tears down
// the pool (a no-op in memory). Both satisfy the store seams, so the projector and
// the server are identical either way.
//
// store.PostgresGolden and store.PostgresExceptions have existed, tested, since
// MASTER-01: nothing ever constructed them. Until now every operator override — a
// named human's signed decision to accept a price the system flagged — lived in a
// map and vanished on the next restart.
func openStores(ctx context.Context, cfg config.Config) (store.GoldenStore, store.ExceptionStore, func(), error) {
	if cfg.DatabaseURL == "" {
		return store.NewMemoryGoldenStore(), store.NewQueueStore(pricing.NewQueue()), func() {}, nil
	}
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC, so Postgres RLS scopes all reads/writes to it. A
	// non-superuser DB role is required for FORCE RLS to apply.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, nil, nil, err
	}
	tenant := cfg.Tenant
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, nil, nil, err
	}
	return store.NewPostgresGolden(pool), store.NewPostgresExceptions(pool), pool.Close, nil
}

// buildFeeds assembles the vendor feeds. There is no production feed to build: no
// type implements feed.RefSource, so a real vendor is not wired and this returns
// nothing. The canned SimFeed set is built ONLY under DATAMASTER_ALLOW_SIM, for a
// developer running the resolve→arbitrate→persist path with no vendor account.
//
// The canned book is deliberately not clean: three vendors quote SIM1 at 100, 101
// and 130, so the consensus is 101 and the third breaches tolerance. A simulator
// that never produces a break cannot exercise the surface it exists to exercise —
// the developer would see an empty oversight queue and learn nothing about it.
func buildFeeds(cfg config.Config) []feed.VendorFeed {
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
			Candidates: []pricing.Candidate{{InstrumentID: "SIM1", Source: "SIM_A", Price: 100, AsOf: now}},
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
			Candidates: []pricing.Candidate{{InstrumentID: "SIM1", Source: "SIM_B", Price: 101, AsOf: now}},
		},
		feed.SimFeed{
			Name:       "SIM_C",
			Candidates: []pricing.Candidate{{InstrumentID: "SIM1", Source: "SIM_C", Price: 130, AsOf: now}},
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

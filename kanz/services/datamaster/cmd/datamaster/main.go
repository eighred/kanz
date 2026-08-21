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
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
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

	golden, exceptions, proposals, outboxQueue, cycleLock, closeStores, err := openStores(ctx, cfg, logger)
	if err != nil {
		logger.Error("store init failed", "err", err)
		return 2
	}
	defer closeStores()

	// THE OVERRIDE FACT REACHES THE ESTATE (#410).
	//
	// The FACT is committed to the outbox in the SAME transaction as the override,
	// so this relay is what drains it — not what makes the override durable. That
	// split is the whole design: an override never depends on the broker being up,
	// and a FACT is never lost because it was down.
	if outboxQueue != nil {
		if cfg.NATSURL == "" {
			// WARN, and this is a BACKLOG rather than a disable. Overrides keep
			// working and their announcements accumulate in Postgres, so
			// services/audit — the signed, tamper-evident store an examiner reads —
			// falls silently behind by exactly the amount below.
			logger.Warn("no DATAMASTER_NATS_URL: override FACTs are committed to the outbox and NOTHING "+
				"DRAINS IT. Every override still applies and nothing is lost, but the platform's audit "+
				"trail will not contain them until a relay runs",
				"gauge", "kanz_datamaster_outbox_oldest_pending_seconds")
		} else {
			mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
			if err != nil {
				logger.Error("mesh init failed", "err", err)
				return 2
			}
			defer func() { _ = mesh.Close() }()
			// ONE BusMetrics, CREATED BEFORE THE DIAL (#636). The connection's state and
			// its transitions are only exported when DialNATS is given this; a service
			// that publishes and never consumes contributes no kanz_bus_consume_total,
			// so BusConsumerStalled cannot see it and this gauge is the only signal that
			// its spine is gone.
			busMetrics := bus.NewBusMetrics(obs.Registry)
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client, Metrics: busMetrics})
			if err != nil {
				logger.Error("bus dial failed", "err", err)
				return 2
			}
			producer, err := bus.NewProducer(client, bus.ProducerConfig{
				Source: cfg.Source, ProducerVersion: version.String(), Tenant: cfg.Tenant,
				Metrics: busMetrics,
			})
			if err != nil {
				logger.Error("producer init failed", "err", err)
				return 2
			}
			relay, err := outbox.NewRelay(outboxQueue, producer, logger, outbox.WithInterval(cfg.OutboxInterval))
			if err != nil {
				logger.Error("outbox relay init failed", "err", err)
				return 2
			}
			go func() {
				// A DEAD RELAY IS A SILENT AUDIT GAP, so its exit is logged rather
				// than discarded. It does not bring the service down: overrides keep
				// committing to the outbox and the other replica drains them, which
				// is the whole reason the FACT is not published inline. The gauge
				// below is what an operator alerts on.
				if err := relay.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("outbox relay stopped — override FACTs are accumulating and the audit "+
						"trail is falling behind", "err", err)
				}
			}()
			// THE AGE OF THE OLDEST UNPUBLISHED RECORD IS THE ALERTABLE SIGNAL, and
			// it is a GaugeFunc because it must be true when nothing is happening.
			// An empty outbox and a dead relay both produce zero errors, zero failed
			// publishes and a silent log; the only number that tells them apart is
			// how long the front of the queue has waited.
			obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name: "kanz_datamaster_outbox_oldest_pending_seconds",
				Help: "Age of the oldest override FACT committed to the outbox and not yet published. Zero " +
					"means drained. A climbing value means overrides the audit trail has not heard about " +
					"(#410) — the decisions are safe in Postgres, the audit record is behind.",
			}, func() float64 { return relay.OldestPendingAge(ctx) }))
			logger.Info("override FACTs will be published", "subject", store.SubjectExceptionOverridden)
		}
	}

	readiness := &server.Readiness{}
	// MAKER-CHECKER (#410). Armed by DATAMASTER_REQUIRE_DUAL_CONTROL, which
	// defaults to false — the control is built and COUNTED here, and armed with
	// the list of override callers in hand.
	//
	// ARMING WITHOUT A PROPOSAL STORE IS A REFUSAL TO START, not a downgrade to
	// single-signature. Every override would be taken, recorded nowhere, and
	// answered 202 — an act that neither takes effect nor reports why, which is
	// the precise failure #410 exists to end. Falling back to the old behaviour
	// would be worse still: an operator who set the flag would believe dual
	// control was on while one person kept relaxing the control alone.
	if cfg.RequireDualControl && proposals == nil {
		logger.Error("DATAMASTER_REQUIRE_DUAL_CONTROL is set but there is nowhere to record a pending " +
			"override, so every override would be accepted and then lost. Set DATAMASTER_DATABASE_URL, " +
			"or unset DATAMASTER_REQUIRE_DUAL_CONTROL")
		return 2
	}
	if !cfg.RequireDualControl {
		// WARN, not Info: "one person can relax a pricing control on this
		// deployment" is a sentence that must have been read before anyone signs
		// off on the valuation, and Info is where it gets filtered out.
		logger.Warn("MAKER-CHECKER IS NOT ARMED on the pricing override — one person can accept a price "+
			"the system flagged, on their own authority (#410). Every override still records whether a "+
			"second person signed it",
			"arm_with", "DATAMASTER_REQUIRE_DUAL_CONTROL=true",
			"counter", "kanz_datamaster_overrides_total{signatures=\"single_signed\"}")
	}
	// LAPSED PROPOSALS ARE LISTED, AND THEN THEY ARE REMOVED (#563).
	//
	// A proposal nobody signs is never claimed, and Claim is the only thing that
	// deletes one — so before this, every override ever proposed and left unsigned
	// stayed in exception_override_proposals for the life of the deployment. It
	// was invisible growth: Pending filtered the rows out at query time, so the
	// table grew with the lapse rate and no surface ever showed it.
	//
	// The retention is what a proposer can still see, so purging is not a cleanup
	// job that happens to be tidy — it is the moment the record stops existing.
	// That is why zero is loud rather than quiet.
	if armProposalPurge(cfg, proposals, logger) {
		go purgeLapsedProposals(ctx, proposals, cfg.ProposalPurgeInterval, cfg.LapsedProposalRetention, logger)
	}

	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, cfg.Tenant, golden, exceptions, feeds,
		server.WithMetrics(obs.MetricsHandler()),
		server.WithDualControl(server.DualControl{
			Proposals:  proposals,
			Require:    cfg.RequireDualControl,
			TTL:        cfg.DualControlTTL,
			Registerer: obs.Registry,
		})), httpserver.Standard())
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
func openStores(ctx context.Context, cfg config.Config, logger *slog.Logger) (store.GoldenStore, store.ExceptionStore, store.ProposalStore, outbox.Queue, projector.CycleLock, func(), error) {
	if cfg.DatabaseURL == "" {
		if !cfg.AllowEphemeralMaster {
			return nil, nil, nil, nil, nil, nil, errors.New("no DATAMASTER_DATABASE_URL (or _FILE mount): the golden " +
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
		// The proposal store follows the same posture: in-memory here, which means a
		// pending override is approvable only on the pod that took it — acceptable
		// only because this path already MUST run exactly one replica.
		// NO OUTBOX ON THE EPHEMERAL PATH. The in-memory exception queue never
		// enqueues a FACT, so a relay would drain nothing forever; returning nil
		// keeps 'there is no announcement here' explicit rather than staffed.
		return store.NewMemoryGoldenStore(), store.NewQueueStore(pricing.NewQueue()),
			store.NewMemoryProposals(), nil, nil, func() {}, nil
	}
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC, so Postgres RLS scopes all reads/writes to it. A
	// non-superuser DB role is required for FORCE RLS to apply.
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	// The cycle lock is keyed on the TENANT: a shared key would let one tenant's
	// deployment starve every other tenant's projector forever.
	masterDurable.Set(1)
	// The outbox reads through the SAME tenant-scoped pool, which is what keeps
	// the relay unable to publish another tenant's FACTs — see outbox.Relay.
	return store.NewPostgresGolden(pool), store.NewPostgresExceptions(pool, cfg.Tenant), store.NewPostgresProposals(pool),
		outbox.NewPostgres(pool, "datamaster"),
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

// armProposalPurge states the posture and reports whether the loop should run.
//
// A FUNCTION SO IT CAN BE ASSERTED. The decision is three lines and it lives at
// the composition root, which no unit test reaches and which this repository has
// shipped crashes through before. What has to be true is not that a line is
// logged but that the DISABLED case is logged at WARN — an Info here reads as
// "purging is configured" to the one person who would otherwise notice the table
// growing.
func armProposalPurge(cfg config.Config, proposals store.ProposalStore, logger *slog.Logger) bool {
	if proposals == nil {
		// No store, no proposals, nothing to purge. Not a posture worth a line:
		// openStores has already said what this deployment is.
		return false
	}
	if cfg.ProposalPurgeInterval <= 0 {
		logger.Warn("DATAMASTER_PROPOSAL_PURGE_INTERVAL is 0 — lapsed override proposals are never "+
			"removed. They stay LISTED, so this grows in plain sight rather than silently, but "+
			"exception_override_proposals now grows with the lapse rate for the life of this "+
			"deployment (#563)",
			"retention", cfg.LapsedProposalRetention)
		return false
	}
	logger.Info("lapsed override proposals are listed then purged",
		"retention", cfg.LapsedProposalRetention, "every", cfg.ProposalPurgeInterval)
	return true
}

// purgeLapsedProposals removes proposals that lapsed longer than retention ago.
//
// NO LEADER ELECTION, unlike the projection cycle above. PurgeLapsed is an
// idempotent DELETE, so replicas racing on the same rows produce one outcome and
// a smaller count on the losers — whereas a duplicated projection cycle would
// write the book twice. Electing a leader for this would add a failure mode
// (nobody purges because the lock holder is wedged) to buy nothing.
func purgeLapsedProposals(ctx context.Context, proposals store.ProposalStore, every, retention time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := proposals.PurgeLapsed(ctx, time.Now().Add(-retention))
			if err != nil {
				// WARN and continue. A purge that cannot run is a table that grows,
				// not an override that goes wrong, and taking the service down over
				// it would trade a slow problem for an immediate one.
				logger.Warn("cannot purge lapsed override proposals", "err", err)
				continue
			}
			if n > 0 {
				logger.Info("purged lapsed override proposals", "count", n, "older_than", retention)
			}
		}
	}
}

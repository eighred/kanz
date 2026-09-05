// accounting (IBOR) binary entrypoint (IBOR-01). It folds OMS-01 fills and
// cash/corporate-action events into the investment book-of-record and serves
// point-in-time NAV and custodian reconciliation — the system-of-record beneath
// every number (ROI #36). With ACCOUNTING_DATABASE_URL set the journal is the
// durable Postgres store; without it openStore REFUSES TO START unless
// ACCOUNTING_ALLOW_EPHEMERAL_LEDGER=true says the deployment accepts a book of
// record that does not survive a restart (#261).
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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	accounting "github.com/eighred/kanz/services/accounting/internal"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/fxfeed"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/server"
)

// ledgerDurable reports whether the IBOR journal survives a restart: 1 when it
// is the Postgres store, 0 when it is the in-memory one an operator opted into
// with ACCOUNTING_ALLOW_EPHEMERAL_LEDGER.
//
// The gauge is what makes the degraded posture ALERTABLE rather than merely
// readable — the start-up WARN scrolls off, while `kanz_accounting_ledger_durable
// == 0` can be alerted on for as long as it is true. Same contract as
// kanz_audit_log_durable (#236) and kanz_venue_orderview_durable (INFRA-M7a-2).
var ledgerDurable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_accounting_ledger_durable",
	Help: "1 if the IBOR journal is backed by Postgres (survives a restart), 0 if in-memory.",
})

// A CASH-BALANCE ANNOUNCEMENT THAT NEVER LEFT THIS PROCESS (#622). The fold
// deliberately does not fail on it — the ledger is the book of record and
// nacking would stall it — so this is the only estate-wide signal that the
// announcements stopped.
//
// SEEDED AT ZERO, and the seeding is the half that is easy to skip: an
// unseeded counter that never fires reads exactly like a counter nobody
// registered, so an operator cannot tell "no announcements lost" from "no
// metric". The alternative to knowing is inferring it from orders being
// refused, because a consumer ages an unheard balance out to UNKNOWN and the
// buying-power rule fails closed on it.
var announcementsLost = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "kanz_accounting_cash_announcements_lost_total",
	Help: "Cash-balance announcements that could not be published. Non-zero means downstream " +
		"consumers are ageing these portfolios out to UNKNOWN, which refuses their orders " +
		"under a buying-power mandate.",
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
		ServiceName:    "accounting",
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

	// SEC-M3: ONE workload identity for the process, shared by all three bus
	// dials below (consumer, cash publisher, FX feed). The production broker
	// requires a client SVID; a nil TLSConfig is a plaintext client it refuses at
	// the handshake. Built here rather than per-dial because an X509Source is a
	// live rotation watcher — three of them would be three watchers for one
	// identity.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("spiffe source init failed", "err", err)
		return 2
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())

	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	// Registered before openStore so the posture is on /metrics from the first
	// scrape, including the degraded one. A gauge nobody registered is a Set call
	// into the void — exactly the silence this branch is about.
	obs.Registry.MustRegister(ledgerDurable)

	// WHICH KINDS OF ENTRY ANYTHING ACTUALLY PRODUCES (#588). Registered here for
	// the same reason as the gauge above — before anything can fail — so the
	// posture is on /metrics from the first scrape. It is what lets an operator
	// tell "this deployment does not process corporate actions" from "it does,
	// and none occurred": today the book looks identical either way.
	stateEntrySourcePosture(obs.Registry, logger, cfg)

	store, ledgerPool, closeStore, err := openStore(ctx, cfg, logger)
	if err != nil {
		logger.Error("store init failed", "err", err)
		return 2
	}
	defer closeStore()

	// Live FX for multi-currency NAV (WIRE-01d): build the latest-rate cache and
	// the default instrument→currency join from config, and hand the server a
	// live FX provider so NAV values a multi-currency book with no `fx` in the
	// request. A bad reference spec fails loud at startup. Off when unconfigured.
	// Ledger checkpoints (#229). Registered before the server is built so the
	// read path can count the materializations that could NOT be served from a
	// checkpoint — the only signal that this job has stopped working.
	snapMetrics := ledger.NewSnapshotMetrics(obs.Registry)
	// ONE BusMetrics FOR THE PROCESS, created here and passed down (#636).
	//
	// It used to be constructed inside runConsumer AND runFXFeed, both of which
	// receive the SAME *observability.Provider and run CONCURRENTLY whenever
	// ACCOUNTING_NATS_URL is set and a live FX feed is configured.
	// NewBusMetrics calls Registry.MustRegister, which panics on a duplicate
	// collector — so that pair of goroutines crashed the process on startup, in a
	// perfectly valid configuration, whichever of them registered second.
	// Unit tests never saw it because nothing constructs this composition root.
	busMetrics := bus.NewBusMetrics(obs.Registry)
	// NO WithMetrics: /metrics moved to its own listener (#447). See the split
	// below — an API that shares the scraped port is reachable from
	// kanz-observability whatever the NetworkPolicy says.
	// THE CUSTODY BREAK QUEUE AND THE SCHEDULED RUNS SHARE ONE STORE (#962).
	// Built here rather than inside runConsumer because the operator surface on
	// the server needs the same instance: two stores would show an operator an
	// empty queue while the scheduler filled another one, and every assignment
	// they made would be written where nothing reads it.
	custodyStore := newCustodyStore(ledgerPool)
	// THE CUSTODY DECLARATION IS PARSED HERE, BEFORE EITHER CONSUMER OF IT (#1025).
	//
	// The scheduled runs and the server's ad-hoc reconcile endpoint scope the book
	// side by the SAME *custody.BookScope: two built from one environment agree
	// today and diverge the first time one is rebuilt from something else, and a
	// disagreement about which accounts a custodian holds is invisible until a real
	// break is buried in the fabricated ones.
	//
	// IT IS ALSO UNCONDITIONAL, where the plane below is not. runConsumer only runs
	// with a broker configured, so parsing there left a broker-less deployment —
	// the "read/reconcile only" default — serving the reconcile endpoint with a
	// declaration nothing had ever validated. A malformed one now refuses the start
	// on every deployment that can answer the route.
	custodyCfg, err := buildCustodyConfig(cfg, logger)
	if err != nil {
		logger.Error("custody config invalid", "err", err)
		return 2
	}
	opts := []server.Option{
		server.WithSnapshotMetrics(snapMetrics),
		server.WithBreakStore(custodyStore),
		server.WithCustodyBookScope(custodyCfg.scope),
	}
	liveFX, err := buildLiveFX(cfg, &opts)
	if err != nil {
		logger.Error("FX config invalid", "err", err)
		return 2
	}

	// Cash-movement producer (WIRE-01f): with a broker configured, mount the
	// cash-movement endpoint whose posts are EMITTED as accounting.v1 FACTs and
	// folded back by the consumer below — the event-sourced path, so booking a
	// subscription/redemption/fee is replayable. A dial failure is fatal (a
	// configured broker that won't connect is a misconfiguration).
	if cfg.NATSURL != "" {
		pub, closePub, err := buildCashPublisher(ctx, cfg, mesh, busMetrics)
		if err != nil {
			logger.Error("cash publisher init failed", "err", err)
			return 2
		}
		defer closePub()
		opts = append(opts, server.WithCashPublisher(pub))
	}

	readiness := &server.Readiness{}
	// THE SAME TENANT THE RLS POOL IS PINNED TO (#415). Every /v1 route gates on
	// it: this instance serves exactly one tenant, so a caller from another has no
	// business here whatever the database holds. Omitting it does not open the
	// surface — RequireCallerTenantIs fails closed on an unset instance tenant —
	// but it does take the service offline, which is the correct direction for a
	// misconfiguration on the book of record.
	opts = append(opts, server.WithTenant(cfg.Tenant))
	// TWO LISTENERS, AND THE SPLIT IS THE POINT (#447).
	//
	// The API port carries the routes that MOVE MONEY — a subscription or
	// redemption posted to the IBOR, a NAV, a break list — and decides whose
	// tenant they belong to from the principal header the api-gateway injects. A
	// NetworkPolicy admits only the gateway to it. That policy is worth something
	// only while nothing ELSE forces the port open, and /metrics on the same mux
	// does exactly that: allow-observability-scrape must admit whatever port
	// serves it, so a pod in kanz-observability could send a self-chosen tenant
	// header to a cash-movement write.
	//
	// #232 named the fix as a code change in each service and optimization made
	// it first, on the argument that "a trading surface is the one place this
	// platform cannot afford to inherit that gap". A capital MOVEMENT is that
	// argument at least as strongly.
	//
	// The API is on the HIGH port and /metrics keeps the scraped one, the same way
	// round as optimization. 8080 is shared by six services that legitimately
	// scrape there, so moving /metrics off it would need the rule to admit a
	// seventh port for no gain — it is the API that has to leave.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", obs.MetricsHandler())
	metricsSrv := httpserver.New(cfg.MetricsListen, metricsMux, httpserver.Standard())
	go func() {
		logger.Info("accounting metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Telemetry is not the book of record: a dead metrics listener must not
			// take the service down, but it must not be silent either — a scrape
			// target that vanishes reads as a healthy service nobody is watching.
			logger.Error("metrics server failed — this pod is now unmonitored", "err", err)
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutCtx)
	}()

	// PROBES STAY ON THE API LISTENER, as optimization's do. kubelet reaches a pod
	// from the node rather than from a namespace, so a probe is not subject to the
	// NetworkPolicy that keeps everything else off this port.
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, store, cfg.BaseCurrency, opts...), httpserver.Standard())
	go func() {
		logger.Info("accounting listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	// Fatal runtime errors surface through fatal, raised only by the fill
	// consumer below (the fatal path) and read after consumers.Wait() joins in
	// awaitConsumers — the join is what makes the write happen-before the read.

	// Fill-folding consumer (WIRE-01b): fold live order.v1 fill FACTs into the
	// same journal the server reads, so NAV/positions reflect live execution.
	// Runs concurrently with the server; both share ctx so SIGTERM stops them
	// together, and both are JOINED before this function returns (see the
	// shutdown block below — the WaitGroup is not decoration). Empty
	// ACCOUNTING_NATS_URL ⇒ read/reconcile only (default).
	var consumers sync.WaitGroup
	if cfg.NATSURL != "" {
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			if err := runConsumer(ctx, cfg, custodyCfg, store, ledgerPool, custodyStore, mesh, logger, obs, busMetrics); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("fill consumer stopped with error", "err", err)
				fatal.Raise(err)
			}
		}()
		// Live FX feed (WIRE-01d): fold market.v1 FX quotes into the rate cache.
		// Auxiliary to fill folding — a subscription failure degrades multi-
		// currency NAV (a stale/missing rate fails that valuation loudly) rather
		// than bringing the service down, so it does not stop() on error.
		if liveFX != nil {
			consumers.Add(1)
			go func() {
				defer consumers.Done()
				if err := runFXFeed(ctx, cfg, liveFX, mesh, logger, obs, busMetrics); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("FX feed stopped with error", "err", err)
				}
			}()
		}
	} else {
		logger.Info("no ACCOUNTING_NATS_URL set — serving read/reconcile only (no fill folding)")
	}

	// Ledger checkpoint job (#229). Without it, materializing a book reads the
	// portfolio's ENTIRE lifetime journal on every NAV and reconcile request:
	// ledger_snapshots stays empty, MaterializeCurrent takes its no-checkpoint
	// fallback forever, and the service degrades with AGE rather than with load.
	// It joins the same WaitGroup as the bus consumers so SIGTERM stops it with
	// them; a pass in flight holds one pool connection and closeStore must not
	// run underneath it.
	//
	// Durable store only: the in-memory journal dies with the pod, so
	// checkpointing it buys nothing. Both branches SAY which one they took —
	// "nothing configured" and "checked, and fine" must not look the same, and a
	// checkpoint job silently not running is the defect this fixes.
	switch {
	case cfg.DatabaseURL == "":
		logger.Info("in-memory journal — no ledger snapshotter (reads fold the whole in-process journal)")
	case cfg.SnapshotInterval <= 0:
		logger.Warn("ACCOUNTING_SNAPSHOT_INTERVAL is not positive — ledger checkpointing is DISABLED; "+
			"every NAV and reconcile request will scan the portfolio's entire journal (#229)",
			"interval", cfg.SnapshotInterval)
	default:
		snapshotter, err := ledger.NewSnapshotter(store, logger, ledger.SnapshotterConfig{
			Interval: cfg.SnapshotInterval,
			Batch:    cfg.SnapshotBatch,
			Metrics:  snapMetrics,
		})
		if err != nil {
			logger.Error("ledger snapshotter init failed", "err", err)
			return 2
		}
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			if err := snapshotter.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("ledger snapshotter stopped with error", "err", err)
			}
		}()
	}

	readiness.Set(true)

	<-ctx.Done()
	readiness.Set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	awaitConsumers(&consumers, logger)

	return fatal.Code()
}

// awaitConsumers blocks until the bus consumer goroutines have finished
// draining. It is the LAST statement in main for a reason: everything that
// tears this process down — closePub, closeStore (pool.Close), mesh.Close,
// obs.Shutdown — is deferred, so it runs after main's body and cannot be
// reordered ahead of this wait.
//
// #233: without it, main returned from <-ctx.Done(), ran a near-instant
// httpSrv.Shutdown, and fired those defers while the consumer was still
// folding. pkg/bus does not STOP a cancelled consumer, it DRAINS one —
// Subscribe deliberately spends up to DrainGrace + drainStopSlack (5s + 2s,
// pkg/bus/nats.go) after ctx.Done() handing buffered messages to the handler,
// because Stop() would abandon them and reorder the replayed log on every
// rolling deploy. Those handler calls landed on a closed pool, ledger Append
// failed, and bus.WithDLQ republished each fill to dlq.order.order.filled and
// ACKED it. tools/replay refuses dlq. subjects, so the fills were gone from the
// book of record with no supplied way back — on every deploy.
//
// THE BOUND IS A context.WithTimeout(context.Background(), N*time.Second) ON
// PURPOSE, not a bare time.After: that is the exact spelling
// test/arch/pod_disruption_and_drain_test.go's worstServiceShutdownSeconds
// greps for to derive every manifest's terminationGracePeriodSeconds from the
// source. Rewriting it as a timer would drop 10s out of the derived drain
// budget while the process still spends it, and nothing would say so.
//
// 10s against a 7s bus drain: enough headroom for the drain plus the consumer's
// own teardown, short enough to stay inside the grace period the manifests set.
// That grace period is NOT quoted here — the version of this line that quoted
// 45s was already wrong (#264), and the arch guard is where it is computed. When
// the budget expires we close anyway and say so loudly: a handler that ignores
// context cancellation must not hold the pod until the kubelet SIGKILLs it.
//
// Copied, not shared: services/risk-engine/internal/app/lifecycle.go is the
// reference implementation and the right home for this once promoted — the day
// it moves, TestNoUndeclaredShutdownBudgetReachableFromAMain keeps it counted.
func awaitConsumers(consumers *sync.WaitGroup, logger *slog.Logger) {
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer joinCancel()

	drained := make(chan struct{})
	go func() {
		consumers.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		logger.Info("bus consumers drained")
	case <-joinCtx.Done():
		logger.Error("bus consumers did not finish draining within the join budget — closing the " +
			"journal pool and bus clients anyway; any event still in a handler will fail its " +
			"Append and be routed to dlq.<subject>")
	}
}

// openStore selects the durable Postgres journal when a DSN is set (PARITY-02a),
// and otherwise REFUSES TO START unless the deployment has said out loud that it
// accepts an ephemeral book of record. Returns a close func that tears down the
// pool (a no-op for the in-memory store). Both satisfy ledger.Store, so the
// server and the fill consumer share one journal regardless of backend.
//
// This branch used to return ledger.NewMemoryStore() with no log, no gauge and
// no error (#261). THIS STORE IS THE FUND'S BOOK OF RECORD. The process started
// clean, /readyz answered 200, NAV materialized from a journal that looked
// right, and every entry in it was discarded by the next rolling update — with
// nothing in the deployment that forgot its DSN to distinguish it from one that
// was configured correctly.
//
// WHY THIS ONE REFUSES WHERE market-data WARNS. The distinction is whether the
// store holds the only copy. market-data's price history is a fold of a feed
// that keeps publishing and is archived to Kafka besides, so an in-memory pod
// loses history and refills; this journal is fed by a durable consumer group
// that resumes at its last ack and NEVER REPLAYS, off an ACCOUNTING stream that
// ages out at 168h. A restart does not come back short, it comes back EMPTY, and
// no amount of running time repairs it. That is tv-sync's argument
// (services/tv-sync/internal/config/config.go validateBook) applied to the
// ledger: an ephemeral book is not a degraded mode, it is a lie with a delay
// on it.
//
// And no replica count rescues it. infra/deploy/accounting-deploy.yaml runs
// replicas: 2, which is safe ONLY because ledger.Postgres.Append is idempotent
// at the engine — ON CONFLICT (tenant_id, entry_id) DO NOTHING — so a fill folds
// once whichever pod receives it. Two maps have no such conflict to detect:
// each pod folds the subset of fills it happened to get, and a NAV query is
// answered by whichever pod the Service picked. There is no deployment shape
// that makes this acceptable, so it is an opt-in rather than a default — the
// same call audit's WORM log and webhook-ingest's nonce store make.
//
// By the time openStore runs, config.Load has already refused to start if the
// _FILE mount was DECLARED but unreadable (secret.Read), so an empty DSN here
// can only mean no DSN was ever configured — precisely the case that must not be
// silent. Every shipped manifest mounts one, so this refusal does not change the
// deployed posture; it changes what happens to the deployment that forgot.
// It returns the POOL alongside the store because the custody reconciliation
// plane (#962) needs a durable home for the break lifecycle on the same
// tenant-scoped connection. nil means the in-memory book, and the custody plane
// reports that at ERROR rather than inheriting it silently.
func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (ledger.Store, *pgxpool.Pool, func(), error) {
	if cfg.DatabaseURL == "" {
		if !cfg.AllowEphemeralLedger {
			return nil, nil, nil, errors.New("no ACCOUNTING_DATABASE_URL (or _FILE mount): the IBOR journal " +
				"would be IN-MEMORY and the fund's BOOK OF RECORD would be DISCARDED on the next restart, " +
				"rollout or eviction — every fill, cash movement and FX revaluation with it, and the fold " +
				"cannot be rebuilt because the consumer group resumes at its last ack and the ACCOUNTING " +
				"stream ages out at 168h. Set ACCOUNTING_DATABASE_URL, or set " +
				"ACCOUNTING_ALLOW_EPHEMERAL_LEDGER=true to accept a ledger that does not survive a restart " +
				"— in which case this deployment is NOT a book of record and MUST run exactly one replica")
		}
		logger.Warn("IBOR JOURNAL IS IN-MEMORY — ACCOUNTING_ALLOW_EPHEMERAL_LEDGER accepted an ephemeral "+
			"book of record. Every entry is DISCARDED on the next restart, rollout or eviction, and nothing "+
			"downstream reports the loss. This deployment MUST run exactly ONE replica: two pods would each "+
			"fold only the fills they received and answer NAV from a different partial journal",
			"fix", "set ACCOUNTING_DATABASE_URL (or its _FILE mount); the shipped manifest runs replicas: 2",
			"gauge", "kanz_accounting_ledger_durable=0")
		ledgerDurable.Set(0)
		return ledger.NewMemoryStore(), nil, func() {}, nil
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
		return nil, nil, nil, err
	}
	ledgerDurable.Set(1)
	return ledger.NewPostgres(pool), pool, pool.Close, nil
}

// runConsumer folds live order.v1 fill FACTs into store until ctx is canceled.
// It mirrors the market-data / risk-engine ingest pattern: one bus.Consumer,
// each fill subject subscribed on its own goroutine (Subscribe blocks), fail-
// fast — the first non-cancellation error cancels the siblings and is returned,
// so a broken subscription brings folding down rather than running silently
// degraded (book-of-record data loss must be loud). Idempotency is handled below
// this layer (consumer dedup + ledger.Store.Append on the entry id).
func runConsumer(ctx context.Context, cfg config.Config, custodyCfg custodyConfig, store ledger.Store, ledgerPool *pgxpool.Pool, custodyStore custody.Store, mesh *transport.Mesh, logger *slog.Logger, obs *observability.Provider, busMetrics *bus.BusMetrics) error {
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client, Metrics: busMetrics})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// THE BOOK OF RECORD ANNOUNCES WHAT A PORTFOLIO CAN SPEND (#450).
	//
	// The pre-trade buying-power gate (#415) and exchange balance reconciliation
	// (#418) both need a cash balance and NEITHER MAY COMPUTE ONE: this ledger is
	// the only component that folds trade legs, cash movements and corporate
	// actions bitemporally, with restatements. Two independent computations of one
	// number drift, and the drift surfaces as a trading control that refuses or
	// admits wrongly.
	//
	// CORPORATE ACTIONS ARE NOT IN THIS BALANCE (#588), and both consumers of it
	// are entitled to know: the ruling above is about AUTHORITY, not coverage.
	// The fold is written and correct; nothing announces an action, so no
	// EntryCorporateAction has ever reached this journal. Every announcement this
	// producer sends is therefore complete for fills and cash movements and short
	// by every dividend, coupon and merger payment received.
	// stateEntrySourcePosture above is what says so on /metrics.
	//
	// It rides the CONSUMER'S connection rather than dialing a second one: an
	// announcement is caused by a fold, so "can fold, cannot announce" is a state
	// worth not inventing.
	announceProducer, err := bus.NewProducer(client, cashProducerConfig(cfg))
	if err != nil {
		return err
	}
	// AND EVERY ANNOUNCEMENT CARRIES THE POSTURE (#614). The gauge above is what an
	// operator reads; this is what the pre-trade gate reads, and they are the same
	// classification (classifyEntrySources) so they cannot disagree. Without it the
	// consumer has to infer completeness from silence — and a balance short by
	// every dividend looks exactly like a whole one.
	announcer := consume.NewAnnouncer(store, announceProducer, cfg.BaseCurrency,
		entrySourceCompleteness(cfg), logger, nil)
	logger.Info("accounting: cash-balance announcements armed — downstream can see what each "+
		"portfolio may spend", "subject", consume.SubjectPortfolioCash)

	// Registered before anything can drop: a plain Counter exports at zero the
	// moment it is registered, which is what makes "none lost" a readable answer
	// rather than a missing series (#622).
	obs.Registry.MustRegister(announcementsLost)

	folder, err := consume.NewFolder(cfg.Tenant, store, cfg.BaseCurrency,
		consume.WithAnnouncer(announcer), consume.WithLogger(logger),
		consume.WithAnnounceFailureObserver(func() { announcementsLost.Inc() }))
	if err != nil {
		return err
	}

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

	// CUSTODY RECONCILIATION (#962) — the control around the comparison engine.
	//
	// The engine in internal/recon has been correct since IBOR-01e and its only
	// caller was an HTTP handler taking the custodian's statement from the REQUEST
	// BODY. So the platform had a reconciliation engine and no reconciliation
	// control: nothing ingested a statement, nothing ran on a cadence, nothing
	// recorded that a run happened, and a break had no lifecycle once found.
	//
	// It rides the consumer's connection for the same reason the cash announcer
	// does: a run is caused by folded state, and "can fold, cannot record the
	// reconciliation" is a state worth not inventing.
	custodyProducer, err := bus.NewProducer(client, cashProducerConfig(cfg))
	if err != nil {
		return err
	}
	plane, err := buildCustodyPlane(cfg, custodyCfg, ledgerPool, custodyStore, store, custodyProducer, obs.Registry, logger)
	if err != nil {
		return err
	}
	if cfg.CustodyStatementSubject != "" {
		subscribe(cfg.CustodyStatementSubject, plane.consumer.Handle)
	}
	if plane.scheduler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := plane.scheduler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}()
	}

	// THE OUTBOX RELAY, AND IT IS THE RECOVERY #804 ADDED.
	//
	// The cash announcement now commits with the journal entry that changed it,
	// and the fold flushes it inline so a healthy deployment publishes exactly as
	// promptly as the direct publish it replaces. This goroutine is what happens
	// when that flush fails: before it, the ONLY thing that re-announced a
	// portfolio was the next fold for that portfolio, so one broker blip refused
	// every order for it under a buying-power mandate until unrelated activity
	// happened to arrive.
	//
	// Started only when there is an announcer to publish through — NewFolder
	// builds no relay without one, and a nil relay here is the no-announcer
	// deployment the startup log already reports.
	if relay := folder.Outbox(); relay != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("accounting outbox relay armed — cash levels are published from the store, " +
				"in the same transaction that committed the entry that changed them")
			if err := relay.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// cashProducerConfig is the producer identity the cash-movement publisher runs
// under. It is split out of buildCashPublisher, which cannot be unit-tested
// because it dials a broker first (#245).
//
// TENANT IS NOT OPTIONAL HERE, AND ITS ABSENCE IS NOT A DEGRADED MODE (MT-01b).
// A cash movement is raised by an HTTP request — POST /portfolios/{id}/cash —
// not by an inbound bus delivery, so there is no ctx tenant for the producer to
// inherit and this fallback is the ONLY source of one. Configured without it,
// bus.Validate refused EVERY subscription, redemption and fee with "tenant_id
// required": the endpoint answered 400, the service stayed ready, and the
// ledger's non-trade cash inputs never reached NAV. market-ingest's composition
// root carries the same note for the same reason; the OMS crash-looped on it.
func cashProducerConfig(cfg config.Config) bus.ProducerConfig {
	return bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version.String(),
		Tenant:          cfg.Tenant,
	}
}

// buildCashPublisher dials a producer connection and builds the WIRE-01f
// cash-movement publisher. It returns a close func for the producer client.
func buildCashPublisher(ctx context.Context, cfg config.Config, mesh *transport.Mesh, busMetrics *bus.BusMetrics) (*cashmove.Publisher, func(), error) {
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source + "-producer", TLSConfig: mesh.Client, Metrics: busMetrics})
	if err != nil {
		return nil, nil, err
	}
	producer, err := bus.NewProducer(client, cashProducerConfig(cfg))
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
func runFXFeed(ctx context.Context, cfg config.Config, liveFX *fxfeed.LiveFX, mesh *transport.Mesh, logger *slog.Logger, obs *observability.Provider, busMetrics *bus.BusMetrics) error {
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source + "-fx", TLSConfig: mesh.Client, Metrics: busMetrics})
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

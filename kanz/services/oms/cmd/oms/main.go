// oms binary entrypoint (OMS-01). Consumes the order commands
// (submit/amend/cancel), drives the order aggregate through validation → the
// pre-trade compliance gate → admission, works admitted orders via the EMS, and
// emits the lifecycle FACTs + command outcomes. A position projector folds the
// resulting fills back onto the risk engine's position-changed input. Without
// OMS_NATS_URL it serves HTTP/probes only (no command consumption).
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

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/version"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/compliance"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/eighred/kanz/services/oms/internal/outbox"
	"github.com/eighred/kanz/services/oms/internal/position"
	"github.com/eighred/kanz/services/oms/internal/server"
)

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

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Two independent things can turn fatal after startup has completed: the
	// probes/metrics server dying, and runConsumers() surfacing a
	// post-subscription error (see its own doc comment for the started/!started
	// split). lifecycle.Fatal records both through one seam without an os.Exit
	// of its own (rule 6, #266); stop() cancelling ctx is what makes the joins
	// below (runConsumers returning, or <-ctx.Done()) the point where the write
	// is guaranteed to happen-before the read. startupFailed is a separate,
	// plain bool: it answers "which exit code", not "was there a fatal at all",
	// and only runConsumers's own return value can tell those apart.
	fatal := lifecycle.NewFatal(stop)
	var startupFailed bool

	go func() {
		logger.Info("oms listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	if cfg.NATSURL != "" {
		started, err := runConsumers(ctx, cfg, readiness, logger, obs)
		if err != nil {
			logger.Error("oms consumers stopped with error", "err", err)
			fatal.Raise(err)
			startupFailed = !started
		}
	} else {
		readiness.Set(true)
		logger.Warn("no OMS_NATS_URL set — serving HTTP/probes only (no command consumption)")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	// oms is the only composition root whose startup wiring happens inside the
	// same helper that then runs the consumers, so "never became ready" and
	// "died after running" are only distinguishable from that helper's return
	// value — lifecycle.Fatal has no notion of that distinction, which is why
	// this service overrides fatal.Code() here instead of calling it directly.
	if fatal.Err() != nil {
		if startupFailed {
			return 2
		}
		return 1
	}
	return 0
}

// runConsumers wires the bus producer + consumers and subscribes the command
// and fill subjects. Each subscription runs in its own goroutine; the first
// non-cancel error fails the group.
//
// Returns (started, err): started reports whether the subscription goroutines
// were launched before err occurred. Everything up to that point is startup
// wiring — a dial, a store, a bad OMS_VENUE_ACCOUNTS — and the caller maps
// !started to exit 2; started (a failure surfacing after the group is
// running, i.e. firstErr below) maps to exit 1 (#266).
func runConsumers(ctx context.Context, cfg config.Config, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) (bool, error) {
	busMetrics := bus.NewBusMetrics(obs.Registry)

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return false, err
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		return false, err
	}
	defer func() { _ = client.Close() }()

	// Tenant is the LAST-RESORT fallback for a publish that has no tenant at all,
	// and it is what every other producer in this estate already sets (risk-engine,
	// market-ingest, archiver, venue-binance, venue-okx). The OMS was the only one
	// without it, and that omission is why a tenantless publish was reachable here
	// at all: the startup sweep published outside any delivery, envelope validation
	// rejected it, and because a sweep failure is fatal the OMS crash-looped on
	// exactly the orders the sweep exists to rescue (fixed in dd4822c).
	//
	// WHAT THIS DOES NOT DO: it does not override per-delivery tenancy. Precedence
	// in Producer.stamp is Event.TenantID > ctx > this, and the Consumer stashes
	// every inbound envelope's tenant on the ctx — so a FACT derived from a
	// fund-alpha order still publishes as fund-alpha. This only catches the paths
	// with no inbound envelope to inherit from, which is precisely the class that
	// was crashing the service. cfg.Tenant is the owning tenant of THIS deployment
	// (see config.Tenant), already pinned as app.tenant_id on every DB connection.
	//
	// It is a safety net, not a licence to stop being explicit: SweepInterrupted
	// still demands an explicit tenant and refuses without one, because an order it
	// re-drives may belong to a tenant this deployment should name deliberately
	// rather than default.
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version.String(),
		Tenant:          cfg.Tenant,
		Metrics:         busMetrics,
	})
	if err != nil {
		return false, err
	}

	// The two durable stores, over ONE pool: the order store (the admission gate,
	// EXEC-M7c) and the POSITION BOOK (EXEC-M18). Both are safety choices, not
	// persistence ones, and both stop being safe the moment there is a second pod.
	store, book, closeStores, err := openStores(ctx, cfg, logger)
	if err != nil {
		return false, err
	}
	defer closeStores()

	// OMS-01e: fill→position projector over the shared book.
	projector, err := position.NewProjector(book, producer, cfg.Tenant)
	if err != nil {
		return false, err
	}

	// OMS-01f + COMP-01c: the pre-trade gate is the COMP-01 engine resolving
	// against the mandate stream (a MandateConsumer feeds the registry from the
	// shared ConfigChanged subject) and projecting onto the live position book.
	mandateReg := comp.NewMandateRegistry(comp.WithMandateLogger(logger))
	mandateConsumer := comp.NewMandateConsumer(mandateReg, logger)

	// COMP-M2: the reference-mark source the pre-trade gate values MARKET/STOP
	// orders from. It starts EMPTY and warms as the spine delivers, so a freshly
	// started pod refuses market orders until the first tick for that instrument
	// arrives. That is the safe direction and it is deliberate — the alternative
	// is admitting an order at a price we do not have.
	marks := mark.New(time.Now, cfg.PriceMaxAge)

	// AN UNGOVERNED PORTFOLIO IS NOW COUNTED AND ANNOUNCED (EXEC-M14).
	//
	// It used to be silent: an order for a portfolio nobody had put under mandate
	// returned Allowed:true with no log and no metric, so "we forgot to mandate fund
	// X" and "fund X passed compliance" were the same observable event. This counter
	// is what makes "how much of the book is ungoverned" a number somebody can look
	// at, rather than a question nobody has asked.
	ungoverned := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_compliance_ungoverned_orders_total",
		Help: "Orders admitted or refused for a portfolio that NO MANDATE GOVERNS. " +
			"Non-zero means part of the book is trading with no compliance constraints (or, with " +
			"OMS_REQUIRE_MANDATE, is being refused for want of one).",
	})
	obs.Registry.MustRegister(ungoverned)

	// AN UNPRICED REFUSAL IS NOW COUNTED TOO (COMP-M2).
	//
	// Ungoverned already had a counter; Unpriced and Unvaluable — the other
	// "nothing was evaluated" refusals — had only a once-per-(portfolio,
	// instrument) warn log. That log fires once for the LIFE OF THE PROCESS, so a
	// sustained pricing outage goes invisible after the first refused order. The
	// two causes are split by label because they are different incidents with
	// different responses: "never_seen" is a cold pod, a thin instrument, or a
	// subscription delivering nothing (a warm-up); "expired" is a feed that WAS
	// reporting and has stalled (an outage).
	unpriced := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_compliance_unpriced_orders_total",
		Help: "Orders refused PRICE_UNAVAILABLE because no usable reference mark exists for the instrument. " +
			"Sustained non-zero means the market-data spine is not reaching this OMS for that instrument.",
	}, []string{"reason"})
	obs.Registry.MustRegister(unpriced)

	// HOW MANY INSTRUMENTS IS THE FOLD ACTUALLY HOLDING? (#96)
	//
	// The price spine is a broadcast, so every replica folds every trade and
	// quote the estate publishes — deliberately, because a consumer group made
	// admission depend on which pod received the tick (see the subscription
	// below). The open question was whether that volume needs bounding by an
	// instrument allowlist, and it could not be answered: nothing reported the
	// fold's size, so the choice sat between accepting unmeasured growth and
	// adding config whose omission would refuse live orders.
	//
	// GaugeFuncs rather than a counter, because this is a level and not an event,
	// and they read through mark.Source.Stats so the metric cannot drift from the
	// map it describes.
	//
	// held − live is the tombstone population: instruments seen once whose marks
	// have expired and whose prices have been released. A held that climbs while
	// live stays flat is an estate publishing instruments this OMS never trades —
	// which is the measurement that would justify an allowlist.
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_mark_instruments_held",
		Help: "Instruments held by the reference-mark fold, including expired tombstones. " +
			"Growth here is the price spine's instrument cardinality, not this OMS's trading universe.",
	}, func() float64 { held, _ := marks.Stats(); return float64(held) }))
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_mark_instruments_live",
		Help: "Instruments whose reference mark is present and within OMS_PRICE_MAX_AGE — the marks the " +
			"pre-trade gate can actually value an order from. A fall here with held flat is a stalling feed.",
	}, func() float64 { _, live := marks.Stats(); return float64(live) }))

	preTrade := comp.NewPreTradeGate(
		comp.NewEngine(nil), compliance.NewBookSource(book), mandateReg, nil, nil, logger,
		comp.WithRequireMandate(cfg.RequireMandate),
		comp.WithUngovernedObserver(func(string, string) { ungoverned.Inc() }),
		comp.WithUnpricedObserver(func(portfolioID, instrumentID string) {
			// Two very different incidents arrive at the same refusal, and an
			// operator needs to tell them apart: a mark we have NEVER seen means a
			// cold pod, a thin instrument, or a subscription delivering nothing;
			// a mark we HAVE seen but which expired means the feed was working and
			// stalled. One is a warm-up, the other is an outage.
			if _, asOf, seen := marks.Lookup(instrumentID); seen {
				unpriced.WithLabelValues("expired").Inc()
				logger.Warn("order refused: the reference mark is STALE — the price feed has stopped reporting for this instrument",
					"portfolio", portfolioID, "instrument", instrumentID,
					"mark_as_of", asOf, "max_age", cfg.PriceMaxAge)
				return
			}
			unpriced.WithLabelValues("never_seen").Inc()
			logger.Warn("order refused: NO reference mark has ever been seen for this instrument — a cold pod warming up, an instrument nothing quotes, or a price subscription delivering nothing",
				"portfolio", portfolioID, "instrument", instrumentID, "subjects", cfg.PriceSubjects)
		}),
	)
	gate := compliance.NewCOMP01Gate(preTrade, cfg.BaseCurrency,
		compliance.WithMarkSource(marks),
	)

	// State the posture, loudly, at startup. Which of these two lines is in the log is
	// the difference between "an unmandated portfolio trades unconstrained" and "an
	// unmandated portfolio cannot trade at all", and nobody should have to read the
	// config to find out which one they deployed.
	if cfg.RequireMandate {
		logger.Info("pre-trade compliance: MANDATE REQUIRED — an order for a portfolio with no mandate is REJECTED (MANDATE_MISSING)")
	} else {
		logger.Warn("pre-trade compliance: MANDATE ADVISORY — an order for a portfolio with NO MANDATE is ADMITTED, unconstrained. " +
			"Put every live portfolio under mandate with kanz-mandate, or set OMS_REQUIRE_MANDATE=true to refuse instead")
	}

	// EXEC-M16 — WHOSE COLLATERAL DOES AN ORDER SPEND?
	//
	// An exchange margins, nets and LIQUIDATES per ACCOUNT. Two portfolios settling
	// into one exchange account share one collateral pool, so a drawdown in the first
	// consumes the second's margin — while a per-portfolio ledger still shows that
	// cash sitting there. Segregation is therefore a property of the ACCOUNT, and this
	// is where the platform learns which account each portfolio may spend from.
	//
	// A BAD BINDING IS FATAL. If one account is bound to two portfolios, the platform
	// would report segregated books over a shared pool — the exact failure this exists
	// to prevent. That is not a config typo to warn about and carry on from; the OMS
	// refuses to start.
	bindings, err := execution.ParseBindings(cfg.VenueAccounts)
	if err != nil {
		logger.Error("OMS_VENUE_ACCOUNTS is not safe to trade on", "err", err)
		return false, err
	}
	// Orders that margin against an account nobody bound to their portfolio. Non-zero
	// means some part of the book is sharing collateral with the rest of it.
	sharedCollateral := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_shared_collateral_orders_total",
		Help: "Orders executed against an exchange account NOT bound to their portfolio — i.e. against " +
			"a collateral pool shared with every other unbound portfolio at that venue. An exchange " +
			"liquidates per account, so this is the number of orders whose segregation is nominal only.",
	})
	obs.Registry.MustRegister(sharedCollateral)

	quarantined := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_orders_quarantined_total",
		Help: "Orders frozen because the platform could not establish what the venue did with them — " +
			"the venue denied an order it had acknowledged, could not be queried at all, or reported " +
			"a fill the order cannot accept. Each one is a position whose true size nobody knows, and " +
			"it will not be re-driven, cancelled or mentioned again until a human resolves it. " +
			"This should be zero; any non-zero value is an incident, not a metric to trend.",
	})
	obs.Registry.MustRegister(quarantined)

	// Cancels and amends that gave up waiting for the goroutine working their
	// order. Non-zero means an operator instruction went to the DLQ instead of
	// the order it was meant to act on, while the order itself is still live at
	// an exchange — and the usual cause is a venue call that is not returning.
	claimTimeouts := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_claim_timeouts_total",
		Help: "Cancels and amends abandoned because the goroutine working the order would not release it " +
			"in time. Each one is an operator instruction parked in the DLQ rather than applied, for an " +
			"order that is still working at a venue. Non-zero means a venue call is hanging and somebody's " +
			"withdrawal did not land; it should be zero.",
	})
	obs.Registry.MustRegister(claimTimeouts)

	// Orders the store committed at admission whose ORDER_ACCEPTED FACT never
	// reached the bus, and which a compensator had to announce afterwards (#238).
	// Every increment is a period during which risk, compliance and the audit log
	// were all short one order and nothing was in an error state.
	acceptedReannounced := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_orders_accepted_reannounced_total",
		Help: "Orders whose ORDER_ACCEPTED FACT was committed to the store but never published, and which " +
			"a compensator re-announced later: a durable order no downstream service had heard of, so risk " +
			"carried no exposure for it and the projection dropped every later FACT about it. Since #292 " +
			"admission commits that FACT in the SAME transaction as the order, so the only rows that can " +
			"reach the compensator are ones admitted before migration 0006. Non-zero therefore means either " +
			"a pre-outbox row was repaired late, or admission has stopped committing the record " +
			"transactionally; it should be zero.",
	})
	obs.Registry.MustRegister(acceptedReannounced)

	// Periodic sweeps that ended in an error. Unlike the startup sweep, a failure
	// here CANNOT be fatal — killing a pod that is currently working live orders
	// is worse than the unreconciled order it would be reacting to — so this
	// counter and the ERROR log beside it are the only way the failure is visible.
	// A sweep that has been failing every tick means the compensator is not
	// compensating, which is indistinguishable from no defect at all if nothing
	// counts it.
	sweepFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_sweep_failures_total",
		Help: "Periodic in-flight reconciliation passes that ended in an error. The pod keeps trading — a " +
			"failed sweep is not a reason to kill a process working live orders — so nothing else surfaces " +
			"this. Sustained non-zero means orders left mid-flight are NOT being recovered and the only " +
			"remaining compensator is the next restart.",
	})
	obs.Registry.MustRegister(sweepFailures)

	// Venue adapters trading an account NOBODY has proved against the exchange
	// (SOV-02a). The adapter's account is read from its own config, so a mis-declared
	// deployment looks exactly like a correct one — non-zero means some part of the
	// book is settling against a collateral pool that only a human's typing says it
	// belongs to.
	unverifiedAccounts := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_unverified_venue_account_total",
		Help: "Venue adapters registered whose exchange account was NOT confirmed by the exchange itself. " +
			"The adapter holds the API credential but has not proved which account it belongs to, so its fills " +
			"could margin against a different fund's collateral than the ledger books them to.",
	})
	obs.Registry.MustRegister(unverifiedAccounts)

	if bindings.Empty() {
		logger.Warn("COLLATERAL IS SHARED — no venue-account bindings configured (OMS_VENUE_ACCOUNTS). Every portfolio trades whatever account its venue adapter holds, so they all margin against ONE pool per venue: a liquidation caused by one portfolio consumes the margin of all of them, and each ledger still reports its own cash intact")
	} else if cfg.RequireVenueAccount {
		logger.Info("venue accounts: BINDING REQUIRED — an order for a portfolio bound to no account at its venue is REJECTED (VENUE_ACCOUNT_UNBOUND)",
			"bindings", bindings.Len(), "accounts", bindings.Accounts())
	} else {
		logger.Warn("venue accounts: BINDING ADVISORY — a portfolio with no binding still trades, against a SHARED account. Set OMS_REQUIRE_VENUE_ACCOUNT=true to refuse instead",
			"bindings", bindings.Len(), "accounts", bindings.Accounts())
	}

	// THE OUTBOX RELAY'S INSTRUMENTATION (#292). The relay itself is built by
	// order.NewService, from the store's own queue and the emitter's own bus, so
	// that a composition holding a Service always holds a drain — there is no
	// wiring step here that could be omitted. What this composition root still
	// owns is running it (below) and measuring it.
	//
	// There is no switch to turn the relay off, deliberately. A disabled relay
	// is an OMS that admits orders, commits their ACCEPTED FACTs to a table and
	// tells nobody — the exact invisible-order failure #238 was filed for, made
	// permanent. "Nothing configured" and "checked, and fine" must not look the
	// same, and the cheapest way to guarantee that is for the off position not
	// to exist.
	outboxPublished := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_outbox_published_total",
		Help: "Order FACTs published from the transactional outbox. Every lifecycle FACT the OMS commits " +
			"transactionally is counted here exactly once per successful publish; a relay that has stopped " +
			"shows as a flat line while orders keep being admitted.",
	})
	obs.Registry.MustRegister(outboxPublished)

	outboxFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_outbox_publish_failures_total",
		Help: "Attempts to publish an outbox record that did not reach the broker. The record stays at the " +
			"head of its order's queue and every FACT behind it is held back — publishing past it would hand " +
			"consumers that order's history out of sequence. Nothing else surfaces this: the command that " +
			"enqueued the record was acked successfully long before.",
	})
	obs.Registry.MustRegister(outboxFailures)

	// OMS-01b/c: order command handler over the order store + a sim venue.
	emitter := order.NewEmitter(producer)
	// Venue set is composition-root-selected: SimVenue by default; Binance Spot +
	// its user-data/reconciliation/ticker workers under -tags binance
	// (configuredVenues is build-tag split, wired to the shared order store).
	venues, closeVenues, err := configuredVenues(ctx, cfg, store, producer, unverifiedAccounts, logger)
	if err != nil {
		return false, err
	}
	defer closeVenues()
	router := execution.NewRouter(venues...)
	svc, err := order.NewService(cfg.Tenant, store, emitter, gate, router, closeRegistry, logger,
		order.WithAccountBindings(bindings, cfg.RequireVenueAccount, sharedCollateral),
		order.WithQuarantineCounter(quarantined),
		order.WithClaimTimeoutCounter(claimTimeouts),
		order.WithAcceptedReannounceCounter(acceptedReannounced),
		order.WithOutboxRelay(
			outbox.WithInterval(cfg.OutboxInterval),
			outbox.WithCounters(outboxPublished, outboxFailures)))
	if err != nil {
		return false, err
	}
	relay := svc.Outbox()

	// THE AGE OF THE OLDEST UNPUBLISHED RECORD IS THE ALERTABLE SIGNAL, and it is
	// a GaugeFunc because it must be true even when nothing is happening. An
	// empty outbox and a relay that died both produce zero errors, zero failed
	// publishes and a silent log; the only number that separates them is how long
	// the front of the queue has been waiting.
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_outbox_oldest_pending_seconds",
		Help: "Age of the oldest order FACT committed to the outbox and not yet published. Zero means the " +
			"outbox is drained. A value that keeps climbing means FACTs the estate depends on are sitting in " +
			"Postgres: risk, compliance and the audit log are behind by that much.",
	}, func() float64 { return relay.OldestPendingAge(ctx) }))

	// WithDLQ is load-bearing on this path, not hygiene. Without it a SubmitOrder
	// whose venue call fails AFTER admission returns an error with nowhere to go:
	// the broker redelivers it, handleSubmit's fast path finds the order it
	// already created and acks it as a duplicate, and the order is left at ROUTED
	// looking exactly like a limit order resting normally — with nothing working
	// it and nothing anywhere saying so. Parking it in dlq.<subject> is what makes
	// that failure a thing an operator can see. Retry is deliberately NOT wired
	// here; see test/arch/bus_dlq_test.go for why it would make this worse.
	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))
	if err != nil {
		return false, err
	}

	type sub struct {
		subject string
		handler bus.EventHandler
	}
	var subs []sub
	for _, s := range cfg.CommandSubjects() {
		subs = append(subs, sub{s, svc.Handle})
	}
	for _, s := range cfg.FillSubjects() {
		subs = append(subs, sub{s, projector.Handle})
	}
	// THE PRICE SPINE — BROADCAST, NOT A WORK QUEUE, for the same reason as the
	// mandate registry below.
	//
	// This was a durable consumer GROUP, which LOAD-BALANCES: with the shipped
	// replicas: 2 each pod folded only the ticks it happened to receive, so each
	// held a DIFFERENT mark map. The pre-trade gate values MARKET and STOP orders
	// from that map, so the same order was admitted by one pod and refused
	// PRICE_UNAVAILABLE by the other, and for a thin instrument a pod could hold
	// no mark indefinitely. It fails safe, but admission stops being
	// deterministic — and "whether your order is legal depends on which pod got
	// it" is not a property a compliance gate may have. A mark is replicated
	// STATE, not work.
	//
	// DeliverLastPerSubject over the market.> wildcard is a bonus: the server
	// replays the latest tick PER CONCRETE SUBJECT, so a cold pod boots with a
	// mark already folded for every instrument that has ever traded, instead of
	// refusing orders until the next tick arrives on each one.
	//
	// THE COST IS DELIBERATE: every replica now folds every tick, so the market-
	// data work is N× the work-queue arrangement. Deterministic admission on a
	// compliance gate is worth more than the saved CPU. The tick-volume question
	// is tracked separately on the board — do NOT "optimize" this back into a
	// consumer group.
	// THE PRE-TRADE GATE'S MANDATE REGISTRY — BROADCAST, NOT A WORK QUEUE.
	//
	// This fed from a durable CONSUMER GROUP, which resumes at its last ack. So a
	// RESTARTED OMS came back with an EMPTY registry and its pre-trade compliance
	// gate PASSED EVERY ORDER: the control did not fail, it DISARMED — silently, on
	// every rolling update, with the pod reporting ready throughout (EXEC-M13).
	//
	// A broadcast subscription over the per-portfolio mandate subjects replays
	// DeliverLastPerSubject, so this process boots holding the mandate IN FORCE for
	// every portfolio. It is also a broadcast because a mandate must reach EVERY
	// replica — a group would arm one OMS pod and leave the other ungoverned.
	mandateSub := comp.SubjectMandateAll

	// RECONCILE WHAT THE LAST PROCESS LEFT MID-FLIGHT, BEFORE ANYTHING CAN ADD MORE.
	//
	// An order saved as ROUTED whose process died before the fill was folded has
	// NO redelivery coming — its command was acked. Nothing else in this system
	// will ever mention it again, and it is indistinguishable over the API from a
	// limit order resting normally at the exchange. This is the only thing that
	// finds it. It runs before the subscriptions and before readiness, the way
	// tv-sync rebuilds its book before it serves one.
	//
	// A failure here is fatal on purpose. A pod that cannot account for the orders
	// its predecessor was working does not know what the fund holds, and admitting
	// new orders on top of that is not degraded operation — it is trading blind.
	// The sweep publishes lifecycle FACTs for the orders it re-drives, but it
	// runs BEFORE any bus delivery — there is no inbound envelope for it to
	// inherit a tenant from the way a handler does (bus.Consumer stashes that
	// on ctx per-delivery; see pkg/bus/context.go). Stamp the OMS's own tenant
	// here explicitly. This is not a guess: cfg.Tenant is the same tenant the
	// order store's connection pool is pinned to (pg.NewTenantPool below), so
	// ListByStatus above only ever returns this tenant's orders. Scoped to this
	// call only — the outer ctx must stay bare so each subscription's own
	// consumer loop keeps setting its own tenant per delivery.
	// DRAIN THE OUTBOX FIRST — the same completeness rule as the sweep below, one
	// layer earlier (#292). A pod that starts holding records its predecessor
	// committed and died before publishing is holding FACTs the estate is
	// currently missing, and it should say them before it admits anything new.
	//
	// A FAILURE HERE IS NOT FATAL, unlike the sweep. The records are durable and
	// the relay goroutine retries them on its tick; refusing to start would take
	// away the only thing that can publish them. That is the opposite trade to
	// the sweep, whose failure means the pod cannot ACCOUNT for its predecessor's
	// orders at all.
	if drained, derr := relay.DrainOnce(ctx); derr != nil {
		logger.Error("oms: could not drain the outbox at startup — order FACTs committed by the previous "+
			"process are not yet published; the relay will keep retrying them",
			"err", derr, "published_before_failure", drained)
	} else if drained > 0 {
		logger.Warn("oms: published order FACTs the previous process committed and never announced — "+
			"until now no downstream service had heard them",
			"count", drained)
	}

	sweepStart := time.Now()
	swept, err := svc.SweepInterrupted(bus.WithTenantID(ctx, cfg.Tenant))
	if err != nil {
		logger.Error("could not reconcile the orders left in flight by the previous process — refusing to admit new orders on a book we cannot account for", "err", err, "reconciled_before_failure", swept)
		return false, err
	}
	logger.Info("in-flight orders reconciled", "count", swept, "took", time.Since(sweepStart).String())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)

	// THE OUTBOX RELAY (#292). It is JOINED INTO wg, and that is not tidiness:
	// closeStores() — deferred above, so it runs AFTER wg.Wait() returns — closes
	// the pool this relay reads and writes. An unjoined drainer would still be
	// mid-UPDATE against a closing pool, and its per-key advisory lock would be
	// released by a connection teardown rather than by the unlock it expects.
	// Same hazard test/arch/consumer_goroutine_join_test.go exists for on the
	// subscription goroutines; that guard does not classify this one (it only
	// looks for bus.NewConsumer), so the join is here by argument rather than by
	// enforcement.
	//
	// A relay error IS terminal for the process, unlike a failed sweep pass. Run
	// returns only on a fault that makes draining impossible at all, and an OMS
	// that admits orders while nothing can publish their FACTs is trading
	// invisibly — which is precisely the state this whole change exists to make
	// impossible. Take the pod down and let the other replica carry it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("oms outbox relay armed — order FACTs are published from the store, in the same "+
			"transaction that committed them", "interval", cfg.OutboxInterval.String())
		if err := relay.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
	}()

	for _, s := range subs {
		wg.Add(1)
		go func(s sub) {
			defer wg.Done()
			logger.Info("oms subscribing", "subject", s.subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, s.subject, cfg.ConsumerGroup, s.handler)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(s)
	}
	for _, subject := range cfg.PriceSubjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			logger.Info("oms subscribing to the price spine (broadcast)", "subject", subject)
			err := consumer.SubscribeBroadcast(ctx, subject, marks.Handle)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(subject)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("oms arming the pre-trade mandate registry", "subject", mandateSub)
		// SubscribeBroadcastReady, not SubscribeBroadcast: mandateReg.Arm fires only once
		// DeliverLastPerSubject has drained, i.e. once every portfolio's mandate in force
		// has actually been folded — see the wait loop below for why that distinction is
		// the whole fix.
		err := consumer.SubscribeBroadcastReady(ctx, mandateSub, mandateConsumer.Handle, mandateReg.Arm)
		if err != nil && !errors.Is(err, context.Canceled) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
	}()

	// THE SAME RECONCILIATION, ON A TICKER, WHILE THE POD IS SERVING (#238).
	//
	// The startup sweep above is mandatory and fatal on failure; this one is
	// neither, and the difference is the point. Admission USED to commit the
	// order and then publish its FACT with no outbox between the two, so a failed
	// publish left a durable order the rest of the estate had never heard of.
	// The startup sweep was the compensator for that, which made the recovery
	// latency "whenever this pod next restarts" — days, on a deployment that is
	// behaving. This bounds it at OMS_SWEEP_MIN_AGE + OMS_SWEEP_INTERVAL.
	//
	// #292 MOVED THE ADMISSION HALF TO THE OUTBOX, so what this ticker now
	// recovers is an order left mid-flight at a VENUE, plus the pre-outbox rows
	// that predate migration 0006. It is deliberately kept and deliberately still
	// armed: the outbox is new, this has run, and a compensator is retired after
	// its replacement is proven rather than alongside it.
	//
	// IT IS SAFE TO RUN CONCURRENTLY WITH THE SUBSCRIPTIONS ABOVE, and not by
	// assumption: resume() takes the SAME per-order claim every live handler
	// takes (orderlock.go) and re-reads the order under it, so it cannot
	// interleave with a delivery working the same order; across pods Store.Save's
	// version predicate refuses the loser (#122); and SweepOlderThan skips orders
	// young enough that a live admission might still be between store.Create and
	// the claim it takes afterwards. SweepInterrupted's "must run BEFORE the
	// consumers subscribe" is a completeness rule for STARTUP — do not admit onto
	// a book you cannot account for — not an exclusion mechanism this violates.
	//
	// A FAILURE HERE MUST NOT KILL THE POD. The startup sweep is fatal because a
	// process that cannot account for its predecessor's orders must not begin
	// trading. This one runs in a process that is ALREADY trading, holding live
	// orders at venues, and terminating it would strand exactly what the sweep
	// exists to protect. So it logs at ERROR, counts, and tries again next tick —
	// and the counter is what makes a sweep that has been failing all day
	// distinguishable from one that has found nothing to do.
	if cfg.SweepInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("oms periodic in-flight reconciliation armed",
				"interval", cfg.SweepInterval.String(), "min_age", cfg.SweepMinAge.String())
			ticker := time.NewTicker(cfg.SweepInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// Same explicit tenant as the startup sweep, and for the same
					// reason: this runs outside any inbound delivery, so there is no
					// envelope for bus.Consumer to have stashed a tenant from, and a
					// re-announced FACT would fail envelope validation without one.
					swept, err := svc.SweepOlderThan(bus.WithTenantID(ctx, cfg.Tenant), cfg.SweepMinAge)
					if err != nil {
						sweepFailures.Inc()
						// reconciled_despite_failures, not "…before_failure": unlike the
						// startup sweep this pass does NOT stop at the first order it
						// cannot reconcile, so this count is the orders it DID finish
						// while err records the ones it could not.
						logger.Error("oms: the periodic in-flight reconciliation reported failures — some orders "+
							"left mid-flight are not being recovered, and the pod is still admitting new ones",
							"err", err, "reconciled_despite_failures", swept)
						continue
					}
					if swept > 0 {
						logger.Info("oms periodic reconciliation pass", "orders_examined", swept)
					}
				}
			}
		}()
	} else {
		// SAID OUT LOUD, because off and on must not look the same in a log. With
		// the periodic sweep disabled, an order whose ACCEPTED FACT failed to
		// publish stays unknown to the estate until this pod restarts.
		logger.Warn("oms periodic in-flight reconciliation is DISABLED (OMS_SWEEP_INTERVAL=0) — an order " +
			"whose ORDER_ACCEPTED FACT fails to publish will stay invisible to risk, compliance and the " +
			"audit log until this pod next restarts")
	}

	// READINESS MUST WAIT ON THE MANDATE REPLAY, NOT ON THE SUBSCRIPTION GOROUTINE HAVING
	// STARTED (EXEC-M13).
	//
	// This used to call readiness.Set(true) immediately after LAUNCHING the goroutine above,
	// not after its replay had folded. OMS_REQUIRE_MANDATE defaults to false, and the pre-trade
	// gate's own startup warning says it plainly: "an order for a portfolio with NO MANDATE is
	// ADMITTED, unconstrained." So in the window between the pod reporting Ready and the mandate
	// registry actually catching up, every portfolio looked ungoverned and every order was
	// admitted with no constraint — EXEC-M13 ("a restarted OMS came back with an empty registry
	// and its gate passed every order"), narrowed here from "only on a broken durable-group
	// replay" to "a race on every single rolling restart". The control did not fail loudly. It
	// disarmed silently while the health check said everything was fine.
	//
	// This mirrors webhook-ingest's position-cache wait loop
	// (services/webhook-ingest/cmd/webhook-ingest/main.go) exactly, rather than inventing a new
	// shape: poll Armed() on a short tick with a <-ctx.Done() escape, so a shutdown signal that
	// arrives before the replay lands falls through to graceful shutdown WITHOUT ever reporting
	// ready — a pod that never learned which mandates are in force must not be told it is
	// healthy. On a fresh install with zero mandates published, SubscribeBroadcastReady's ready()
	// fires as soon as the (empty) backlog drains, so this does not deadlock a first deployment;
	// it only closes the race on a populated one.
	//
	// This runs concurrently with the OTHER subscriptions already launched into wg above (order
	// commands, fills, the price spine) — they keep running in their own goroutines regardless of
	// how long this wait takes, so a slow mandate replay delays only the readiness flip, never the
	// rest of the subscription group.
mandateArmWait:
	for !mandateReg.Armed() {
		select {
		case <-ctx.Done():
			// Shutting down before the replay landed. Fall through to wg.Wait() below
			// WITHOUT reporting ready: this pod never learned which mandates are in force.
			break mandateArmWait
		case <-time.After(50 * time.Millisecond):
		}
	}
	if mandateReg.Armed() {
		logger.Info("pre-trade mandate registry armed — mandates in force are known", "subject", mandateSub)
		readiness.Set(true)
	}

	wg.Wait()
	readiness.Set(false)
	return true, firstErr
}

// openStore selects the durable Postgres order store when a DSN is set
// (EXEC-M7c), otherwise the in-memory store. Returns a close func that tears
// down the pool (a no-op for the in-memory store). Both satisfy order.Store, so
// the command handler and the venue adapters share one store either way.
//
// The choice is a SAFETY choice, not a persistence one. order.Store.Create is
// the admission gate that decides which delivery of an order routes to a live
// venue; MemoryStore enforces it with a mutex, so its guarantee stops at the
// process boundary. Two replicas over two maps both admit the same order_id and
// the fund trades twice. Postgres enforces it with a PRIMARY KEY, which holds
// across replicas — so a multi-replica OMS requires OMS_DATABASE_URL.
// THE POSITION BOOK IS THE SAME KIND OF SAFETY CHOICE (EXEC-M18).
//
// position.Book is an in-process map. The projector consumes fills through a durable
// consumer GROUP, which LOAD-BALANCES, so with two pods each folds only the fills it
// received — and then PUBLISHES the result as the fund's ABSOLUTE position. Worse than a
// control that fails: a projection does not merely fail to act, it SPEAKS, and the risk
// engine, the compliance monitor, tv-sync AND THIS OMS'S OWN PRE-TRADE GATE all believe
// it. A concentration limit evaluated against half a book does not refuse loudly; it
// quietly says yes. And a restarted pod comes back flat, so its next 0.1 BTC fill
// publishes `position = 0.1` while the fund holds 5.1.
//
// So both stores come from ONE pool and one DSN, and both degrade together: no
// OMS_DATABASE_URL ⇒ in-memory ⇒ EXACTLY ONE REPLICA, said out loud.
// THE OUTBOX IS THE THIRD STORE, AND IT DEGRADES WITH THE OTHER TWO (#292).
//
// It comes from the same pool for the same reason the position book does — one
// DSN, one tenant scope, one failure domain — and with no DSN it is the
// in-process queue that order.MemoryStore already owns. That is coherent rather
// than convenient: in a memory deployment a crash loses the orders AND the FACTs
// announcing them, together, so the two halves cannot come back disagreeing.
// The single-replica warning below already covers it.
//
// THE RELAY'S TENANT SCOPE COMES FROM THIS POOL AND NOWHERE ELSE. It reads the
// outbox through the same pg.NewTenantPool connections the order store uses, so
// RLS constrains it to exactly the tenant whose orders it is announcing. That is
// the reason the relay is in-process rather than one estate-wide binary: there
// is no unscoped pool on this platform to build such a binary on, and the app
// role is NOSUPERUSER so it could not bypass RLS to read the other tenants
// either. See outbox.Relay.
func openStores(ctx context.Context, cfg config.Config, logger *slog.Logger) (order.Store, position.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		logger.Warn("NO OMS_DATABASE_URL — the order store, the position book AND the outbox are IN-PROCESS. This deployment "+
			"MUST run exactly ONE replica: two pods would each admit the same order (double trade) and each publish "+
			"an ABSOLUTE position folded from only the fills it happened to receive. A crash loses any order FACT "+
			"committed and not yet published",
			"fix", "set OMS_DATABASE_URL; the shipped manifest runs replicas: 2")
		return order.NewMemoryStore(), position.NewBook(cfg.BaseCurrency), func() {}, nil
	}
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC, so Postgres RLS scopes all reads/writes to it (the
	// authenticated-session-GUC pattern). A non-superuser DB role is required for
	// FORCE RLS to apply.
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, nil, err
	}
	return order.NewPostgres(pool), position.NewPostgres(pool, cfg.BaseCurrency), pool.Close, nil
}

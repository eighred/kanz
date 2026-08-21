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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/venuemargin"
	"github.com/eighred/kanz/internal/version"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/approval"
	"github.com/eighred/kanz/services/oms/internal/cashview"
	"github.com/eighred/kanz/services/oms/internal/compliance"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/costwatch"
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/eighred/kanz/services/oms/internal/position"
	"github.com/eighred/kanz/services/oms/internal/riskview"
	"github.com/eighred/kanz/services/oms/internal/server"
	"github.com/eighred/kanz/services/oms/internal/venuesrv"
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
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())), httpserver.Standard())

	// THE POST-TRADE PLANE'S POSTURE, BEFORE ANYTHING ELSE AND ON BOTH BRANCHES
	// (#589). It is reported here rather than inside runConsumers for the reason
	// the accounting and audit roots register their store posture before opening
	// the store: it must be on /metrics from the FIRST scrape, and it must be
	// true of a deployment with no OMS_NATS_URL too. The SETTLEMENT stream is
	// provisioned by the bootstrap job regardless of what this process does, so a
	// broker-less OMS is a deployment where the stream is empty and nothing at
	// all says why. nil running: no stage is wired — see settlement_posture.go.
	settlementPlanePosture(obs.Registry, logger, nil)

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

	// THE PLATFORM KILL-SWITCH, CONSTRUCTED CLOSED (#635). Named haltGate because
	// `gate` in this function is already the pre-trade COMPLIANCE gate, and the two
	// answer different questions: compliance asks whether THIS order is allowed,
	// the halt asks whether ANY order is.
	//
	// Built before the dial so it can be the disconnect watchdog. Until now the OMS
	// did not import the halt package at any depth, so an operator's kanz-halt
	// stopped TradingView signals and this service admitted every order the gateway
	// published, right through the declared halt.
	haltGate := halt.NewGate(time.Now)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client,
		// LOSING THE SPINE CLOSES THE GATE, the stance webhook-ingest has always
		// taken. The halt FACT travels on this connection, and the ephemeral
		// consumer carrying it may not survive a reconnect — so a dropped spine
		// means this OMS can no longer establish that admitting an order is safe.
		// It keeps CANCELLING, which is the half that matters during an outage.
		OnDisconnect: func(err error) {
			haltGate.TripOnBusLoss(err)
			logger.Error("NATS spine lost — the OMS admits no new orders until an operator resumes; "+
				"cancels are unaffected", "err", err)
		},
		OnReconnect: func() {
			_, reason, since := haltGate.State()
			logger.Warn("NATS spine reconnected — halt gate remains latched", "reason", reason, "since", since)
		},
	})
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

	// WHAT EACH PORTFOLIO CAN SPEND, folded from the book of record (#450).
	//
	// The OMS MUST NOT compute a balance: accounting's ledger is the only fold
	// that takes trade legs, cash movements and corporate actions bitemporally
	// with restatements, and a second computation here would drift from it — the
	// drift surfacing as a pre-trade control that refuses or admits wrongly. This
	// only remembers a level accounting announced.
	//
	// An unannounced or STALE portfolio reads as UNKNOWN, and BuyingPowerRule
	// fails closed on unknown: an order under a spending mandate is refused with a
	// named reason rather than admitted on evidence nobody can date.
	cash := cashview.New(
		cashview.WithOnStale(func(portfolioID string, age time.Duration) {
			logger.Warn("oms: cash balance is too old to act on — orders under a buying-power "+
				"mandate will be REFUSED for this portfolio until accounting announces again",
				"portfolio_id", portfolioID, "age", age.String(), "subject", cashview.Subject)
		}),
	)

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
	//
	// IT IS PARSED HERE, AHEAD OF THE PRE-TRADE GATE, because the gate now needs it:
	// the margin control resolves (tenant, portfolio, venue) → exchange account
	// through these bindings before it can ask what the venue reported (#408,
	// control 3). Control 2 stopped being a neighbouring concern and became a
	// precondition the moment margin was switched on, which is what #408 said would
	// happen.
	bindings, err := execution.ParseBindings(cfg.VenueAccounts)
	if err != nil {
		logger.Error("OMS_VENUE_ACCOUNTS is not safe to trade on", "err", err)
		return false, err
	}

	// THE RISK HALF OF THE PRE-TRADE GATE (#438).
	//
	// Order admission never asked the risk engine anything: risk was a downstream
	// OBSERVER of orders, never a gate in front of them, so an order could be
	// inside every mandate rule and still take the portfolio through its VaR
	// limit. This folds the measures risk already publishes so the gate can check
	// them as arithmetic on a local map — no call, no added latency, and a
	// degraded risk engine makes this view STALE (a refusal) rather than making
	// the OMS wait on it (an outage).
	// A MEASURE THE ENGINE COULD NOT COMPUTE IS COUNTED, NOT JUST DROPPED (#509).
	//
	// The view declines to fold a measure whose coverage says it was computed over
	// an incomplete book, so a risk limit over it refuses. That refusal otherwise
	// looks identical to an engine that never published — and the two need
	// different people: a stale view is a risk-engine incident, this is a
	// REFERENCE-DATA one. The contract-terms store has no production writer today,
	// so the fixed-income measures are the population this counts.
	//
	// Zero on registration, so "no unresolved measure has ever arrived" is a
	// reading rather than an absent series.
	unresolvedMeasures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_risk_measures_unresolved_total",
		Help: "Announced risk measures this OMS refused to fold because the engine computed " +
			"them over an incomplete book (domain.v1.RiskMeasure.coverage). Non-zero means a " +
			"risk-limit mandate over that measure is REFUSING orders, and the fix is upstream " +
			"reference data, not the risk engine.",
	})
	obs.Registry.MustRegister(unresolvedMeasures)

	risk := riskview.New(
		riskview.WithOnStale(func(portfolioID, measure string, age time.Duration) {
			logger.Warn("oms: a risk measure is too old to gate on — orders under a risk-limit "+
				"mandate will be REFUSED for this portfolio until the engine publishes again",
				"portfolio_id", portfolioID, "measure", measure, "age", age.String(),
				"subject", riskview.Subject)
		}),
		riskview.WithOnUnresolved(func(portfolioID, measure string, excluded uint32) {
			unresolvedMeasures.Inc()
			logger.Warn("oms: a risk measure was announced having been computed over an "+
				"INCOMPLETE BOOK and will not gate anything — orders under a mandate naming it "+
				"will be REFUSED until the engine can resolve its inputs",
				"portfolio_id", portfolioID, "measure", measure, "excluded_positions", excluded,
				"subject", riskview.Subject)
		}),
	)
	// HELD vs CURRENT, because the difference is what an operator needs BEFORE a
	// risk limit starts refusing everything. A portfolio held but not current is
	// one whose gate is about to fail closed, and that is a different incident
	// from one the engine has never computed.
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_risk_portfolios_held",
		Help: "Portfolios whose risk measures this OMS has folded at all.",
	}, func() float64 { held, _ := risk.Stats(); return float64(held) }))
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_risk_portfolios_current",
		Help: "Portfolios whose risk measures are within OMS freshness — the ones a declared " +
			"risk limit can actually be checked against. held − current is the population whose " +
			"orders a risk mandate will refuse.",
	}, func() float64 { _, live := risk.Stats(); return float64(live) }))

	// THE MARGIN HALF OF THE PRE-TRADE GATE (#408, control 3).
	//
	// Control 1 made the exchange's own margin state a first-class read; this is
	// what reads it. A portfolio whose mandate declares margin trading has its
	// orders gated on the venue's own maintenance figures for the account it
	// spends from, and every way of not knowing them — never observed, observed
	// too long ago, observed without a ratio, observed with the venue declining
	// part of the answer, or bound to no account at all — REFUSES the order.
	//
	// THE FRESHNESS BOUND IS TWO MINUTES, not the fifteen cash and risk allow, and
	// the difference is the point. Margin does not move on our schedule: it moves
	// with the mark, and the scenario this control exists for is a fast move. A
	// margin ratio from ten minutes ago during one is not a margin ratio.
	uncoveredMargin := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_venue_margin_uncovered_total",
		Help: "Venue margin observations this OMS folded in which the EXCHANGE declined part of " +
			"the answer (collateral.v1.VenueMarginState.coverage). Non-zero means orders under a " +
			"venue-margin mandate are being REFUSED on that account, and the fix is at the venue " +
			"or in the adapter's margin call, not in the OMS.",
	})
	obs.Registry.MustRegister(uncoveredMargin)
	undatedMargin := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_venue_margin_undated_total",
		Help: "Venue margin observations discarded for carrying no observation time. An undated " +
			"margin figure cannot be judged against a freshness bound, so it is dropped and the " +
			"account ages out to UNKNOWN — which fails every margin control closed.",
	})
	obs.Registry.MustRegister(undatedMargin)

	margins := venuemargin.New(
		venuemargin.WithOnStale(func(venue, account string, age time.Duration) {
			logger.Warn("oms: the exchange's margin state for this account is too old to act on — orders "+
				"under a venue-margin mandate will be REFUSED for it until the adapter observes again",
				"venue", venue, "account", account, "age", age.String(),
				"max_age", venuemargin.DefaultMaxAge.String(), "subject", venuemargin.Subject)
		}),
		venuemargin.WithOnUncovered(func(venue, account string, excluded uint32) {
			uncoveredMargin.Inc()
			logger.Warn("oms: the exchange did not report part of this account's margin state — those "+
				"quantities are UNKNOWN, not zero, and orders under a venue-margin mandate are REFUSED "+
				"on this account until the venue answers in full",
				"venue", venue, "account", account, "excluded", excluded, "subject", venuemargin.Subject)
		}),
		venuemargin.WithOnUndated(func(venue, account string) {
			undatedMargin.Inc()
			logger.Warn("oms: discarded an UNDATED margin observation — a figure of unknown age cannot be "+
				"judged against a freshness bound, so the account will age out to UNKNOWN",
				"venue", venue, "account", account, "subject", venuemargin.Subject)
		}),
	)
	// HELD vs CURRENT, the same pair the risk view exports and for the same
	// reason: the gap between them is the population whose orders a margin mandate
	// is about to refuse, and it is visible minutes before anybody files a ticket
	// about rejections.
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_venue_margin_accounts_held",
		Help: "Exchange accounts whose venue margin state this OMS has folded at all. ZERO MEANS " +
			"NOTHING IS OBSERVING MARGIN, not that the accounts are safe.",
	}, func() float64 { held, _ := margins.Stats(); return float64(held) }))
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_venue_margin_accounts_current",
		Help: "Exchange accounts whose venue margin state is within OMS freshness — the ones a " +
			"declared venue-margin rule can actually be checked against. held − current is the " +
			"population whose orders that rule will refuse.",
	}, func() float64 { _, live := margins.Stats(); return float64(live) }))

	// THE CLASSIFIER SEAM IS NIL AND THAT IS A DECISION, not an oversight
	// (#640). No production compliance.Classifier exists — the reference-data
	// source it would read is a schema with no writer — and inventing a sector
	// or issuer map to fill it would make a control confidently wrong instead of
	// merely unarmed. What it must not be is QUIET: a mandate rule naming the
	// SECTOR, ISSUER or ASSET_CLASS dimension is now REFUSED with a named reason
	// (compliance.unresolvedDimension) rather than passed. That is only
	// reachable for a portfolio whose mandate DECLARES such a rule, so nothing
	// trading today changes; before it, a book 100% in one sector was admitted
	// under a 10% cap on that sector and the audit trail recorded a pass.
	// test/arch/no_nil_classifier_seam_test.go keeps this nil tracked.
	preTrade := comp.NewPreTradeGate(
		comp.NewEngine(nil), compliance.NewBookSource(book, cash, risk), mandateReg, nil, nil, logger,
		comp.WithRequireMandate(cfg.RequireMandate),
		// THE SOURCE IS ALWAYS WIRED, even on a deployment that binds no accounts
		// and observes no margin. It answers UNKNOWN there, which refuses — and
		// refusing is only reachable for a portfolio whose mandate DECLARES margin
		// trading, so nothing that trades today changes. Leaving the seam nil
		// instead would make "no margin control on this build" and "margin unknown"
		// the same observable state, which is the conflation this estate designs
		// against.
		comp.WithMarginSource(compliance.NewMarginSource(bindings, margins)),
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

	// MAKER-CHECKER ON ORDER SUBMISSION (#410, act three).
	//
	// THE SAME MARK FOLD THE PRE-TRADE GATE USES, deliberately. One price source
	// means the compliance record and the four-eyes record can never disagree
	// about how large the same order was — the reason compliance.OrderPrice is one
	// exported function rather than a switch copied into this control.
	//
	// THE GATE IS BUILT WHETHER OR NOT A THRESHOLD IS CONFIGURED. With none, every
	// admitted order is still counted under posture="absent", so "this deployment
	// has no dual-control threshold" is a series on a dashboard rather than a
	// missing metric — "nothing configured" and "checked, and fine" must not look
	// the same, and neither must "nothing configured" and "this build has no gate".
	dualControl, err := approval.NewGate(cfg.RequireDualControl, cfg.DualControlMinNotional, marks, obs.Registry)
	if err != nil {
		logger.Error("oms: dual-control gate refused its configuration", "err", err)
		return false, err
	}
	if dualControl.Armed() {
		// THE ENFORCING POSTURE, AND IT IS THE ONE THAT NEEDS A HUMAN ON THE OTHER
		// END. An order at or above the threshold is written to order_proposals
		// and does not trade until a DIFFERENT authenticated subject approves it.
		// If nobody watches the pending queue, large orders stop being placed and
		// expire — which is a trading outage, not a security posture, and the log
		// says so at startup rather than leaving it to be discovered.
		logger.Warn("oms: MAKER-CHECKER IS ENFORCING — an order at or above the threshold is HELD in "+
			"order_proposals and is NOT admitted until a second, different authenticated subject "+
			"approves it. Somebody must own the pending queue or these orders expire unfilled (#410)",
			"threshold", dualControl.Threshold().FloatString(2), "currency", cfg.DualControlNotionalCurrency,
			"ttl", dualcontrol.DefaultTTL.String(),
			"metric", "kanz_oms_order_signatures_total")
	} else if dualControl.Watching() {
		logger.Info("oms: MAKER-CHECKER IS OBSERVING, NOT ENFORCING — orders at or above the threshold "+
			"are admitted on ONE signature and counted. Set OMS_REQUIRE_DUAL_CONTROL=true to hold them "+
			"once somebody owns the pending queue",
			"threshold", dualControl.Threshold().FloatString(2), "currency", cfg.DualControlNotionalCurrency,
			"metric", "kanz_oms_order_signatures_total")
	} else {
		logger.Warn("oms: NO DUAL-CONTROL THRESHOLD — every order, of any size, is committed on one " +
			"person's authority (#410). Set OMS_DUAL_CONTROL_MIN_NOTIONAL (e.g. \"1000000 USD\") to " +
			"start counting how much of the flow would need a second signature")
	}

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

	// Orders that margin against an account nobody bound to their portfolio. Non-zero
	// means some part of the book is sharing collateral with the rest of it.
	sharedCollateral := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_shared_collateral_orders_total",
		Help: "Orders executed against an exchange account NOT bound to their portfolio — i.e. against " +
			"a collateral pool shared with every other unbound portfolio at that venue. An exchange " +
			"liquidates per account, so this is the number of orders whose segregation is nominal only. " +
			"COUNTS ORDERS, AND ONE DECISION IS NOW N ORDERS (#435): an order worked as a " +
			"schedule appears as a resting parent plus one child per slice, so a threshold " +
			"tuned against decisions is wrong by a factor of slice_count for that flow.",
	})
	obs.Registry.MustRegister(sharedCollateral)

	quarantined := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_orders_quarantined_total",
		Help: "Orders frozen because the platform could not establish what the venue did with them — " +
			"the venue denied an order it had acknowledged, could not be queried at all, or reported " +
			"a fill the order cannot accept. Each one is a position whose true size nobody knows, and " +
			"it will not be re-driven, cancelled or mentioned again until a human resolves it. " +
			"This should be zero; any non-zero value is an incident, not a metric to trend. " +
			"COUNTS ORDERS, AND ONE DECISION IS NOW N ORDERS (#435): an order worked as a " +
			"schedule appears as a resting parent plus one child per slice, so a threshold " +
			"tuned against decisions is wrong by a factor of slice_count for that flow.",
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
			"withdrawal did not land; it should be zero. " +
			"COUNTS ORDERS, AND ONE DECISION IS NOW N ORDERS (#435): an order worked as a " +
			"schedule appears as a resting parent plus one child per slice, so a threshold " +
			"tuned against decisions is wrong by a factor of slice_count for that flow.",
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

	proposalExpiryFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_proposal_expiry_failures_total",
		Help: "Proposal-expiry sweep passes that ended in an error. The pod keeps trading, so nothing " +
			"else surfaces this. Sustained non-zero means a held order that will NEVER trade is still " +
			"telling the trader who submitted it nothing at all — the silent drop #539 exists to end, " +
			"reappearing one layer down.",
	})
	obs.Registry.MustRegister(proposalExpiryFailures)

	// THE EXECUTION-ALGORITHM DRIVER'S THREE SIGNALS (#435).
	//
	// A parent order that is not being worked looks exactly like one being worked
	// slowly: it rests at WORKING_SCHEDULED either way, and every screen shows it
	// live. These are what tell the two apart.
	scheduleChildren := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_schedule_children_total",
		Help: "Child orders created by the execution-algorithm driver. Rising means parent orders " +
			"are being worked; flat while parents rest means their quantity is reaching no venue.",
	})
	scheduleFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_schedule_failures_total",
		Help: "Driver passes that ended in an error. The pod keeps trading, so nothing else surfaces " +
			"this. Sustained non-zero means at least one parent order is not being sliced and its " +
			"quantity is reaching no venue at all.",
	})
	// A GAUGE, NOT A COUNTER, because it is a CONDITION rather than an event: the
	// tick is either fast enough for the book currently being worked or it is not,
	// and it can become wrong the moment somebody submits a tighter schedule.
	scheduleTickTooSlow := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kanz_oms_schedule_tick_too_slow",
		Help: "1 when OMS_SCHEDULE_INTERVAL is longer than the tightest slice gap the driver is " +
			"working, meaning those orders are being sliced more coarsely than they were asked to " +
			"be and every slice is late by up to one tick. This has NO other symptom: the quantities " +
			"still sum, no error is raised, and it looks identical to a slow venue.",
	})
	obs.Registry.MustRegister(scheduleChildren, scheduleFailures, scheduleTickTooSlow)

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

	// Venue adapters that did not say which order types they can place (#405).
	// Non-zero means the admission gate is OPEN for that MIC: an order type the
	// adapter cannot translate will be admitted, stored and announced, and refused
	// only at the exchange. Zero is the goal; OMS_REQUIRE_ORDER_TYPE_SUPPORT is how
	// it is held there once the fleet is upgraded.
	undeclaredOrderTypes := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_undeclared_venue_order_types_total",
		Help: "Venue adapters registered that declared no supported order types. The OMS cannot refuse an " +
			"unroutable order type at admission for these, so one reaches the venue and fails there instead.",
	})
	obs.Registry.MustRegister(undeclaredOrderTypes)

	// A SEPARATE COUNTER FROM THE ONE ABOVE (#486), because they are separate
	// gaps with separate fixes. An adapter may answer the order-type question and
	// not the time-in-force one; collapsing both into ..._order_types_total would
	// report an adapter as having failed to declare order types when it declared
	// them perfectly well.
	undeclaredTimeInForce := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_undeclared_venue_time_in_force_total",
		Help: "Venue adapters that did not declare which time-in-force instructions they can " +
			"express. Non-zero means this OMS cannot refuse an inexpressible time-in-force at " +
			"admission for that venue, so an order will be accepted and announced and then " +
			"refused by the connector. Distinct from the order-type gap: an IOC placed as " +
			"good-til-cancelled does not fail, it RESTS — the trader asked to hold no exposure " +
			"and holds it.",
	})
	obs.Registry.MustRegister(undeclaredTimeInForce)

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
	venues, catalogue, closeVenues, err := configuredVenues(ctx, cfg, store, producer, unverifiedAccounts, undeclaredOrderTypes, undeclaredTimeInForce, logger)
	if err != nil {
		return false, err
	}
	defer closeVenues()

	// THE READ SURFACE (#399 order history, #406 tradeable instruments), if this
	// deployment serves one.
	//
	// It is started HERE rather than beside the HTTP health server for two
	// reasons. It needs the store, whose lifetime is this function's — serving
	// reads from a store closeStores has already closed is the shape of bug the
	// outbox relay's join comment describes, one layer up. And it needs the venue
	// catalogue, which only exists once every adapter has been dialled and asked:
	// opening the port first would serve an EMPTY instrument list during startup,
	// and an empty list is indistinguishable from "this deployment trades
	// nothing". A caller cannot tell a race from a fact.
	//
	// mesh is the SAME identity the bus client uses — this process has one
	// workload identity and the transport package is explicit that it should not
	// be built twice.
	if cfg.GRPCListen != "" {
		stopGRPC, gerr := serveOrderQuery(cfg, mesh, store, catalogue, logger)
		if gerr != nil {
			return false, gerr
		}
		defer stopGRPC()
	}

	// WHERE DOES AN UNTARGETED ORDER GO? (#437) The router used to answer
	// venues[0] — whichever adapter the config happened to list first, chosen by
	// nobody, while the type called itself a smart order router. It is now a named
	// choice, and an ambiguous one is refused rather than guessed.
	// MEASURED VENUE COST, FOR AN UNTARGETED ORDER (#437 B, on #436's signal).
	//
	// costwatch folds every fill's realized shortfall into this, and the router
	// reads it when an order names no venue. It is a PREFERENCE over venues that
	// have already passed every correctness check — it cannot admit a venue they
	// refused, and it abstains below its evidence floor, leaving the DECLARED
	// default in charge. So the worst it does is prefer the wrong one of two
	// acceptable venues, better informed than the config line it defers to.
	//
	// In-process, deliberately: the OMS publishes order.cost.recorded but does not
	// subscribe to it. Reading its own FACT back would put a broker round trip
	// inside a feedback loop. The honest cost is that each replica ranks on the
	// fills it saw — acceptable for a preference, and the sample floor means a
	// replica with thin evidence abstains rather than acting on noise.
	venueCosts := execution.NewVenueCosts()
	router := execution.NewRouter(venues,
		execution.WithDefaultVenue(cfg.DefaultVenueMIC),
		execution.WithCostRanker(venueCosts))
	if v, ok := router.DefaultVenue(); ok {
		logger.Info("oms: orders naming no venue will be worked at", "venue", v.MIC(),
			"named_explicitly", cfg.DefaultVenueMIC != "")
	} else if cfg.DefaultVenueMIC != "" {
		// FATAL, unlike the ambiguous case below. Somebody NAMED a venue and this
		// OMS holds no adapter for it — a typo in a MIC, or a default left pointing
		// at an adapter that was removed. Starting anyway would leave a deployment
		// that looks configured, logs nothing, and refuses every untargeted order
		// with an error naming a venue the operator believes is wired.
		//
		// Refusing to start is safe HERE specifically because OMS_DEFAULT_VENUE_MIC
		// is new in #437: no deployment sets it yet, so this cannot turn an existing
		// estate's silent state into an outage. The only way to reach it is to set
		// it wrong today.
		return false, fmt.Errorf("oms: OMS_DEFAULT_VENUE_MIC=%q but no configured adapter holds it "+
			"(this OMS holds %s) — an untargeted order would be refused naming a venue you "+
			"believe is wired", cfg.DefaultVenueMIC, strings.Join(micsOf(venues), ", "))
	} else if len(venues) > 1 {
		// NOT FATAL. Every order from the fan-out producers carries a target, so
		// this deployment trades perfectly well without a default; refusing to
		// start over a path nothing currently takes would be a self-inflicted
		// outage. An order that does arrive untargeted is refused with the
		// candidates named.
		logger.Warn("oms: NO DEFAULT VENUE and more than one is configured — an order naming no "+
			"venue will be REFUSED rather than sent somewhere nobody chose. Set OMS_DEFAULT_VENUE_MIC "+
			"to pick one", "venues", strings.Join(micsOf(venues), ","))
	}
	svc, err := order.NewService(cfg.Tenant, store, emitter, gate, router, closeRegistry, logger,
		// The decision-time benchmark for every admitted order (#436). The same
		// mark fold the pre-trade gate values MARKET/STOP orders against — one
		// price source, so a cost measure and a compliance check can never
		// disagree about what the market showed.
		order.WithArrivalMarks(marks),
		// THE BRAKE. NewService REFUSES to build without it (#635), so this line is
		// not something a future edit can quietly drop.
		order.WithHaltGate(haltGate),
		order.WithDualControl(dualControl),
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

	// group is per-subscription, NOT one shared name. Two handlers on the SAME
	// subject under the SAME durable group LOAD-BALANCE: each would receive a
	// disjoint half of the fills. On this subject that is not a degraded report,
	// it is a corrupt position book — the projector would fold half the fills and
	// believe it had them all.
	type sub struct {
		subject string
		group   string
		handler bus.EventHandler
	}
	var subs []sub
	for _, s := range cfg.CommandSubjects() {
		subs = append(subs, sub{s, cfg.ConsumerGroup, svc.Handle})
	}
	// REALIZED EXECUTION COST, PER FILL, BY VENUE (#436).
	//
	// A THIRD consumer of the same fill FACTs, on its own durable group so it
	// cannot starve the position projector or the books. It only measures —
	// nothing it computes reaches a routing or sizing decision — so a failure
	// here costs a report, never an order.
	//
	// It reads the fill FACT rather than the stored order because the FEE lives
	// only in the FACT: OrderState keeps a cumulative quantity and an average
	// price and no fee total, and a cost measure that drops fees ranks a zero-fee
	// venue with poor fills above a maker-rebate venue with good ones.
	costs := costwatch.New(obs.Registry, cfg.Tenant, producer, venueCosts, logger)
	for _, s := range cfg.FillSubjects() {
		subs = append(subs, sub{s, cfg.ConsumerGroup, projector.Handle})
		// ITS OWN GROUP, so it sees EVERY fill and takes none from the projector.
		subs = append(subs, sub{s, cfg.ConsumerGroup + "-tca", costs.Handle})
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

	// THE HALT SUBSCRIPTION, ARMED BEFORE THE COMMAND SUBSCRIPTIONS (#635) — the
	// order matters: an OMS that started consuming order.order.submit before it
	// knew the platform mode would admit whatever arrived in that window.
	//
	// Arm blocks until the broker confirms it, so a missing platform.mode.changed
	// grant surfaces here. It is NOT fatal: Arm has already latched the gate
	// closed, so this OMS refuses every new order with that reason and keeps
	// cancelling. Taking the pod down instead would remove the exits too.
	waitHalt, herr := halt.Arm(ctx, consumer, haltGate, logger)
	if herr != nil {
		logger.Error("halt gate is NOT armed — this OMS will refuse every new order until it is "+
			"restarted; cancels are unaffected", "err", herr)
	}
	if waitHalt != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := waitHalt(); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("halt subscription ended — the OMS admits no new orders", "err", err)
			}
		}()
	}

	for _, s := range subs {
		wg.Add(1)
		go func(s sub) {
			defer wg.Done()
			logger.Info("oms subscribing", "subject", s.subject, "group", s.group)
			err := consumer.Subscribe(ctx, s.subject, s.group, s.handler)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(s)
	}
	// THE CASH SPINE — BROADCAST, NOT A WORK QUEUE, for exactly the reason the
	// price spine above and the mandate registry below are (#450).
	//
	// A consumer GROUP load-balances, so each replica would hold a DIFFERENT
	// subset of portfolios' balances: the same order would be admitted by one pod
	// and refused "cash unavailable" by the other. "Whether your order is legal
	// depends on which pod got it" is not a property a compliance gate may have.
	// A balance is replicated STATE, not work.
	//
	// DeliverLastPerSubject replays the latest announcement, so a cold pod boots
	// holding a balance rather than refusing every order under a spending mandate
	// until accounting next folds something.
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("oms subscribing to the cash spine (broadcast)", "subject", cashview.Subject)
		err := consumer.SubscribeBroadcast(ctx, cashview.Subject, cash.Handle)
		if err != nil && !errors.Is(err, context.Canceled) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
	}()
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
	// RISK MEASURES — BROADCAST, NOT A WORK QUEUE, for exactly the reason the
	// price spine is (#438).
	//
	// A durable GROUP load-balances, so with replicas: 2 each pod would fold only
	// the measures it happened to receive and hold a DIFFERENT risk map. The gate
	// checks a declared risk limit against that map, so the same order would be
	// admitted by one pod and refused by the other — and "whether your order is
	// legal depends on which pod got it" is not a property a pre-trade gate may
	// have. A risk measure is replicated STATE, not work.
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("oms subscribing to risk measures (broadcast)", "subject", riskview.Subject)
		err := consumer.SubscribeBroadcast(ctx, riskview.Subject, risk.Handle)
		if err != nil && !errors.Is(err, context.Canceled) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
	}()

	// VENUE MARGIN — BROADCAST, for the same reason risk measures are (#408).
	//
	// A durable GROUP load-balances, so with replicas: 2 each pod would fold only
	// the observations it happened to receive and hold a DIFFERENT margin state.
	// The gate refuses an order when margin is unknown, so the same order would be
	// admitted by one pod and refused by the other — and on a control whose whole
	// purpose is to fail closed, half the pods failing closed is not a weaker
	// version of the control, it is the control not existing. Margin state is
	// replicated STATE, not work.
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("oms subscribing to venue margin state (broadcast)", "subject", venuemargin.Subject)
		err := consumer.SubscribeBroadcast(ctx, venuemargin.Subject, margins.Handle)
		if err != nil && !errors.Is(err, context.Canceled) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
	}()

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

	// THE EXPIRY SWEEP (#539): a held order nobody signed says so.
	//
	// Between #537 and #539 this branch did not exist, and its absence was the
	// silent one. An order over the dual-control threshold is held rather than
	// admitted; if nobody signs it before dualcontrol.DefaultTTL the proposal
	// leaves the pending queue and — until this — entered no other queue at all.
	// The trader saw ORDER_PENDING_APPROVAL and then nothing, ever: no rejection,
	// no CommandOutcome, no order. That is precisely "an order that neither
	// executes nor reports why", produced by the control built to prevent it.
	//
	// NO LEASE, NO LEADER, ON PURPOSE. oms-deploy.yaml runs replicas: 2 and both
	// pods sweep. AnnounceExpiry is a conditional UPDATE whose rows-affected is
	// the verdict, so the loser announces nothing — the same argument Claim makes
	// on the approval path, and the same one the schedule driver makes for needing
	// no lease.
	//
	// A FAILURE IS NOT FATAL. The orders it would announce are already held; the
	// next tick tries again. Taking the OMS down over a sweep would stop admitting
	// new orders to fix a reporting gap on old ones.
	if cfg.ProposalExpiryInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("oms proposal-expiry sweep armed",
				"interval", cfg.ProposalExpiryInterval.String(), "ttl", dualcontrol.DefaultTTL.String())
			ticker := time.NewTicker(cfg.ProposalExpiryInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// The explicit tenant, for the reason the sweep above gives: this
					// runs outside any inbound delivery, so there is no envelope to
					// have stashed one, and outbox.From refuses a record without it
					// rather than enqueueing a FACT that can never be published.
					if err := svc.ExpireProposals(bus.WithTenantID(ctx, cfg.Tenant)); err != nil {
						proposalExpiryFailures.Inc()
						logger.Error("oms: the proposal-expiry sweep reported failures — a held order "+
							"that will never trade is still telling its submitter nothing",
							"err", err)
						continue
					}
				}
			}
		}()
	} else {
		// SAID OUT LOUD. Off and armed must not look the same, and this one is
		// worse than most when it is off: the orders it would announce are ones a
		// human is waiting on.
		logger.Warn("oms proposal-expiry sweep is DISABLED (OMS_PROPOSAL_EXPIRY_INTERVAL=0) — a held " +
			"order nobody signs will leave the pending queue at its deadline and announce nothing, " +
			"so the trader who submitted it never learns it will not trade (#539)")
	}

	// THE EXECUTION-ALGORITHM DRIVER (#435): what actually works a parent order.
	//
	// Deliberately SEPARATE from the sweep above, though both are tickers over the
	// order book. They answer opposite questions — the sweep asks "what did the
	// previous process leave unfinished", this asks "what is due now" — and the
	// sweep's SweepMinAge floor, which exists so it cannot race a live admission,
	// would put a two-minute delay in front of every first slice if the two shared
	// a loop. Conflating them would also make one stuck parent capable of shadowing
	// crash recovery for the whole book.
	if cfg.ScheduleInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("oms execution-algorithm driver armed", "interval", cfg.ScheduleInterval.String())
			ticker := time.NewTicker(cfg.ScheduleInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// Same explicit tenant as both sweeps, and for the same reason:
					// this runs outside any inbound delivery, so there is no envelope
					// for bus.Consumer to have stashed a tenant from, and a child's
					// ACCEPTED FACT would fail envelope validation without one.
					tctx := bus.WithTenantID(ctx, cfg.Tenant)

					// IS THE TICK FAST ENOUGH FOR THE WORK IT IS DRIVING? Checked
					// every pass, because the answer changes with the book: an
					// operator can submit a one-minute schedule at any moment, and
					// the only symptom of a tick too slow for it is slices arriving
					// late — which looks exactly like a slow venue.
					if gap, parents, ok := svc.TightestSliceInterval(tctx); ok && gap < cfg.ScheduleInterval {
						scheduleTickTooSlow.Set(1)
						logger.Warn("oms: the execution-algorithm driver ticks more slowly than the "+
							"tightest schedule it is working — those orders are being sliced more "+
							"coarsely than they were asked to be, and every slice will be late by up "+
							"to one tick. Lower OMS_SCHEDULE_INTERVAL below the tightest slice gap.",
							"interval", cfg.ScheduleInterval.String(), "tightest_slice_gap", gap.String(),
							"parents_working", parents)
					} else {
						scheduleTickTooSlow.Set(0)
					}

					created, err := svc.DriveSchedules(tctx)
					if err != nil {
						scheduleFailures.Inc()
						// children_created_despite_failures, not "…before_failure":
						// like the periodic sweep this pass does NOT stop at the
						// first parent it cannot advance, so this is what it DID
						// send while err records the parents it could not.
						logger.Error("oms: the execution-algorithm driver reported failures — some parent "+
							"orders are not being worked, and their quantity is reaching no venue at all",
							"err", err, "children_created_despite_failures", created)
						continue
					}
					if created > 0 {
						scheduleChildren.Add(float64(created))
						logger.Info("oms schedule driver pass", "children_created", created)
					}
				}
			}
		}()
	} else {
		// SAID OUT LOUD, for the same reason as the sweep above and with a worse
		// consequence: with the driver off, a parent order is admitted, announced,
		// and shown working on every screen while NOTHING will ever slice it. Its
		// quantity reaches no venue, ever, and no error is raised anywhere.
		logger.Warn("oms execution-algorithm driver is DISABLED (OMS_SCHEDULE_INTERVAL=0) — any order " +
			"submitted with an execution_schedule will be admitted and then rest forever: its quantity " +
			"will reach no venue and nothing further will say so")
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

// serveOrderQuery starts the order.v1 read surface and returns a graceful stop.
//
// mTLS WHEN THE MESH HAS AN IDENTITY, plaintext otherwise — the same rule the
// risk engine's query surface follows, and for the same reason: a local run has
// no SPIFFE socket and must still be drivable, while a deployed one must not
// serve a portfolio's trading history to an unauthenticated peer.
//
// A bind failure is returned SYNCHRONOUSLY so startup fails loudly. Serving on a
// goroutine and logging the error would leave the pod Ready with no read surface
// — an outage that looks like a routing bug, which is the shape web-bff's static
// root refuses for the same reason.
func serveOrderQuery(cfg config.Config, mesh *transport.Mesh, store order.Store, catalogue []execution.VenueInstrument, logger *slog.Logger) (func(), error) {
	var opts []grpc.ServerOption
	if mesh.Enabled() {
		opts = append(opts, transport.ServerOption(mesh.Source, transport.AuthorizeMesh()))
		logger.Info("order query gRPC: mTLS enabled")
	} else {
		logger.Warn("order query gRPC: serving plaintext (no SPIFFE_ENDPOINT_SOCKET)")
	}

	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		return nil, fmt.Errorf("order query gRPC: listen %s: %w", cfg.GRPCListen, err)
	}
	srv := grpc.NewServer(opts...)
	// cfg.Tenant is the owning tenant of THIS deployment; grpcsrv stamps it on
	// every reply as the deny-by-default gate input. Empty fails closed.
	// THE PENDING QUEUE IS WIRED FROM THE SAME STORE (#410). A held order is in
	// order_proposals and nowhere else, so this route is the only way a person
	// can see one — building the read surface without it would hold orders
	// nobody could find, which is the drop the control exists to end.
	grpcsrv.New(store, store.Proposals(), time.Now, cfg.Tenant).Register(srv)
	venuesrv.New(catalogue, cfg.Tenant).Register(srv)

	go func() {
		logger.Info("order query gRPC listening", "addr", cfg.GRPCListen)
		if err := srv.Serve(lis); err != nil {
			logger.Error("order query gRPC server failed", "err", err)
		}
	}()
	return srv.GracefulStop, nil
}

// micsOf lists the configured venue MICs for the startup log.
func micsOf(venues []execution.Venue) []string {
	out := make([]string, 0, len(venues))
	for _, v := range venues {
		out = append(out, v.MIC())
	}
	return out
}

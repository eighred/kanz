package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/order"
)

// venueDescribeTimeout bounds the startup ask ("which account do you hold?") against
// each adapter. It is the EXEC-M10 stance: an unbounded startup call against a
// dependency that has not scheduled yet is a process that hangs Running, 0/1 Ready,
// forever, with nothing in the platform able to say why. Bounded, it exits with a
// named error and the kubelet restarts it with backoff.
const venueDescribeTimeout = 30 * time.Second

// closeRegistry is the shared in-flight-close registry — the In-Flight Certainty
// seam's single instance for the process. It is deliberately UNTAGGED and shared:
// the OMS order service Tracks a close here the moment it dispatches a venue
// cancel (the writer), and whichever exchange reconcilers are compiled in drain
// it from their healing watchdogs (the readers). One registry across Binance and
// OKX — the seam is shared, never duplicated per venue.
var closeRegistry = execution.NewCloseRegistry()

// venueCapabilityCounters is the dial-time capability signal, as ONE named
// thing rather than four positional prometheus.Counter parameters.
//
// It lives here, beside dialVenues — the only code that increments any of them —
// and it exists because the composition root said so: adding the margin counter
// inline pushed runConsumers past the #643 ratchet, and that guard's own answer
// ("put the new wiring in a named builder beside it instead") is the right one.
// Four counters threaded through two signatures were already the shape of
// something that wanted a name.
//
// THEY ARE FOUR NUMBERS, NOT ONE. Each names a different gap with a different
// fix, and an adapter can close one while leaving another open; a shared counter
// would report an adapter as having failed to declare order types when it
// declared them perfectly well.
// EACH FIELD IS NAMED FOR ITS METRIC, and that is load-bearing rather than
// tidy. TestEveryRegisteredMetricHasAWriter tracks a collector from where it is
// BOUND to where it is written by IDENTIFIER NAME — it has no type resolution —
// so binding a counter to `unverifiedAccounts` and then writing it through a
// field called `unverified` makes the metric look writer-less and the guard
// report it as dead. It did exactly that here. One name from registration to
// increment keeps the metric traceable to the guard and to a person reading it.
type venueCapabilityCounters struct {
	unverifiedAccounts    prometheus.Counter
	undeclaredOrderTypes  prometheus.Counter
	undeclaredTimeInForce prometheus.Counter
	undeclaredMarginModes prometheus.Counter
}

// newVenueCapabilityCounters builds the four counters and registers them.
//
// The collectors are bound INSIDE a composite literal rather than assigned to
// fields afterwards, because that is the shape TestEveryRegisteredMetricHasAWriter
// recognises as a binding: `c.field = prometheus.NewCounter(...)` reads to it as
// "constructed inline and bound to nothing, so nothing can ever write it". The
// guard is right to be strict there — an unbound collector exports a family with
// no series, and an empty series reads as zero to every alert and dashboard.
func newVenueCapabilityCounters(reg prometheus.Registerer) venueCapabilityCounters {
	c := venueCapabilityCounters{
		// Venue adapters trading an account NOBODY has proved against the
		// exchange (SOV-02a). The adapter's account is read from its own config,
		// so a mis-declared deployment looks exactly like a correct one —
		// non-zero means some part of the book is settling against a collateral
		// pool that only a human's typing says it belongs to.
		unverifiedAccounts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_oms_unverified_venue_account_total",
			Help: "Venue adapters registered whose exchange account was NOT confirmed by the exchange itself. " +
				"The adapter holds the API credential but has not proved which account it belongs to, so its fills " +
				"could margin against a different fund's collateral than the ledger books them to.",
		}),

		// Venue adapters that did not say which order types they can place
		// (#405). Non-zero means the admission gate is OPEN for that MIC: an
		// order type the adapter cannot translate will be admitted, stored and
		// announced, and refused only at the exchange. Zero is the goal;
		// OMS_REQUIRE_ORDER_TYPE_SUPPORT is how it is held there once the fleet
		// is upgraded.
		undeclaredOrderTypes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_oms_undeclared_venue_order_types_total",
			Help: "Venue adapters registered that declared no supported order types. The OMS cannot refuse an " +
				"unroutable order type at admission for these, so one reaches the venue and fails there instead.",
		}),

		// A SEPARATE COUNTER FROM THE ONE ABOVE (#486), because they are
		// separate gaps with separate fixes. An adapter may answer the
		// order-type question and not the time-in-force one; collapsing both
		// into ..._order_types_total would report an adapter as having failed to
		// declare order types when it declared them perfectly well.
		undeclaredTimeInForce: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_oms_undeclared_venue_time_in_force_total",
			Help: "Venue adapters that did not declare which time-in-force instructions they can " +
				"express. Non-zero means this OMS cannot refuse an inexpressible time-in-force at " +
				"admission for that venue, so an order will be accepted and announced and then " +
				"refused by the connector. Distinct from the order-type gap: an IOC placed as " +
				"good-til-cancelled does not fail, it RESTS — the trader asked to hold no exposure " +
				"and holds it.",
		}),

		// A FOURTH COUNTER, ON THE SAME REASONING (#742). Margin mode is the
		// fifth capability in this family and was the only member with NO
		// dial-time signal at all: DeclaresMarginModes() existed and had zero
		// callers, so an adapter with a silently open margin gate was
		// indistinguishable from one that had been checked and was fine. That is
		// the distinction AGENTS.md says must never collapse.
		//
		// Its consequence is the most expensive of the four. An undeclared order
		// type produces an order that does NOTHING; an inexpressible
		// time-in-force produces one that does the WRONG THING; an unrefused
		// collateral regime produces a position that is REAL and whose regime the
		// fund's records get wrong — the platform reserves margin and buying
		// power against leverage the exchange never applied.
		undeclaredMarginModes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_oms_undeclared_venue_margin_modes_total",
			Help: "Venue adapters that did not declare which collateral regimes they can express. " +
				"Non-zero means this OMS cannot refuse an inexpressible margin mode at admission for " +
				"that venue, so a levered order is accepted, announced, and either refused by the " +
				"connector or placed as spot — leaving a real position whose regime the audit root " +
				"records wrongly.",
		}),
	}
	// Registered together: four collectors, one call, so a fifth added to the
	// struct cannot be constructed and silently left unregistered.
	reg.MustRegister(c.unverifiedAccounts, c.undeclaredOrderTypes,
		c.undeclaredTimeInForce, c.undeclaredMarginModes)
	return c
}

// venuePostureGauges answers ONE question nothing on this process could answer
// before (#865): is this OMS filling orders against a simulator, or against an
// exchange?
//
// The posture was stated only in a log line — the WARN in configuredVenues below
// — and a log line is not a thing an alert, a dashboard, or a caller can read.
// Every other venue signal on this process is a COUNTER OF GAPS, and all four sit
// at zero for an OMS with NO VENUES AT ALL, which is exactly the collapse
// AGENTS.md forbids: "nothing configured" and "checked, and fine" read identically.
// kanz_oms_unverified_venue_account_total == 0 means "every adapter proved its
// account" on a live deployment and "there are no adapters" on a simulator, and
// nothing distinguishes them.
//
// WHO NEEDS TO READ IT. An operator, because a production OMS that fell back to
// the simulator is trading nothing while reporting healthy. And the write-path
// load harness (test/load/orderflow), which must REFUSE to submit an order
// against anything that could reach an exchange — a refusal it can only make
// against a fact the system under test asserts about itself, never against an
// env var the operator running the load test sets for themselves.
//
// TWO GAUGES, NOT A RATIO OR A SINGLE "is_simulated" BOOL. A deployment with a
// live adapter AND a SimVenue is a configuration nothing forbids today (it takes
// an empty OMS_VENUE_ENDPOINTS to reach the sim arm, so it is currently
// unreachable — but a reader must not have to know that to interpret the
// number). Two counts state what is there; a bool would state a conclusion this
// file is not entitled to draw for its reader.
//
// BOTH ARE ALWAYS SET, including to zero. An unset gauge exports a family with no
// series, which every consumer reads as zero — so a harness that required
// "simulated >= 1" would be satisfied by an OMS too old to have this metric at
// all if the absence and a real zero were allowed to look the same. The harness
// requires the FAMILY to be present; this builder is what makes that requirement
// meaningful on every deployment, sim or live.
type venuePostureGauges struct {
	liveVenueAdapters prometheus.Gauge
	simulatedVenues   prometheus.Gauge
}

// newVenuePostureGauges builds and registers the two posture gauges.
//
// Bound INSIDE the composite literal for the reason newVenueCapabilityCounters
// gives above: TestEveryRegisteredMetricHasAWriter reads
// `g.field = prometheus.NewGauge(...)` as "constructed inline and bound to
// nothing", and an unbound collector exports a family with no series.
func newVenuePostureGauges(reg prometheus.Registerer) venuePostureGauges {
	g := venuePostureGauges{
		liveVenueAdapters: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kanz_oms_live_venue_adapters",
			Help: "Out-of-process venue.v1 adapters this OMS registered and can route orders to. " +
				"Each one holds an exchange credential, so a non-zero value means orders placed " +
				"through this process reach a real exchange.",
		}),
		simulatedVenues: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kanz_oms_simulated_venues",
			Help: "In-process SimVenues this OMS registered. Non-zero means orders routed to those " +
				"MICs are filled against NOTHING — correct for tests and local development, and a " +
				"silent trading outage in production, where it means the platform is booking fills " +
				"no exchange ever made.",
		}),
	}
	reg.MustRegister(g.liveVenueAdapters, g.simulatedVenues)
	return g
}

// configuredVenues is the venue composition root. Every venue is now OUT OF
// PROCESS (INFRA-M7a): OMS_VENUE_ENDPOINTS maps each MIC to an adapter, dialed
// over mTLS as an execution.GRPCVenue.
//
// There are no build tags left. There is no Binance code and no OKX code in this
// binary — no vendor SDK, no request signing, no exchange websocket. The OMS
// cannot reach an exchange except through an adapter, which is the entire point:
// a crash or a compromise in vendor client code no longer shares an address space
// with the process that owns order state.
//
// The SimVenue fallback survives for tests and local dev, and ONLY for that. If it
// is ever reached in production the OMS is filling orders against nothing, so it
// says so at WARN in as many words, and the router hard-errors on any MIC it has
// no venue for rather than quietly routing there.
func configuredVenues(ctx context.Context, cfg config.Config, store order.Store, producer execution.Publisher, counters venueCapabilityCounters, posture venuePostureGauges, logger *slog.Logger) ([]execution.Venue, []execution.VenueInstrument, func(), error) {
	var venues []execution.Venue

	// INFRA-M7a: out-of-process adapters. These need no build tag and link no
	// vendor code — the OMS speaks venue.v1 over mTLS and never imports an
	// exchange SDK. They are the path that retires the tags above.
	grpcVenues, catalogue, closeConns, err := dialVenues(ctx, cfg, counters, logger)
	if err != nil {
		// A configured venue that will not dial is FATAL, not a degradation. The
		// alternative is booting without it and silently routing its orders
		// nowhere — or worse, to the simulator below.
		logger.Error("venue adapter dial failed", "err", err)
		return nil, nil, nil, err
	}
	venues = append(venues, grpcVenues...)

	if len(venues) == 0 {
		// THIS OMS IS A SIMULATOR. It will accept orders and fill them against
		// nothing. That is correct for tests and local dev and catastrophic in
		// production, so it is stated at WARN, not buried at Info.
		//
		// OMS_SIM_VENUE_MIC takes a LIST ("XNAS,XLON"), one SimVenue per MIC. This
		// platform is multi-venue by construction: an allocation fans one signal out
		// across venues and stamps each leg with its target MIC, and the router sends
		// a leg only to the venue whose MIC matches. A simulator that can be exactly
		// ONE venue therefore cannot simulate this platform's own core loop — every
		// leg addressed to any other MIC is refused. That is why the M1 end-to-end
		// loop certification (webhook-ingest's TestIntegration_LoopOverNATS, which
		// fans out to XNAS and XLON) has never once passed: with a single XSIM venue
		// both legs were unroutable, so no fills ever came back.
		mics := parseMICs(cfg.SimVenueMIC)
		logger.Warn("NO REAL VENUES CONFIGURED — every order will be filled by an in-process SimVenue and NOTHING will reach an exchange",
			"sim_mics", mics,
			"fix", "set OMS_VENUE_ENDPOINTS (e.g. XBIN=venue-binance.kanz-services.svc:9000)")
		sims := make([]execution.Venue, 0, len(mics))
		for _, mic := range mics {
			sims = append(sims, execution.NewSimVenue(mic))
		}
		// THE POSTURE, AS A NUMBER (#865). The WARN above is the operator's
		// sentence; these are what an alert, a dashboard and the write-path load
		// harness can read. Both are set on BOTH arms so the family always carries
		// a series — see venuePostureGauges.
		posture.liveVenueAdapters.Set(0)
		posture.simulatedVenues.Set(float64(len(sims)))
		// A simulator trades nothing real, so it contributes nothing to a catalogue a
		// person picks pairs from. An empty list here is the honest answer.
		return sims, nil, closeConns, nil
	}
	posture.liveVenueAdapters.Set(float64(len(venues)))
	posture.simulatedVenues.Set(0)
	return venues, catalogue, closeConns, nil
}

// parseMICs splits the sim-venue MIC list ("XNAS,XLON"). An empty setting yields
// one unnamed SimVenue, which is the historical single-venue behaviour.
func parseMICs(s string) []string {
	var out []string
	for _, mic := range strings.Split(s, ",") {
		if mic = strings.TrimSpace(mic); mic != "" {
			out = append(out, mic)
		}
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// dialVenues turns OMS_VENUE_ENDPOINTS ("XBIN/binance-main=host:port,XOKX/okx-sub-1=host:port")
// into execution.GRPCVenue clients and returns a func that closes them.
//
// The key is MIC/account. An adapter deployment holds one API credential and is
// therefore exactly ONE exchange account — the pool its fills margin against. Naming
// it is what makes segregation possible: a portfolio is bound to an account, and the
// router will not send its orders to an adapter holding a different one.
//
// The dial is mTLS when a SPIFFE socket is configured (SEC-01a: the workload's
// SVID is the credential, authorized against the mesh). Without one it is
// PLAINTEXT, which is a dev-only posture and says so loudly — this connection
// carries live orders to a live exchange, and an unauthenticated peer on it can
// submit trades.
//
// AND THE ACCOUNT IN THAT STRING IS NOT BELIEVED (SOV-02a). Every adapter is ASKED
// who it is (venue.v1.Describe) before it is registered, and one that answers with a
// different account than it was declared as STOPS THE OMS FROM STARTING. The adapter
// holds the API credential; this manifest holds a human's typing. When they disagree,
// the credential is what the exchange will act on — the orders would margin against
// the account the adapter really holds while the ledger booked them to the one named
// here, which is the exact failure EXEC-M16 exists to prevent, arriving through the
// one door EXEC-M16 left open.
func dialVenues(ctx context.Context, cfg config.Config, counters venueCapabilityCounters, logger *slog.Logger) ([]execution.Venue, []execution.VenueInstrument, func(), error) {
	endpoints := parseSymbolMap(cfg.VenueEndpoints) // MIC → address; same "K=V,K=V" form
	if len(endpoints) == 0 {
		return nil, nil, func() {}, nil
	}

	dialOpt := grpc.WithTransportCredentials(insecure.NewCredentials())
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("venue mTLS source: %w", err)
		}
		dialOpt = transport.ClientDialOption(src, transport.AuthorizeMesh())
		logger.Info("venue adapters: mTLS enabled", "venues", len(endpoints))
	} else {
		logger.Warn("VENUE ADAPTERS ARE PLAINTEXT — no SPIFFE_ENDPOINT_SOCKET. This link carries live orders; anyone who can reach it can trade",
			"venues", len(endpoints))
	}

	var (
		venues    []execution.Venue
		catalogue []execution.VenueInstrument
		conns     []*grpc.ClientConn
	)
	closeConns := func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}
	for key, addr := range endpoints {
		// The endpoint key is MIC or MIC/account. THE ACCOUNT IS THE COLLATERAL
		// BOUNDARY: this adapter holds ONE API credential, so everything it fills
		// margins against ONE exchange account, whichever portfolio the order was for.
		// Declaring it here is what lets a portfolio be bound to it — and what lets the
		// router refuse to send another portfolio's order to it.
		mic, account, ok := strings.Cut(key, "/")
		if !ok || account == "" {
			// The venue is its own account: every portfolio trading here shares one
			// collateral pool. That may be true and fine — one account is the normal
			// case — but it must be a stated fact, not an absence.
			account = mic
			logger.Warn("venue adapter declares no account — treating the venue as ONE account, shared by every portfolio that trades it. An exchange liquidates per account",
				"mic", mic, "fix", fmt.Sprintf("OMS_VENUE_ENDPOINTS=%s/<account>=%s", mic, addr))
		}
		conn, err := grpc.NewClient(addr, dialOpt)
		if err != nil {
			closeConns()
			return nil, nil, nil, fmt.Errorf("venue %s at %s: %w", mic, addr, err)
		}
		conns = append(conns, conn)
		venue := execution.NewGRPCVenue(mic, account, conn, cfg.Tenant)

		// ASK THE ADAPTER WHO IT IS, before it is allowed to take an order.
		//
		// grpc.NewClient is lazy, so this is also the first time the connection is
		// actually made — and that is deliberate. An adapter the OMS cannot reach is
		// one whose account it cannot confirm, and a venue that will not answer is
		// already FATAL here (see configuredVenues): booting without it would route
		// its orders nowhere, or to the simulator. The call is BOUNDED (the EXEC-M10
		// stance) so an adapter that has not scheduled yet produces a named error and
		// a kubelet restart, never a process hanging silently at startup.
		dctx, cancel := context.WithTimeout(ctx, venueDescribeTimeout)
		id, err := venue.Describe(dctx)
		cancel()
		if err != nil {
			closeConns()
			return nil, nil, nil, fmt.Errorf("venue %s at %s: cannot ask the adapter which exchange account it holds "+
				"(is it running?): %w", mic, addr, err)
		}
		if err := execution.VerifyIdentity(mic, account, id); err != nil {
			closeConns()
			return nil, nil, nil, fmt.Errorf("venue adapter at %s is not who OMS_VENUE_ENDPOINTS says it is: %w", addr, err)
		}

		switch {
		case id.Proof.Verified:
			logger.Info("venue adapter registered — account PROVEN against the exchange",
				"mic", mic, "account", account, "exchange_account_id", id.Proof.ExchangeAccountID, "endpoint", addr)
		case cfg.RequireVerifiedAccount:
			closeConns()
			return nil, nil, nil, fmt.Errorf("venue %s at %s: account %q is UNVERIFIED — the adapter has not proved its "+
				"credential belongs to it, and OMS_REQUIRE_VERIFIED_ACCOUNT=true. Bind the exchange account id at the "+
				"adapter (e.g. BINANCE_VENUE_ACCOUNT_UID) so it can prove itself, or unset the requirement",
				mic, addr, account)
		default:
			// The adapter agrees with the manifest but nobody has checked it against the
			// exchange. Both mis-configured and correct deployments look like this, so it
			// must not be silent: it is named, and it is counted.
			counters.unverifiedAccounts.Inc()
			logger.Warn("venue adapter registered with an UNVERIFIED account — nobody has confirmed this API credential "+
				"belongs to the collateral pool it names. An exchange liquidates per account",
				"mic", mic, "account", account, "endpoint", addr,
				"fix", "bind the exchange account id at the adapter (e.g. BINANCE_VENUE_ACCOUNT_UID), then set OMS_REQUIRE_VERIFIED_ACCOUNT=true")
		}
		// AND WHICH ORDER TYPES IT CAN PLACE (#405). Same posture as the account
		// above, one field over: order.v1 declares four types, the spot adapters
		// translate two, and before Describe carried this the OMS had no way to
		// ask — so a stop was admitted, stored, announced, and refused only inside
		// the adapter, after the estate had been told the order existed.
		//
		// WithOrderTypes is what arms the admission gate for this MIC. An empty
		// declaration leaves the venue unwrapped and the gate open, because empty
		// means "did not say" and refusing every order for an adapter that predates
		// the field would turn a schema addition into a trading outage.
		switch {
		case id.DeclaresOrderTypes():
			logger.Info("venue adapter declares its order types — unroutable types will be refused at admission",
				"mic", mic, "order_types", id.OrderTypes)
		case cfg.RequireOrderTypeSupport:
			closeConns()
			return nil, nil, nil, fmt.Errorf("venue %s at %s: the adapter did not declare which order types it can "+
				"place, and OMS_REQUIRE_ORDER_TYPE_SUPPORT=true. Without it this OMS will admit an order type "+
				"the adapter cannot translate and only the exchange will refuse it. Upgrade the adapter so its "+
				"Describe reports supported_order_types, or unset the requirement", mic, addr)
		default:
			// The adapter said nothing, which is not the same as "supports nothing"
			// — and both an old adapter and a broken one look like this. Named, and
			// counted, exactly as the unverified account above.
			counters.undeclaredOrderTypes.Inc()
			logger.Warn("venue adapter declared NO order types — the OMS cannot refuse an unroutable order type "+
				"at admission for this venue, so one will be accepted, announced, and fail at the exchange",
				"mic", mic, "account", account, "endpoint", addr,
				"fix", "upgrade the adapter so venue.v1.Describe reports supported_order_types, then set OMS_REQUIRE_ORDER_TYPE_SUPPORT=true")
		}

		// AND WHICH TIME-IN-FORCE INSTRUCTIONS IT CAN EXPRESS (#486). Same posture
		// again, and the defect it closes was worse than the two above: those
		// produced an order that did NOTHING, while a time-in-force the adapter
		// could not express produced an order that did the WRONG THING — an IOC
		// sent as good-til-cancelled RESTS, so a trader who asked to hold no
		// exposure holds it.
		//
		// Reported but NOT fatal on its own, even under RequireOrderTypeSupport:
		// an adapter may answer one capability question and not the other, and
		// refusing to start over the newer field would make upgrading the adapters
		// an all-or-nothing step.
		if id.DeclaresTimeInForce() {
			logger.Info("venue adapter declares its time-in-force set — instructions it cannot express "+
				"will be refused at admission", "mic", mic, "time_in_force", id.TimeInForce)
		} else {
			// ITS OWN COUNTER, NOT THE ORDER-TYPE ONE. They are different gaps with
			// different fixes, and a counter named ..._order_types_total that also
			// counts time-in-force gaps tells an operator two adapters failed to
			// declare order types when one of them declared them fine.
			counters.undeclaredTimeInForce.Inc()
			logger.Warn("venue adapter declared NO time-in-force support — the OMS cannot refuse an "+
				"inexpressible time-in-force at admission for this venue, so an order will be accepted, "+
				"announced, and refused by the connector",
				"mic", mic, "account", account, "endpoint", addr,
				"fix", "upgrade the adapter so venue.v1.Describe reports supported_time_in_force")
		}

		// AND WHICH COLLATERAL REGIMES IT CAN EXPRESS (#417/#742). The fifth
		// member of this family, and until now the only one with NO signal at
		// all: DeclaresMarginModes() existed and nothing called it, so an adapter
		// with a silently open margin gate looked exactly like one that had been
		// checked and was fine. "Nothing configured" and "checked, and fine" must
		// never look the same, and here they did.
		//
		// ITS CONSEQUENCE IS THE WORST OF THE THREE. An undeclared order type
		// produces an order that does nothing; an inexpressible time-in-force
		// produces one that does the wrong thing; an unrefused collateral regime
		// produces a REAL position whose regime the fund's records get wrong —
		// margin and buying power reserved against leverage the exchange never
		// applied, and no downstream record can tell.
		//
		// Reported and counted, NOT fatal, and deliberately with no
		// OMS_REQUIRE_MARGIN_MODE_SUPPORT beside it: #486 declined that second
		// switch on the same reasoning, and a third would be a fourth answer to
		// one question an operator already has two ways to ask.
		if id.DeclaresMarginModes() {
			logger.Info("venue adapter declares its collateral regimes — modes it cannot express "+
				"will be refused at admission", "mic", mic, "margin_modes", id.MarginModes)
		} else {
			counters.undeclaredMarginModes.Inc()
			logger.Warn("venue adapter declared NO margin modes — the OMS cannot refuse an "+
				"inexpressible collateral regime at admission for this venue, so a levered order is "+
				"accepted and announced, and is then either refused by the connector or placed as "+
				"SPOT while the audit root records margin",
				"mic", mic, "account", account, "endpoint", addr,
				"fix", "upgrade the adapter so venue.v1.Describe reports supported_margin_modes")
		}

		// AND WHAT IT CAN TRADE (#406) — asked once, here, and held. The set is
		// deploy-time configuration (BINANCE_SYMBOLS / OKX_SYMBOLS), so serving it
		// from memory is not a staleness risk: changing it requires redeploying the
		// adapter, which the OMS already notices because it re-asks on the next dial.
		//
		// It also keeps a pair picker off the adapters' critical path — one
		// unreachable venue must not take down a screen that only lists names.
		//
		// A VENUE THAT LISTS NOTHING IS NOT FATAL. It can route no orders, which is
		// a real and visible state (its orders are refused at admission by the
		// router), but it is not a reason to refuse to start: an adapter deployed
		// ahead of its symbol map is a rollout in progress, not a corrupt one.
		lctx, lcancel := context.WithTimeout(ctx, venueDescribeTimeout)
		instruments, lerr := venue.ListInstruments(lctx)
		lcancel()
		switch {
		case lerr != nil:
			logger.Warn("venue adapter cannot list its instruments — this venue's pairs will be missing from every "+
				"picker, and a user will read that as the venue offering nothing",
				"mic", mic, "endpoint", addr, "err", lerr)
		case len(instruments) == 0:
			logger.Warn("venue adapter lists NO instruments — it holds no symbol map, so it can route nothing",
				"mic", mic, "endpoint", addr,
				"fix", "set the adapter's symbol map (e.g. BINANCE_SYMBOLS=BTC-USD=BTCUSDT)")
		default:
			logger.Info("venue adapter instruments loaded", "mic", mic, "instruments", len(instruments))
			for _, in := range instruments {
				catalogue = append(catalogue, execution.VenueInstrument{MIC: mic, InstrumentSymbol: in})
			}
		}

		// ALL THREE DECLARATIONS ARM THE SAME GATE. Each wrapper is a no-op when
		// its list is empty, so an adapter that answered one question and not the
		// others is gated on exactly what it answered — which is what lets #417's
		// margin-mode declaration land beside two that predate it without
		// refusing a single order any existing adapter can already place.
		venues = append(venues,
			execution.WithMarginModes(
				execution.WithTimeInForce(
					execution.WithOrderTypes(venue, id.OrderTypes), id.TimeInForce), id.MarginModes))
	}
	// Ordered by (instrument, venue) so the catalogue is stable between boots —
	// OMS_VENUE_ENDPOINTS is a map, and Go randomises its iteration.
	sort.Slice(catalogue, func(i, j int) bool {
		if catalogue[i].InstrumentID != catalogue[j].InstrumentID {
			return catalogue[i].InstrumentID < catalogue[j].InstrumentID
		}
		return catalogue[i].MIC < catalogue[j].MIC
	})
	return venues, catalogue, closeConns, nil
}

// parseSymbolMap parses "BTC-USD=BTCUSDT,ETH-USD=ETHUSDT" into a map.
func parseSymbolMap(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

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
func configuredVenues(ctx context.Context, cfg config.Config, store order.Store, producer execution.Publisher, unverified, undeclared prometheus.Counter, logger *slog.Logger) ([]execution.Venue, []execution.VenueInstrument, func(), error) {
	var venues []execution.Venue

	// INFRA-M7a: out-of-process adapters. These need no build tag and link no
	// vendor code — the OMS speaks venue.v1 over mTLS and never imports an
	// exchange SDK. They are the path that retires the tags above.
	grpcVenues, catalogue, closeConns, err := dialVenues(ctx, cfg, unverified, undeclared, logger)
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
		// A simulator trades nothing real, so it contributes nothing to a catalogue a
		// person picks pairs from. An empty list here is the honest answer.
		return sims, nil, closeConns, nil
	}
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
func dialVenues(ctx context.Context, cfg config.Config, unverified, undeclared prometheus.Counter, logger *slog.Logger) ([]execution.Venue, []execution.VenueInstrument, func(), error) {
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
			unverified.Inc()
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
			undeclared.Inc()
			logger.Warn("venue adapter declared NO order types — the OMS cannot refuse an unroutable order type "+
				"at admission for this venue, so one will be accepted, announced, and fail at the exchange",
				"mic", mic, "account", account, "endpoint", addr,
				"fix", "upgrade the adapter so venue.v1.Describe reports supported_order_types, then set OMS_REQUIRE_ORDER_TYPE_SUPPORT=true")
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

		venues = append(venues, execution.WithOrderTypes(venue, id.OrderTypes))
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

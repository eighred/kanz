package authz_test

// THE ARCH TEST IS THE DELIVERABLE (SEC-M2).
//
// The routes are easy to get right today and easy to get wrong in six months, when someone
// adds `POST /v1/orders/{id}/amend` and registers it next to its neighbours without thinking
// about who may call it. A capability model that depends on remembering to apply it is not a
// control — it is a convention, and conventions decay silently.
//
// So: the capability is a REQUIRED PARAMETER of registration (the compiler enforces that
// every route declares one), and these tests enforce that the declaration is the RIGHT one.
// A route with a capital effect that anyone but a trader can reach fails the build.

import (
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"net/http"
	"testing"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/orders"
	"github.com/eighred/kanz/services/api-gateway/internal/proxy"
)

// TestEveryRouteOnTheCapitalPathRequiresTRADE.
//
// services/api-gateway/internal/orders is THE capital path: every route it serves submits or
// cancels an order at a live exchange. Rather than listing those routes (a list someone must
// remember to extend), this asserts the property over WHATEVER the package registers — so a
// route added there tomorrow is covered by this test the moment it exists.
func TestEveryRouteOnTheCapitalPathRequiresTRADE(t *testing.T) {
	m := authz.NewMux(nil, nil)
	orders.New(nil).Routes(m)

	routes := m.Routes()
	if len(routes) == 0 {
		t.Fatal("the orders handler registered NO routes — this test would pass vacuously and guard nothing")
	}
	for _, r := range routes {
		if r.Capability != authz.Trade {
			t.Errorf("%s requires %q, want %q — it submits or cancels an order at a live exchange, "+
				"and anyone holding a read token could call it",
				r.Pattern, r.Capability, authz.Trade)
		}
	}
}

// TestTheWholeRouteTableIsDeclared is the golden table.
//
// Every /v1 route the gateway serves, and the capability it demands. A NEW route — anywhere,
// in any handler package — fails this test until someone writes it down here, which is the
// point: the decision "who may call this?" is made deliberately, once, in the open, rather
// than inherited by accident from whichever mux the handler happened to be registered on.
func TestTheWholeRouteTableIsDeclared(t *testing.T) {
	m := authz.NewMux(nil, nil)
	// A NON-NIL ORDERS CLIENT, DELIBERATELY. gateway.Routes registers the order
	// history only when one is configured, so passing nil here would let that
	// route escape this table entirely — the guard would pass by not looking.
	gateway.New(nil, stubOrders{}, stubInstruments{}, nil).Routes(m)
	orders.New(nil).Routes(m)
	// A NON-EMPTY FUND ROLE, for the same reason stubOrders is a real value: since
	// #535 the cash-movement route is registered only when the deployment names a
	// funder, so passing "" here would drop it out of this table silently and the
	// guard would pass by not looking at the one route that moves the fund's cash.
	proxy.New(nil, "kanz-treasury").Routes(m)

	want := map[string]authz.Capability{
		// Risk queries. A scenario is a POST, but it computes a what-if and moves no
		// capital — the HTTP verb is not the authority on effect.
		// The LIST is Read, and the guard's own prompt — "decide whether a read
		// token should be able to call it" — is worth answering rather than
		// waving through. It discloses strictly less than the three routes below
		// it: names and a staleness stamp, no positions, no valuations, no
		// money. What it discloses that they do not is the SET of ids, and that
		// is gated twice over — by owner_tenant through writeOwned, and per row
		// by the caller's portfolios claim (#399).
		"GET /v1/portfolios":                authz.Read,
		"GET /v1/portfolios/{id}/exposure":  authz.Read,
		"GET /v1/portfolios/{id}/measures":  authz.Read,
		"POST /v1/portfolios/{id}/scenario": authz.Read,
		// ORDER HISTORY IS A READ, and the guard's prompt is worth answering: it
		// moves no capital and places nothing — the OMS's surface accepts no
		// order, deliberately, so that admission and the compliance gate stay on
		// the bus path. It is the most IDENTIFYING of these reads, naming
		// instruments, sizes and times, which is why it is the one portfolio
		// route that also consults the caller's portfolios claim (#399).
		"GET /v1/portfolios/{id}/orders": authz.Read,
		// THE TRADEABLE-PAIR CATALOGUE IS A READ, and answering the guard's prompt:
		// it is the LEAST disclosing route in this table. It reports which pairs
		// this deployment's venue adapters are configured for — no positions, no
		// sizes, no valuations, and nothing about any portfolio. It is also the
		// only route here that is not portfolio-scoped, because there is no
		// per-portfolio answer to the question: the answer is a property of the
		// deployment. The tenant gate still applies through writeOwned (#406).
		"GET /v1/instruments": authz.Read,
		"GET /v1/health":      authz.Read,

		// THE CAPITAL PATH.
		"POST /v1/orders":             authz.Trade,
		"POST /v1/orders/{id}/cancel": authz.Trade,

		// MODEL PORTFOLIOS (#409), and the guard's prompt is the whole point here.
		//
		// propose is a READ: it optimizes against numbers supplied in the request
		// and moves nothing, the same reasoning that makes the scenario route a
		// read.
		//
		// orders is TRADE, and unconditionally so. It emits the order commands for
		// a proposal, and in a deployment with auto-publish enabled it puts them on
		// the bus. The capability describes what the route DOES in its most
		// permissive configuration — never what one deployment's environment
		// variable currently allows — because a route whose required capability
		// changes with a config flag is one nobody can reason about, and a read
		// token must not reach a surface that in some deployment moves capital.
		"POST /v1/model-portfolios/propose": authz.Read,
		"POST /v1/model-portfolios/orders":  authz.Trade,

		// THE FUNDING PATH (#415) — the fund's OWN capital, not the market's.
		//
		// authz.Fund and not authz.Trade, and the guard's prompt is exactly the
		// question to answer here: should a read token reach this? No — it WRITES
		// the book of record. Should a TRADE token? Also no, and that is the less
		// obvious half. The person who can move money is never the person who
		// trades it; giving Trade this route hands every strategy operator the
		// authority to book a redemption against the IBOR.
		"POST /v1/portfolios/{id}/cash-movements": authz.Fund,

		// Reference + wealth reads.
		"GET /v1/households/{id}": authz.Read,
		"GET /v1/securities/{id}": authz.Read,
		"GET /v1/prices/{id}":     authz.Read,
		"GET /v1/exceptions":      authz.Read,
		"POST /v1/ask":            authz.Read,

		// The TradingView Broker API: reads of the fund's own book.
		"GET /v1/broker/accounts":                 authz.Read,
		"GET /v1/broker/accounts/{id}/state":      authz.Read,
		"GET /v1/broker/accounts/{id}/positions":  authz.Read,
		"GET /v1/broker/accounts/{id}/orders":     authz.Read,
		"GET /v1/broker/accounts/{id}/executions": authz.Read,
	}

	got := map[string]authz.Capability{}
	for _, r := range m.Routes() {
		got[r.Pattern] = r.Capability
	}

	for pattern, cap := range got {
		switch w, ok := want[pattern]; {
		case !ok:
			t.Errorf("UNDECLARED ROUTE %q (requires %q). Add it to this table — and while you are "+
				"here, decide whether a read token should be able to call it", pattern, cap)
		case w != cap:
			t.Errorf("%q requires %q, but this table says %q", pattern, cap, w)
		}
	}
	for pattern := range want {
		if _, ok := got[pattern]; !ok {
			t.Errorf("declared route %q is no longer registered — delete it from this table", pattern)
		}
	}
}

// TestAReadCapabilityCannotBeGrantedTradeByAccident: Grants maps a role to the capabilities
// it carries, and nothing else confers one. In particular, the BASELINE role that admits a
// caller to the gateway at all (SEC-M1's API_GATEWAY_REQUIRED_ROLE) must not smuggle in
// trade — every authenticated caller carries it, so if it did, SEC-M2 would be undone in one
// line of config.
func TestAReadCapabilityCannotBeGrantedTradeByAccident(t *testing.T) {
	g := authz.Grants{"kanz.reader": {authz.Read}}
	if g.Allows([]string{"kanz.reader"}, authz.Trade) {
		t.Fatal("a read-only role was allowed to trade")
	}
	if !g.Allows([]string{"kanz.reader"}, authz.Read) {
		t.Fatal("a read role was not allowed to read")
	}
	if g.Allows(nil, authz.Read) {
		t.Fatal("a principal with NO roles was allowed to read — capabilities must be deny-by-default")
	}
}

// TestRegisteringARouteWithNoCapabilityIsImpossible documents what the compiler already
// guarantees: Handle takes the capability as a REQUIRED parameter, so there is no call that
// registers a route without declaring who may reach it. This test exists to say so out loud
// — the guarantee is the API shape, not a check that runs.
func TestRegisteringARouteWithNoCapabilityIsImpossible(t *testing.T) {
	m := authz.NewMux(nil, nil)
	// m.Handle("GET /v1/thing", h)  // does not compile: missing the capability.
	m.Handle(authz.Read, "GET /v1/thing", func(http.ResponseWriter, *http.Request) {})
	if got := m.Routes(); len(got) != 1 || got[0].Capability != authz.Read {
		t.Fatalf("routes = %+v", got)
	}
}

// stubOrders exists only so gateway.Routes registers the order-history route.
//
// It is a REAL value rather than a typed nil: a nil interface value compares
// equal to nil, so the conditional registration would skip the route and this
// guard would pass by not looking at it — the exact failure mode the table is
// meant to prevent.
type stubOrders struct {
	orderpb.OrderQueryServiceClient
}

// stubInstruments does the same for the tradeable-pair catalogue (#406), and for
// the same reason — the route is registered conditionally, so a nil here would
// quietly remove it from the golden table.
type stubInstruments struct {
	venuepb.VenueQueryServiceClient
}

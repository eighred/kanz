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
	"net/http"
	"testing"

	"github.com/kanz-eng/kanz/services/api-gateway/internal/authz"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/gateway"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/orders"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/proxy"
)

// TestEveryRouteOnTheCapitalPathRequiresTRADE.
//
// services/api-gateway/internal/orders is THE capital path: every route it serves submits or
// cancels an order at a live exchange. Rather than listing those routes (a list someone must
// remember to extend), this asserts the property over WHATEVER the package registers — so a
// route added there tomorrow is covered by this test the moment it exists.
func TestEveryRouteOnTheCapitalPathRequiresTRADE(t *testing.T) {
	m := authz.NewMux(nil)
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
	m := authz.NewMux(nil)
	gateway.New(nil).Routes(m)
	orders.New(nil).Routes(m)
	proxy.New(nil).Routes(m)

	want := map[string]authz.Capability{
		// Risk queries. A scenario is a POST, but it computes a what-if and moves no
		// capital — the HTTP verb is not the authority on effect.
		"GET /v1/portfolios/{id}/exposure":  authz.Read,
		"GET /v1/portfolios/{id}/measures":  authz.Read,
		"POST /v1/portfolios/{id}/scenario": authz.Read,
		"GET /v1/health":                    authz.Read,

		// THE CAPITAL PATH.
		"POST /v1/orders":             authz.Trade,
		"POST /v1/orders/{id}/cancel": authz.Trade,

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
	m := authz.NewMux(nil)
	// m.Handle("GET /v1/thing", h)  // does not compile: missing the capability.
	m.Handle(authz.Read, "GET /v1/thing", func(http.ResponseWriter, *http.Request) {})
	if got := m.Routes(); len(got) != 1 || got[0].Capability != authz.Read {
		t.Fatalf("routes = %+v", got)
	}
}

package authz_test

// SEC-M2 — ONE ROLE FOR EVERYTHING: ANY PRINCIPAL WHO COULD READ COULD TRADE.
//
// The gateway wrapped ALL of /v1 in one chain with one check — middleware.Auth(authn,
// cfg.RequiredRole) — and that check was a single HasRole. So `GET /v1/portfolios/{id}/
// exposure` and `POST /v1/orders` were guarded IDENTICALLY: the token handed to an analyst
// to look at the fund's exposure would submit an order to a live exchange.
//
// SEC-M1 closed the door to strangers. This is about what the people INSIDE may do.
//
// A capability is declared AT REGISTRATION and enforced by the router, so a route with a
// capital effect cannot be reached without the trade capability BY CONSTRUCTION — see
// arch_test.go, which fails the build if anyone adds one that can.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

const (
	roleReader = "kanz.reader"
	roleTrader = "kanz.trader"
)

// grants is the gateway's role → capability policy: the baseline role reads, the trade role
// reads AND trades (a trader can obviously look at what they are trading).
func grants() authz.Grants {
	return authz.Grants{
		roleReader: {authz.Read},
		roleTrader: {authz.Read, authz.Trade},
	}
}

// probe builds a mux with one read route and one route with a capital effect, and serves a
// request as the given principal — exactly as middleware.Auth would have left it on ctx.
func probe(t *testing.T, method, path string, roles ...string) int {
	t.Helper()
	m := authz.NewMux(grants(), nil)
	m.Handle(authz.Read, "GET /v1/portfolios/{id}/exposure", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	m.Handle(authz.Trade, "POST /v1/orders", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	if roles != nil {
		req = req.WithContext(middleware.WithPrincipal(req.Context(),
			&middleware.Principal{Subject: "user-1", Tenant: "acme", Roles: roles}))
	}
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, req)
	return rr.Code
}

// TestAnAnalystCannotTrade is the point of SEC-M2.
func TestAnAnalystCannotTrade(t *testing.T) {
	if code := probe(t, http.MethodGet, "/v1/portfolios/p1/exposure", roleReader); code != http.StatusOK {
		t.Errorf("an analyst could not read exposure: %d, want 200", code)
	}
	if code := probe(t, http.MethodPost, "/v1/orders", roleReader); code != http.StatusForbidden {
		t.Errorf("AN ANALYST SUBMITTED AN ORDER TO A LIVE EXCHANGE: %d, want 403 — "+
			"the token given out to look at exposure also trades", code)
	}
}

// TestATraderCanDoBoth: the capability is a gate, not a wall. A principal who may trade may
// obviously also read — a control that forced traders to hold two tokens would be routed
// around within a week.
func TestATraderCanDoBoth(t *testing.T) {
	if code := probe(t, http.MethodGet, "/v1/portfolios/p1/exposure", roleTrader); code != http.StatusOK {
		t.Errorf("a trader could not read exposure: %d, want 200", code)
	}
	if code := probe(t, http.MethodPost, "/v1/orders", roleTrader); code != http.StatusAccepted {
		t.Errorf("a trader could not trade: %d, want 202", code)
	}
}

// TestAnUnknownRoleGrantsNOTHING: capabilities are deny-by-default. A role nobody has mapped
// carries no capability — it does not fall back to "well, it authenticated, so let it read".
func TestAnUnknownRoleGrantsNOTHING(t *testing.T) {
	if code := probe(t, http.MethodGet, "/v1/portfolios/p1/exposure", "some.other.role"); code != http.StatusForbidden {
		t.Errorf("an unmapped role was granted read: %d, want 403", code)
	}
	if code := probe(t, http.MethodPost, "/v1/orders", "some.other.role"); code != http.StatusForbidden {
		t.Errorf("an unmapped role was granted TRADE: %d, want 403", code)
	}
}

// TestNoPrincipalIsNotAnOversight: the router runs INSIDE middleware.Auth, so a request with
// no principal on ctx means the chain was composed wrong. It must REFUSE, never fall through
// — a capability check that treats "nobody" as "allowed" is worse than no check, because it
// looks like one.
func TestNoPrincipalIsNotAnOversight(t *testing.T) {
	if code := probe(t, http.MethodPost, "/v1/orders"); code != http.StatusForbidden {
		t.Errorf("an unauthenticated request reached a capital route: %d, want 403", code)
	}
}

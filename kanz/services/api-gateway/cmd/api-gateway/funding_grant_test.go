package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"github.com/eighred/kanz/services/api-gateway/internal/orders"
	"github.com/eighred/kanz/services/api-gateway/internal/proxy"
)

// THE GRANT, AT THE ONLY PLACE THAT DECIDES IT (#535).
//
// authz.Fund was declared, demanded by POST /v1/portfolios/{id}/cash-movements,
// and carried by NO ROLE in this composition root's Grants map — three roles for
// four capabilities. Every principal that exists got a 403 on the one route that
// moves the fund's own capital, and a registered, documented, capability-gated
// route is indistinguishable from a working one until somebody tries to move
// cash.
//
// IT IS TESTED HERE AND NOT IN THE authz PACKAGE ON PURPOSE. authz already has
// tests proving a Fund token reaches the route and a Trade token does not — and
// every one of them builds its own Grants map, so all of them passed for the
// entire time the estate granted authz.Fund to nobody. The map that decides is
// the one in buildRouter, and buildRouter had no test at all: this file exercises
// the wiring, which is the layer every unit test in this service skips.

// stubAuthenticator is the token validator seam. It returns a fixed principal, so
// what these cases exercise is the GRANTS MAP and the route table rather than
// token verification (covered by middleware and pkg/auth).
type stubAuthenticator struct {
	principal *middleware.Principal
}

func (s stubAuthenticator) Authenticate(token string) (*middleware.Principal, error) {
	if token == "" {
		return nil, errors.New("no token")
	}
	return s.principal, nil
}

// recordingBackend stands in for the accounting upstream: it answers 202 and
// remembers whether it was reached, so "the capability check let this through"
// is distinguishable from "the route answered for some other reason".
type recordingBackend struct{ called bool }

func (b *recordingBackend) Forward(context.Context, proxy.Request) (proxy.Response, error) {
	b.called = true
	return proxy.Response{Status: http.StatusAccepted, ContentType: "application/json",
		Body: []byte(`{"status":"accepted"}`)}, nil
}

const (
	fundingPath   = "/v1/portfolios/PF1/cash-movements"
	fundingBody   = `{"movement_id":"M1","kind":"subscription","amount":"100"}`
	baselineRole  = "kanz-user"
	traderRole    = "kanz-trader"
	treasuryRole  = "kanz-treasury"
	fundingTenant = "acme"
)

// routerFor builds the REAL router the deployed gateway serves, for a config and
// a caller's roles.
func routerFor(t *testing.T, fundRole string, callerRoles ...string) (http.Handler, *recordingBackend) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	obs, err := observability.New(context.Background(), observability.Config{
		ServiceName: "api-gateway-test", ServiceVersion: version.String(), SampleRatio: 1,
	}, slog.NewTextHandler(io.Discard, nil))
	if err != nil {
		t.Fatalf("observability: %v", err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(context.Background()) })

	cfg := config.Config{
		RequiredRole: baselineRole,
		TradeRole:    traderRole,
		FundRole:     fundRole,
	}
	be := &recordingBackend{}
	var ready atomic.Bool
	router, err := buildRouter(cfg,
		gateway.New(nil, nil, nil, "", logger),
		orders.New(nil, cfg.ApproveRole, halt.OpenGate(nil)),
		proxy.New(be, proxy.Roles{Fund: cfg.FundRole}),
		nil, // no control plane
		obs, &ready, logger,
		nil, // no decision recorder: these cases assert the VERDICT, not the audit trail
		stubAuthenticator{principal: &middleware.Principal{
			Subject: "user:someone", Tenant: fundingTenant, Roles: callerRoles,
		}},
	)
	if err != nil {
		t.Fatalf("buildRouter: %v", err)
	}
	return router, be
}

func fundingPOST(router http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, fundingPath, strings.NewReader(fundingBody))
	req.Header.Set("Authorization", "Bearer a-token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// THE HALF #535 IS ABOUT: a principal holding the fund role REACHES the route.
//
// Without this the estate can ship any number of "a trader cannot fund" tests and
// remain in the state the issue found, where nobody could fund either.
func TestAFundRolePrincipalReachesTheCashMovementRoute(t *testing.T) {
	router, be := routerFor(t, treasuryRole, baselineRole, treasuryRole)

	rr := fundingPOST(router)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s).\n\n"+
			"The composition root's Grants map is the ONLY place a role becomes an authority, and "+
			"authz.Fund appeared in none of its entries — so this route answered 403 to every "+
			"principal that exists while reading as a working control (#535).",
			rr.Code, rr.Body.String())
	}
	if !be.called {
		t.Fatal("the accounting upstream was never reached — the capability check refused before " +
			"the forward, so this test would pass on a 202 produced by something else")
	}
}

// THE SEPARATION, THROUGH THE REAL MAP. The trade role is granted Read and Trade
// here and must NOT pick up Fund: the person who can move money is never the
// person who trades it, and one credential holding both can bring the fund's cash
// in and spend it.
func TestATradeRolePrincipalCannotReachTheCashMovementRoute(t *testing.T) {
	router, be := routerFor(t, treasuryRole, baselineRole, traderRole)

	rr := fundingPOST(router)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a TRADE token funded a portfolio through the deployed "+
			"grants map", rr.Code)
	}
	if be.called {
		t.Fatal("the accounting upstream was reached by a caller holding only the trade role")
	}
}

// The baseline role is carried by EVERY authenticated caller, so if it ever
// picked up Fund the control would be undone in one line of config.
func TestTheBaselineRoleCannotReachTheCashMovementRoute(t *testing.T) {
	router, be := routerFor(t, treasuryRole, baselineRole)

	rr := fundingPOST(router)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — the baseline role reached the funding surface, and every "+
			"authenticated caller carries it", rr.Code)
	}
	if be.called {
		t.Fatal("the accounting upstream was reached by a baseline-role caller")
	}
}

// AND THE UNCONFIGURED DEPLOYMENT ANSWERS 404, NOT 403.
//
// With no API_GATEWAY_FUND_ROLE the route is not registered, so the gateway says
// "there is no cash-movement surface here" — which is true — rather than "you may
// not", which is not, and which sends an operator looking for a role that no
// deployment could grant them. The caller here holds EVERY role the config names,
// so a 404 can only mean the route is absent.
func TestWithNoFundRoleTheCashMovementRouteIs404(t *testing.T) {
	router, be := routerFor(t, "", baselineRole, traderRole)

	rr := fundingPOST(router)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s).\n\n"+
			"An unconfigured funding surface must be ABSENT. Registered-and-ungranted is the third "+
			"state, and it is the defect: 403 to everyone, forever, looking exactly like a control "+
			"that works (#535).", rr.Code, rr.Body.String())
	}
	if be.called {
		t.Fatal("the accounting upstream was reached on a route that must not be registered")
	}
}

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"github.com/eighred/kanz/services/api-gateway/internal/orders"
	"github.com/eighred/kanz/services/api-gateway/internal/proxy"
)

// THE GRANT FOR THE SECOND SIGNATURE, AT THE ONLY PLACE THAT DECIDES IT (#539).
//
// The sibling file states why this layer needs its own tests: authz's own cases
// each build their own Grants map, so all of them passed for the entire time the
// estate granted authz.Fund to nobody. buildRouter is the map that decides.
//
// authz.Approve arrives with that lesson already paid for, so it arrives with
// this file. The four cases are the same four, because they are the ones that
// distinguish a working control from a capability outage wearing its costume.

const (
	approverRole = "kanz-compliance"
	approvePath  = "/v1/exceptions/EX1/override/approve"
	approveBody  = `{"proposal_id":"p1","decision":"approve"}`
)

// approveRouterFor builds the REAL router the deployed gateway serves, for a
// deployment that names approveRole and a caller holding callerRoles.
func approveRouterFor(t *testing.T, approveRole string, callerRoles ...string) (http.Handler, *recordingBackend) {
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
		ApproveRole:  approveRole,
	}
	be := &recordingBackend{}
	var ready atomic.Bool
	router, err := buildRouter(cfg,
		gateway.New(nil, nil, nil, cfg.ApproveRole, logger),
		orders.New(nil, cfg.ApproveRole),
		proxy.New(be, proxy.Roles{Fund: cfg.FundRole, Approve: cfg.ApproveRole}),
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

func approvePOST(router http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, approvePath, strings.NewReader(approveBody))
	req.Header.Set("Authorization", "Bearer a-token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// THE HALF #535 IS ABOUT: a principal holding the approver role REACHES the route.
//
// Every refusal below is satisfied by a route nobody can reach. This is the case
// that separates a control from an outage, and it is the one that was missing
// for authz.Fund.
func TestAnApproverReachesTheOverrideApprovalRoute(t *testing.T) {
	router, be := approveRouterFor(t, approverRole, baselineRole, approverRole)

	rr := approvePOST(router)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("the approver was refused: status = %d, want 202 (%s)", rr.Code, rr.Body.String())
	}
	if !be.called {
		t.Error("the request never reached the datamaster backend — the capability check " +
			"answered before the forward, so this proves nothing about the grant")
	}
}

// A TRADER CANNOT SIGN. The grants map must not fold Approve into the trade role:
// the whole value of the second signature is that the desk that proposes an
// override is not the desk that clears it.
func TestATraderCannotApproveAnOverride(t *testing.T) {
	router, be := approveRouterFor(t, approverRole, baselineRole, traderRole)

	rr := approvePOST(router)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a trade token approved an override: status = %d, want 403", rr.Code)
	}
	if be.called {
		t.Error("the request reached datamaster — the capability check did not stop it")
	}
}

// NOR CAN THE BASELINE ROLE every authenticated principal carries.
func TestTheBaselineRoleCannotApproveAnOverride(t *testing.T) {
	router, _ := approveRouterFor(t, approverRole, baselineRole)

	if rr := approvePOST(router); rr.Code != http.StatusForbidden {
		t.Fatalf("the baseline role approved an override: status = %d, want 403", rr.Code)
	}
}

// A DEPLOYMENT THAT NAMES NO APPROVER ANSWERS 404, NOT 403 (#535).
//
// This is the assertion the funding path did not have until the outage. With
// API_GATEWAY_APPROVE_ROLE unset the capability is carried by nobody, so a
// registered route would refuse every principal that exists and read as a strict
// control. The route must be absent instead, and the status code is the whole
// difference between "there is no override surface in this deployment" — true,
// and actionable — and "you may not", which sends an operator looking for a role
// no deployment could grant them.
func TestWithNoApproverNamedTheOverrideRoutesAreAbsent(t *testing.T) {
	router, be := approveRouterFor(t, "", baselineRole, approverRole)

	rr := approvePOST(router)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("with no approver configured the route answered %d, want 404.\n"+
			"A 403 here is the #535 shape: a capability no role carries, refusing everyone, "+
			"indistinguishable from a control working exactly as designed.", rr.Code)
	}
	if be.called {
		t.Error("an unconfigured deployment forwarded to datamaster")
	}
}

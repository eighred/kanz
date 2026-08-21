package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// WHO MAY READ WHO DID WHAT (#627), AT THE ONLY PLACE THAT DECIDES IT.
//
// These routes had no caller at all: no gateway route, no NetworkPolicy naming
// app: audit, no client in this repository. The only peer that could reach them
// was kanz-observability, through the scrape rule that must admit whatever port
// serves /metrics — and every route takes its tenant from a header a pod there
// sets for itself, against a table deliberately not RLS'd.
//
// So the cases below build the REAL router, as the mandate and funding cases do,
// because a route is not reachable until it is REGISTERED, some role CARRIES the
// capability, and the upstream CLIENT is wired. authz.Fund was granted to nobody
// from #415 to #535 while every unit test in internal/ passed, each having built
// its own Grants map.

const (
	auditRole        = "kanz-compliance-officer"
	auditEventsPath  = "/v1/audit/events"
	auditEventPath   = "/v1/audit/events/evt-1"
	auditLineagePath = "/v1/audit/lineage/evt-1"
	auditReportPath  = "/v1/audit/reports/full-log"
	auditSOC2Path    = "/v1/soc2/evidence"
	auditVerifyPath  = "/v1/audit/verify"
)

// auditRouterFor builds the REAL router for a deployment naming role as its
// audit reader, with a caller holding callerRoles. An empty role is the
// unconfigured posture.
func auditRouterFor(t *testing.T, role string, callerRoles ...string) (http.Handler, *recordingBackend) {
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
		AuditRole:    role,
	}
	be := &recordingBackend{}
	var ready atomic.Bool
	router, err := buildRouter(cfg,
		gateway.New(nil, nil, nil, cfg.ApproveRole, logger),
		orders.New(nil, cfg.ApproveRole, halt.OpenGate(nil)),
		// proxyRoles, NOT a hand-built literal: it is what buildProxy hands the real
		// handler, so a role wired there and forgotten here cannot make this guard
		// certify a route table the deployment does not serve.
		proxy.New(be, proxyRoles(cfg)),
		nil, // no control plane
		obs, &ready, logger,
		nil, // these cases assert the VERDICT, not the audit trail
		stubAuthenticator{principal: &middleware.Principal{
			Subject: "user:someone", Tenant: fundingTenant, Roles: callerRoles,
		}},
	)
	if err != nil {
		t.Fatalf("buildRouter: %v", err)
	}
	return router, be
}

func auditGET(router http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer a-token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func auditPaths() []string {
	return []string{auditEventsPath, auditEventPath, auditLineagePath,
		auditReportPath, auditSOC2Path, auditVerifyPath}
}

// THE HALF #535 IS ABOUT: a principal holding the audit role REACHES all six
// routes, and the request actually arrives at the audit backend.
//
// Every refusal below is satisfied by a route nobody can reach. This is the case
// that separates a control from an outage wearing its costume.
func TestAnAuditReaderReachesAllSixRoutes(t *testing.T) {
	for _, path := range auditPaths() {
		t.Run(path, func(t *testing.T) {
			router, be := auditRouterFor(t, auditRole, baselineRole, auditRole)
			rr := auditGET(router, path)
			if rr.Code == http.StatusNotFound {
				t.Fatalf("GET %s answered 404 for a principal holding %q — the route is not "+
					"registered, so no deployment can expose the compliance record however it is "+
					"configured", path, auditRole)
			}
			if rr.Code == http.StatusForbidden {
				t.Fatalf("GET %s answered 403 for a principal holding %q — authz.Audit is carried "+
					"by no role, which is a total outage of the capability reading as a working "+
					"control (#535)", path, auditRole)
			}
			if !be.called {
				t.Fatalf("GET %s returned %d but never reached the audit backend", path, rr.Code)
			}
		})
	}
}

// THE ASSERTION THE CAPABILITY EXISTS FOR. The baseline role is carried by EVERY
// authenticated caller (config.RequiredRole, SEC-M1), so if these routes were
// mounted on authz.Read every token in the tenant would read the complete record
// of every other principal's actions — which trader was refused by the pre-trade
// gate, who overrode a price, who signed a mandate change.
//
// It is the whole reason authz.Audit is a seventh capability rather than a reuse
// of Read, and it is checked on every route rather than a sample: a single route
// registered under the wrong capability is the exposure.
func TestTheBaselineRoleCannotReadTheAuditTrail(t *testing.T) {
	for _, path := range auditPaths() {
		t.Run(path, func(t *testing.T) {
			router, be := auditRouterFor(t, auditRole, baselineRole)
			rr := auditGET(router, path)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("GET %s answered %d for a principal holding ONLY the baseline role %q — "+
					"every authenticated caller holds it, so this serves the record of every other "+
					"principal's actions to the whole tenant. Want 403", path, rr.Code, baselineRole)
			}
			if be.called {
				t.Fatalf("GET %s reached the audit backend for a baseline-only principal", path)
			}
		})
	}
}

// READING THE RECORD IS NOT A CONSEQUENCE OF ACTING IN IT. A trader moves capital
// all day and every one of those acts is recorded here; the trail exists to be
// read by somebody else.
func TestATraderCannotReadTheAuditTrail(t *testing.T) {
	router, be := auditRouterFor(t, auditRole, baselineRole, traderRole)
	rr := auditGET(router, auditEventsPath)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("GET %s answered %d for a trader. Want 403 — authz.Trade moves capital, it does "+
			"not confer reading the record of who moved it", auditEventsPath, rr.Code)
	}
	if be.called {
		t.Fatalf("GET %s reached the audit backend for a trader", auditEventsPath)
	}
}

// THE UNCONFIGURED POSTURE IS 404, NOT 403 (#535). With no audit reader named,
// the six routes are not registered and the gateway says "there is no audit
// surface here", which is true. A 403 would say the caller lacked a role and send
// them looking for the wrong thing.
func TestWithNoAuditReaderTheAuditRoutesAreAbsent(t *testing.T) {
	for _, path := range auditPaths() {
		t.Run(path, func(t *testing.T) {
			router, be := auditRouterFor(t, "", baselineRole, auditRole)
			rr := auditGET(router, path)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("GET %s answered %d with no API_GATEWAY_AUDIT_ROLE. Want 404: the route "+
					"must not be registered at all", path, rr.Code)
			}
			if be.called {
				t.Fatalf("GET %s reached the audit backend with no audit role configured", path)
			}
		})
	}
}

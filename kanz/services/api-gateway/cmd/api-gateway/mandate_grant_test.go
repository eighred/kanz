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

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"github.com/eighred/kanz/services/api-gateway/internal/orders"
	"github.com/eighred/kanz/services/api-gateway/internal/proxy"
)

// THE GRANT FOR ACT TWO, AT THE ONLY PLACE THAT DECIDES IT (#562, #410 act two).
//
// A ROUTE IS NOT REACHABLE UNTIL THREE THINGS HOLD, and this estate has paid for
// each of them separately (#539): it is REGISTERED, some role CARRIES the
// capability, and the upstream CLIENT is wired. Every one of those is decided in
// buildRouter and buildProxy, which no unit test in internal/ constructs — the
// authz package's own cases each build their own Grants map, which is why all of
// them passed for the entire time the estate granted authz.Fund to nobody.
//
// So the cases below build the REAL router and assert all three, including the
// one that separates a control from a capability outage wearing its costume: a
// principal holding the role REACHES the compliance backend.

const (
	mandateRole        = "kanz-mandate-officer"
	mandateProposePath = "/v1/portfolios/PF1/mandate"
	mandateApprovePath = "/v1/portfolios/PF1/mandate/approve"
	mandateQueuePath   = "/v1/mandates/pending-changes"
	// THE READ OF ONE PROPOSAL'S MANDATE (#606). The id is a real 32-hex proposal
	// id rather than a placeholder, because the path segment is what the mux binds
	// and what the proxy must forward unrewritten.
	mandateChangePath  = "/v1/mandates/pending-changes/9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	mandateProposeBody = `{"mandate":{"mandate_id":"M1","portfolio_id":"PF1","version":1,` +
		`"effective_at":"2026-09-01T00:00:00Z"},"reason":"Q3 mandate"}`
	mandateApproveBody = `{"proposal_id":"p1","decision":"approve"}`
)

// mandateRouterFor builds the REAL router for a deployment that names
// mandateRole, with a caller holding callerRoles.
func mandateRouterFor(t *testing.T, role string, callerRoles ...string) (http.Handler, *recordingBackend) {
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
		// A DIFFERENT NAME FROM THE MANDATE ROLE, deliberately. The two are the
		// halves of one escalation and config.validateAuth refuses to start if they
		// collide — see TestAMandateRoleThatIsAlsoTheApproverIsRefused in
		// internal/config. Setting them equal here would build a router the
		// deployment could never have.
		ApproveRole: approverRole,
		MandateRole: role,
	}
	be := &recordingBackend{}
	var ready atomic.Bool
	router, err := buildRouter(cfg,
		gateway.New(nil, nil, nil, cfg.ApproveRole, logger),
		orders.New(nil, cfg.ApproveRole, halt.OpenGate(nil)),
		// proxyRoles, NOT A HAND-BUILT LITERAL. It is what buildProxy hands the real
		// handler, so a role added there and forgotten here cannot make this guard
		// certify a route table the deployment does not serve.
		proxy.New(be, proxyRoles(cfg)),
		nil, // no control plane
		obs, &ready, logger,
		nil, // no decision recorder: these cases assert the VERDICT, not the audit trail
		stubAuthenticator{principal: &middleware.Principal{
			Subject: "user:someone", Tenant: fundingTenant, Roles: callerRoles,
		}},
		nil, // per-pod idempotency claims: these cases assert routing, not dedup
	)
	if err != nil {
		t.Fatalf("buildRouter: %v", err)
	}
	return router, be
}

func mandatePOST(router http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer a-token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func mandateGET(router http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer a-token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// THE HALF #535 IS ABOUT: a principal holding the mandate role REACHES all four
// routes, and the request actually arrives at the compliance backend.
//
// Every refusal below is satisfied by a route nobody can reach. This is the case
// that separates a control from an outage, and it is the one authz.Fund did not
// have from #415 to #535.
//
// THE FOURTH CASE IS #606'S THIRD REACHABILITY LAYER. compliance registering
// GET /v1/mandates/pending-changes/{proposal_id} on its own mux proves nothing
// about whether any client can call it: the route also has to be fronted here and
// demand a capability somebody holds. #539 cost a full day to the same shape, on
// three layers in one day.
func TestAMandateSignatoryReachesAllFourRoutes(t *testing.T) {
	for _, c := range []struct {
		name, method, path, body string
	}{
		{"propose", http.MethodPost, mandateProposePath, mandateProposeBody},
		{"approve", http.MethodPost, mandateApprovePath, mandateApproveBody},
		{"the queue", http.MethodGet, mandateQueuePath, ""},
		{"the change itself", http.MethodGet, mandateChangePath, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			router, be := mandateRouterFor(t, mandateRole, baselineRole, mandateRole)

			var rr *httptest.ResponseRecorder
			if c.method == http.MethodGet {
				rr = mandateGET(router, c.path)
			} else {
				rr = mandatePOST(router, c.path, c.body)
			}

			if rr.Code != http.StatusAccepted {
				t.Fatalf("the mandate signatory was refused %s: status = %d, want 202 (%s)",
					c.path, rr.Code, rr.Body.String())
			}
			if !be.called {
				t.Error("the request never reached the compliance backend — the capability check " +
					"answered before the forward, so this proves nothing about the grant")
			}
		})
	}
}

// AN APPROVER CANNOT CHANGE A MANDATE, AND THIS IS THE CASE #562 TURNS ON.
//
// #539 predicted act two would be "a third route on the same capability". If it
// were, the person who gives the second signature on a HELD ORDER would also give
// the second signature on relaxing the mandate that held it — sign away the
// limit, then sign the trade the limit existed to stop. Both acts would show two
// names, both would pass their own self-approval check, and nothing anywhere
// compares the two records.
func TestAnOrderApproverCannotChangeAMandate(t *testing.T) {
	router, be := mandateRouterFor(t, mandateRole, baselineRole, approverRole)

	rr := mandatePOST(router, mandateApprovePath, mandateApproveBody)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("an approve token signed a mandate change: status = %d, want 403.\n\n"+
			"That is the escalation #562 was filed on: one signatory relaxes the constraint and "+
			"then clears the order it would have refused, with both acts individually correct "+
			"in the trail", rr.Code)
	}
	if be.called {
		t.Error("the request reached compliance — the capability check did not stop it")
	}
}

// NOR CAN A TRADER. A trader who can change the mandate does not need to break
// the pre-trade gate, only to widen it.
func TestATraderCannotChangeAMandate(t *testing.T) {
	router, be := mandateRouterFor(t, mandateRole, baselineRole, traderRole)

	if rr := mandatePOST(router, mandateProposePath, mandateProposeBody); rr.Code != http.StatusForbidden {
		t.Fatalf("a trade token proposed a mandate change: status = %d, want 403", rr.Code)
	}
	if be.called {
		t.Error("the request reached compliance — the capability check did not stop it")
	}
}

// NOR CAN THE BASELINE ROLE every authenticated principal carries. If it could,
// any two users in the tenant would be the whole control on what the fund may
// hold.
func TestTheBaselineRoleCannotChangeAMandate(t *testing.T) {
	router, _ := mandateRouterFor(t, mandateRole, baselineRole)

	for _, path := range []string{mandateProposePath, mandateApprovePath} {
		if rr := mandatePOST(router, path, mandateProposeBody); rr.Code != http.StatusForbidden {
			t.Errorf("the baseline role reached %s: status = %d, want 403", path, rr.Code)
		}
	}
	if rr := mandateGET(router, mandateQueuePath); rr.Code != http.StatusForbidden {
		t.Errorf("the baseline role read the mandate queue: status = %d, want 403 — it names the "+
			"proposer of every unsigned change to what governs a portfolio", rr.Code)
	}
	// AND NOT THE CHANGE ITSELF (#606). A route that a signatory can reach and a
	// route that EVERYONE can reach are the same route until this assertion runs,
	// and this one carries the rules and the limits rather than the shape.
	if rr := mandateGET(router, mandateChangePath); rr.Code != http.StatusForbidden {
		t.Errorf("the baseline role read a proposed mandate: status = %d, want 403 — it carries "+
			"the RULES and the LIMITS of a change nobody has signed yet", rr.Code)
	}
}

// A DEPLOYMENT THAT NAMES NO MANDATE SIGNATORY ANSWERS 404, NOT 403 (#535).
//
// With API_GATEWAY_MANDATE_ROLE unset the capability is carried by nobody, so a
// registered route would refuse every principal that exists and read as a strict
// control. The status code is the whole difference between "there is no mandate
// surface in this deployment" — true, and actionable — and "you may not", which
// sends an operator looking for a role no deployment could grant them.
func TestWithNoMandateSignatoryTheMandateRoutesAreAbsent(t *testing.T) {
	router, be := mandateRouterFor(t, "", baselineRole, mandateRole)

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, mandateProposePath},
		{http.MethodPost, mandateApprovePath},
		{http.MethodGet, mandateQueuePath},
		{http.MethodGet, mandateChangePath},
	} {
		var rr *httptest.ResponseRecorder
		if c.method == http.MethodGet {
			rr = mandateGET(router, c.path)
		} else {
			rr = mandatePOST(router, c.path, mandateProposeBody)
		}
		if rr.Code != http.StatusNotFound {
			t.Errorf("with no signatory configured %s answered %d, want 404.\n"+
				"A 403 here is the #535 shape: a capability no role carries, refusing everyone, "+
				"indistinguishable from a control working exactly as designed.", c.path, rr.Code)
		}
	}
	if be.called {
		t.Error("an unconfigured deployment forwarded to compliance")
	}
}

// THE THIRD REACHABILITY LAYER: the UPSTREAM CLIENT.
//
// The two cases above cover registration and the grant. This one covers the layer
// that fails differently — buildProxy only puts compliance in the backend's base
// map when API_GATEWAY_COMPLIANCE_ADDR is set, so a deployment can name a real
// signatory, pass every assertion here, and have every mandate route answer 503.
//
// 503 IS THE RIGHT ANSWER AND 403 WOULD NOT BE. "The surface is disabled" and
// "you are not the signatory" are different facts, and an operator can only act
// on the first if they are told it.
func TestAMandateSignatoryOverNoComplianceUpstreamGets503(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	obs, err := observability.New(context.Background(), observability.Config{
		ServiceName: "api-gateway-test", ServiceVersion: version.String(), SampleRatio: 1,
	}, slog.NewTextHandler(io.Discard, nil))
	if err != nil {
		t.Fatalf("observability: %v", err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(context.Background()) })

	cfg := config.Config{
		RequiredRole: baselineRole, TradeRole: traderRole, MandateRole: mandateRole,
	}
	// buildProxy's own no-upstreams branch: a nil backend, the shape a gateway with
	// no addresses configured actually serves.
	var ready atomic.Bool
	router, err := buildRouter(cfg,
		gateway.New(nil, nil, nil, "", logger),
		orders.New(nil, "", halt.OpenGate(nil)),
		proxy.New(nil, proxyRoles(cfg)),
		nil, obs, &ready, logger, nil,
		stubAuthenticator{principal: &middleware.Principal{
			Subject: "user:someone", Tenant: fundingTenant,
			Roles: []string{baselineRole, mandateRole},
		}},
		nil, // per-pod idempotency claims: these cases assert routing, not dedup
	)
	if err != nil {
		t.Fatalf("buildRouter: %v", err)
	}

	rr := mandatePOST(router, mandateProposePath, mandateProposeBody)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s).\n\n"+
			"The route is registered and the caller holds the capability; what is missing is the "+
			"upstream. That is the third reachability layer, and it fails differently from the "+
			"other two", rr.Code, rr.Body.String())
	}
}

// THE PRINCIPAL REACHES COMPLIANCE, AND IT IS THE WHOLE CONTROL.
//
// compliance takes the proposer and the approver off X-Kanz-Principal-Subject.
// If the gateway forwarded without them, both signatures would be anonymous and
// the service would refuse everything — or worse, a later change that defaulted
// the missing subject would let one caller be both. Asserted at the composition
// root because that is where the propagation is wired.
func TestTheMandateRoutesForwardTheAuthenticatedPrincipal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	obs, err := observability.New(context.Background(), observability.Config{
		ServiceName: "api-gateway-test", ServiceVersion: version.String(), SampleRatio: 1,
	}, slog.NewTextHandler(io.Discard, nil))
	if err != nil {
		t.Fatalf("observability: %v", err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(context.Background()) })

	cfg := config.Config{
		RequiredRole: baselineRole, TradeRole: traderRole, MandateRole: mandateRole,
	}
	be := &principalCapturingBackend{}
	var ready atomic.Bool
	router, err := buildRouter(cfg,
		gateway.New(nil, nil, nil, "", logger),
		orders.New(nil, "", halt.OpenGate(nil)),
		proxy.New(be, proxyRoles(cfg)),
		nil, obs, &ready, logger, nil,
		stubAuthenticator{principal: &middleware.Principal{
			Subject: "operator:akif", Tenant: fundingTenant,
			Roles: []string{baselineRole, mandateRole},
		}},
		nil, // per-pod idempotency claims: these cases assert routing, not dedup
	)
	if err != nil {
		t.Fatalf("buildRouter: %v", err)
	}

	if rr := mandatePOST(router, mandateProposePath, mandateProposeBody); rr.Code != http.StatusAccepted {
		t.Fatalf("propose = %d (%s)", rr.Code, rr.Body.String())
	}
	if be.subject != "operator:akif" || be.tenant != fundingTenant {
		t.Fatalf("compliance was handed subject=%q tenant=%q, want %q and %q.\n\n"+
			"compliance names the proposer and the approver from these; without them a mandate "+
			"change is signed by nobody, which is #444's forged-actor defect arriving on the "+
			"surface built to prevent it", be.subject, be.tenant, "operator:akif", fundingTenant)
	}
	if be.service != proxy.ServiceCompliance {
		t.Errorf("the request was forwarded to %q, want %q", be.service, proxy.ServiceCompliance)
	}
	if be.path != mandateProposePath {
		t.Errorf("the upstream path is %q, want %q — the mandate routes are 1:1 with compliance's "+
			"and nothing rewrites them", be.path, mandateProposePath)
	}
}

// principalCapturingBackend records what the proxy actually forwarded.
type principalCapturingBackend struct {
	service         proxy.Service
	path            string
	subject, tenant string
}

func (b *principalCapturingBackend) Forward(_ context.Context, req proxy.Request) (proxy.Response, error) {
	b.service, b.path = req.Service, req.Path
	if req.Principal != nil {
		b.subject, b.tenant = req.Principal.Subject, req.Principal.Tenant
	}
	return proxy.Response{Status: http.StatusAccepted, ContentType: "application/json",
		Body: []byte(`{"status":"PENDING_APPROVAL"}`)}, nil
}

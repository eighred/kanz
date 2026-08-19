package proxy

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// CHANGING A MANDATE IS ITS OWN AUTHORITY (#562, #410 act two).
//
// The three routes below propose, sign and list a change to the constraint every
// order in a portfolio is checked against. The half worth testing is not that a
// mandate signatory reaches them — the composition root proves that — it is which
// OTHER tokens cannot, and the interesting one is authz.Approve.
//
// A capability boundary that is only ever tested from the allowed side is a
// boundary nobody has checked.

const (
	mandateProposeRoute = "POST /v1/portfolios/{id}/mandate"
	mandateApproveRoute = "POST /v1/portfolios/{id}/mandate/approve"
	mandateQueueRoute   = "GET /v1/mandates/pending-changes"
)

func mandateHandler() *Handler {
	return New(&fakeBackend{resp: Response{Status: http.StatusAccepted}},
		Roles{Approve: "compliance", Mandate: "mandate-officer"})
}

func mandateProposeRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/portfolios/PF1/mandate",
		strings.NewReader(`{"mandate":{"mandate_id":"M1"},"reason":"why"}`))
}

// THE ONE THAT DECIDES THE CAPABILITY RULING. #539's text predicted act two would
// be "a third route on the same capability"; it is not, because one signatory
// holding both would sign away a limit and then sign the trade that limit existed
// to stop.
func TestMandateRoutesRefuseAnApproveToken(t *testing.T) {
	h := mandateHandler()
	m := mux("compliance", authz.Read, authz.Approve)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(mandateProposeRequest(), "compliance"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("an APPROVE token proposed a mandate change: status = %d, want 403.\n"+
			"Approve is the second signature on an act somebody else composed, within the policy "+
			"in force. This REWRITES the policy — and one person holding both can relax the "+
			"mandate and then clear the order it would have refused, with each act individually "+
			"correct in the trail (#562)", rr.Code)
	}
}

func TestMandateRoutesRefuseATradeToken(t *testing.T) {
	h := mandateHandler()
	m := mux("trader", authz.Read, authz.Trade)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(mandateProposeRequest(), "trader"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a TRADE token proposed a mandate change: status = %d, want 403.\n"+
			"A trader who can change the mandate does not need to break the pre-trade gate, "+
			"only to widen it.", rr.Code)
	}
}

func TestMandateRoutesRefuseAReadToken(t *testing.T) {
	h := mandateHandler()
	m := mux("reader", authz.Read)
	h.Routes(m)

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/portfolios/PF1/mandate"},
		{http.MethodPost, "/v1/portfolios/PF1/mandate/approve"},
		{http.MethodGet, "/v1/mandates/pending-changes"},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		m.ServeHTTP(rr, as(req, "reader"))
		if rr.Code != http.StatusForbidden {
			t.Errorf("a READ token reached %s: status = %d, want 403.\n"+
				"The queue is the one that looks like a report and is not: it names the proposer "+
				"of every unsigned change to what governs a portfolio, and it is the surface "+
				"somebody signs from", c.path, rr.Code)
		}
	}
}

// WITH NO SIGNATORY NAMED THE ROUTES ARE NOT REGISTERED AT ALL (#535). Registered
// and granted to nobody answers 403 to every principal that exists, which is
// indistinguishable from a control working as intended; unregistered answers 404,
// which is true and actionable.
func TestWithNoMandateSignatoryNoMandateRouteIsRegistered(t *testing.T) {
	h := New(&fakeBackend{resp: Response{Status: http.StatusAccepted}}, Roles{Approve: "compliance"})
	m := authz.NewMux(nil, nil)
	h.Routes(m)

	for _, r := range m.Routes() {
		if strings.Contains(r.Pattern, "mandate") {
			t.Errorf("%q is registered with no API_GATEWAY_MANDATE_ROLE — it demands authz.Mandate, "+
				"which no role carries, so it answers 403 to everybody (#535)", r.Pattern)
		}
	}
}

// AND WITH ONE NAMED, EXACTLY THESE THREE APPEAR. The negative case above passes
// against a handler that registers nothing at all, so it needs its opposite to
// mean anything — and pinning the SET catches a fourth mandate route arriving
// without a decision about who may reach it.
func TestWithASignatoryNamedExactlyThreeMandateRoutesAppear(t *testing.T) {
	h := mandateHandler()
	m := authz.NewMux(nil, nil)
	h.Routes(m)

	var got []string
	for _, r := range m.Routes() {
		if r.Capability == authz.Mandate {
			got = append(got, r.Pattern)
		}
	}
	sort.Strings(got)
	want := []string{mandateQueueRoute, mandateProposeRoute, mandateApproveRoute}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("routes demanding authz.Mandate = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("routes demanding authz.Mandate = %v, want %v", got, want)
		}
	}
}

// EVERY MANDATE ROUTE TARGETS COMPLIANCE, AND ITS OWN LISTENER. A route that
// forwarded to another upstream would carry the principal somewhere that reads
// it for a different decision entirely.
func TestMandateRoutesForwardToCompliance(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: http.StatusAccepted}}
	h := New(be, Roles{Mandate: "mandate-officer"})
	m := mux("mandate-officer", authz.Read, authz.Mandate)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(mandateProposeRequest(), "mandate-officer"))

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rr.Code, rr.Body.String())
	}
	if be.last.Service != ServiceCompliance {
		t.Fatalf("forwarded to %q, want %q", be.last.Service, ServiceCompliance)
	}
	if be.last.Path != "/v1/portfolios/PF1/mandate" {
		t.Errorf("upstream path = %q — the mandate routes are 1:1 with compliance's and nothing "+
			"rewrites them", be.last.Path)
	}
	if be.last.Principal == nil || be.last.Principal.Subject == "" {
		t.Error("the principal did not reach compliance — it names the proposer and the approver " +
			"from that header, so without it a mandate change is signed by nobody")
	}
}

// AN ANONYMOUS REQUEST IS NOT FORWARDED, even on an auth-disabled dev gateway.
// requirePrincipal is the edge check beyond the middleware chain, and it is what
// stops a dev posture from becoming a mandate change with no names on it.
func TestMandateRoutesRefuseAnAnonymousRequest(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: http.StatusAccepted}}
	h := New(be, Roles{Mandate: "mandate-officer"})
	m := authz.NewMux(authz.Grants{"mandate-officer": {authz.Mandate}}, nil)
	h.Routes(m)

	// A principal on the context (so the capability check passes) but with no
	// SUBJECT — the shape an auth-disabled dev gateway produces.
	req := mandateProposeRequest().WithContext(middleware.WithPrincipal(
		mandateProposeRequest().Context(),
		&middleware.Principal{Tenant: "t1", Roles: []string{"mandate-officer"}}))
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", rr.Code, rr.Body.String())
	}
	if be.last.Service != "" {
		t.Error("an anonymous mandate change was forwarded to compliance")
	}
}

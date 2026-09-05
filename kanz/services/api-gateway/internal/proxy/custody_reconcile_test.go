package proxy

// THE AD-HOC CUSTODY COMPARISON HAD NO DOOR (#967, #1025).
//
// accounting has served POST /v1/portfolios/{id}/reconcile since IBOR-01e, and
// this gateway routed none of it. Under network-policies.yaml the gateway is
// accounting's ONLY permitted caller, so "unrouted" meant "reachable by nobody" —
// the shape #539 found in datamaster's maker-checker workflow, and the reason an
// operator who had just folded the missing fill behind a break had no way to ask
// whether the difference was actually gone before the next scheduled run.
//
// Reachability fails at three layers — no route, no role, no client — and each
// repair can reintroduce the bug one layer out. The route and the role are what
// this file holds: registered under authz.Fund, refused to Read and Trade, and
// ABSENT rather than 403 when no funder is named.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
)

const reconcilePattern = "POST /v1/portfolios/{id}/reconcile"

func reconcileRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/portfolios/PF1/reconcile",
		strings.NewReader(`{"custodian_id":"CUST-A","positions":{"AAPL":"100"}}`))
}

// A FUNDER CAN RUN THE COMPARISON, and the request reaches accounting unchanged.
func TestReconcileRouteAdmitsAFundToken(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: http.StatusOK, ContentType: "application/json", Body: []byte(`{"count":0}`)}}
	h := New(be, Roles{Fund: "treasury"})
	m := mux("treasury", authz.Fund)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(reconcileRequest(), "treasury"))

	if rr.Code != http.StatusOK {
		t.Fatalf("a FUND token was refused the comparison: status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if be.last.Service != ServiceAccounting {
		t.Errorf("forwarded to %q, want %q", be.last.Service, ServiceAccounting)
	}
	if be.last.Path != "/v1/portfolios/PF1/reconcile" {
		t.Errorf("upstream path = %q, want the gateway path unchanged", be.last.Path)
	}
	// THE PRINCIPAL MUST TRAVEL. accounting takes the tenant off the header and
	// serves the one tenant its RLS pool is pinned to; an anonymous forward is a
	// comparison over a book nobody established the ownership of, and accounting
	// answers 404 to it.
	if be.last.Principal == nil || be.last.Principal.Tenant != "t1" {
		t.Fatalf("principal forwarded = %+v, want the authenticated caller's tenant", be.last.Principal)
	}
}

// A READ TOKEN MAY NOT, and this is the half worth writing down because the route
// WRITES NOTHING — usually the argument for authz.Read. The response is the full
// set of differences between the fund's book and a named custodian's holdings,
// strictly more disclosing than the break queue beside it, and it is the surface
// an operator acts from while working a break.
func TestReconcileRouteRefusesAReadToken(t *testing.T) {
	h := New(&fakeBackend{resp: Response{Status: http.StatusOK}}, Roles{Fund: "treasury"})
	m := mux("analyst", authz.Read)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(reconcileRequest(), "analyst"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a READ token computed the fund's entire custody position: status = %d, want 403", rr.Code)
	}
}

// NOR A TRADE TOKEN: the person who investigates a custody break is not the
// person who trades the book.
func TestReconcileRouteRefusesATradeToken(t *testing.T) {
	h := New(&fakeBackend{resp: Response{Status: http.StatusOK}}, Roles{Fund: "treasury"})
	m := mux("trader", authz.Trade)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(reconcileRequest(), "trader"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a TRADE token ran a custody comparison: status = %d, want 403", rr.Code)
	}
}

// WITH NO FUNDER NAMED THE ROUTE IS ABSENT — 404, NOT 403 (#535). A route whose
// capability nobody holds refuses every principal that exists, which says "you may
// not" when the truth is "nobody may, in this deployment" — indistinguishable from
// a control working exactly as designed.
func TestReconcileRouteIsNotRegisteredWithoutAFunder(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: http.StatusOK}}
	h := New(be, Roles{}) // no API_GATEWAY_FUND_ROLE
	// A mux granting EVERY capability, so a 404 can only come from the route being
	// absent rather than from a capability refusal.
	m := mux("everything", authz.Read, authz.Trade, authz.Operate, authz.Fund)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(reconcileRequest(), "everything"))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no funder configured", rr.Code)
	}
	if be.last.Service != "" {
		t.Fatalf("the accounting backend was called for an unregistered route")
	}
}

// AND THE OTHER HALF: with a funder named, the route IS registered. Without this
// the absence above is satisfied by a route no configuration can ever bring back.
func TestReconcileRouteIsRegisteredWhenAFunderIsNamed(t *testing.T) {
	m := authz.NewMux(nil, nil)
	New(nil, Roles{Fund: "treasury"}).Routes(m)

	for _, r := range m.Routes() {
		if r.Pattern == reconcilePattern {
			if r.Capability != authz.Fund {
				t.Fatalf("%s requires %q, want %q", r.Pattern, r.Capability, authz.Fund)
			}
			return
		}
	}
	t.Fatalf("%s was not registered even though a fund role is configured — accounting serves it and "+
		"this gateway is its only permitted caller, so it is reachable by nobody", reconcilePattern)
}

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// FUNDING IS ITS OWN AUTHORITY (#415).
//
// POST /v1/portfolios/{id}/cash-movements is the one route on this gateway that
// moves the FUND'S OWN capital rather than the market's: a subscription, a
// redemption or a fee, posted to the book of record.
//
// It sits behind authz.Fund, and the half worth testing is not that a funder can
// reach it — it is that a TRADER CANNOT. The person who can move money is never
// the person who trades it; that is the oldest segregation of duties in fund
// operations, and it is the control an auditor asks about first. Collapsing it
// into Trade would hand every strategy operator the authority to book a
// redemption against the IBOR.
//
// A capability boundary that is only ever tested from the allowed side is a
// boundary nobody has checked.

func mux(role string, caps ...authz.Capability) *authz.Mux {
	return authz.NewMux(authz.Grants{role: caps}, nil)
}

func as(req *http.Request, roles ...string) *http.Request {
	return req.WithContext(middleware.WithPrincipal(req.Context(),
		&middleware.Principal{Subject: "u1", Tenant: "t1", Roles: roles}))
}

func fundingRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/portfolios/PF1/cash-movements",
		strings.NewReader(`{"movement_id":"M1","kind":"subscription","amount":"100"}`))
}

func TestFundingRouteRefusesATradeToken(t *testing.T) {
	h := New(&fakeBackend{resp: Response{Status: http.StatusAccepted}}, "treasury")
	m := mux("trader", authz.Trade)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(fundingRequest(), "trader"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a TRADE token funded a portfolio: status = %d, want 403.\n"+
			"Trade moves the market's capital; this moves the fund's. One credential holding "+
			"both can bring cash in and spend it, which is the control every auditor of a fund "+
			"asks about first.", rr.Code)
	}
}

func TestFundingRouteRefusesAReadToken(t *testing.T) {
	h := New(&fakeBackend{resp: Response{Status: http.StatusAccepted}}, "treasury")
	m := mux("analyst", authz.Read)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(fundingRequest(), "analyst"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a READ token wrote the book of record: status = %d, want 403", rr.Code)
	}
}

// AND A FUNDER CAN. Without this the refusals above are satisfied by a route
// nobody can reach — a funding outage wearing the shape of a control.
func TestFundingRouteAdmitsAFundToken(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: http.StatusAccepted, ContentType: "application/json", Body: []byte(`{"status":"accepted"}`)}}
	h := New(be, "treasury")
	m := mux("treasury", authz.Fund)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(fundingRequest(), "treasury"))

	if rr.Code != http.StatusAccepted {
		t.Fatalf("a FUND token was refused: status = %d, want 202 (%s)", rr.Code, rr.Body.String())
	}
	if be.last.Service != ServiceAccounting {
		t.Errorf("forwarded to %q, want %q", be.last.Service, ServiceAccounting)
	}
	// The path is NOT rewritten: the gateway path IS the upstream path, as it is
	// for every other route on this handler.
	if be.last.Path != "/v1/portfolios/PF1/cash-movements" {
		t.Errorf("upstream path = %q, want the gateway path unchanged", be.last.Path)
	}
	// THE PRINCIPAL MUST TRAVEL. accounting takes the tenant off the header and
	// serves the one tenant its RLS pool is pinned to; forwarding without one is a
	// cash movement nobody signed, and accounting would refuse it 404.
	if be.last.Principal == nil || be.last.Principal.Tenant != "t1" {
		t.Fatalf("principal forwarded = %+v, want the authenticated caller's tenant", be.last.Principal)
	}
}

// WITH NO FUNDER NAMED, THE ROUTE IS ABSENT — 404, NOT 403 (#535).
//
// authz.Fund is carried by no role unless API_GATEWAY_FUND_ROLE names one. A
// registered route whose capability nobody holds refuses EVERY principal that
// exists, and "you may not" is a false answer when the truth is "nobody may, in
// this deployment" — from outside it is indistinguishable from a control working
// exactly as designed, which is how it survived from #415 to #535.
//
// THE STATUS CODE IS THE ASSERTION. 404 says there is no cash-movement surface
// here, which is true and actionable; 403 sends the operator looking for a role
// that no deployment could ever grant them.
func TestFundingRouteIsNotRegisteredWithoutAFunder(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: http.StatusAccepted}}
	h := New(be, "") // no API_GATEWAY_FUND_ROLE
	// A mux that grants EVERY capability, including Fund, so a 404 here can only
	// come from the route being absent — never from a capability refusal.
	m := mux("everything", authz.Read, authz.Trade, authz.Operate, authz.Fund)
	h.Routes(m)

	for _, r := range m.Routes() {
		if r.Capability == authz.Fund {
			t.Fatalf("the funding route %q was registered with no funder configured", r.Pattern)
		}
	}

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(fundingRequest(), "everything"))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. With no role able to carry authz.Fund the route must not "+
			"be registered at all — a 403 to every principal that exists reads as a working "+
			"control and is a total outage of the capability (#535)", rr.Code)
	}
	if be.last.Service != "" {
		t.Fatalf("the accounting backend was called for an unregistered route")
	}
}

// AND THE OTHER HALF: with a funder named, the route IS registered. Without this
// the assertion above is satisfied by a route that no configuration can ever
// bring back — a permanent 404 wearing the shape of a deliberate absence.
func TestFundingRouteIsRegisteredWhenAFunderIsNamed(t *testing.T) {
	m := authz.NewMux(nil, nil)
	New(nil, "treasury").Routes(m)

	for _, r := range m.Routes() {
		if r.Capability == authz.Fund && r.Pattern == "POST /v1/portfolios/{id}/cash-movements" {
			return
		}
	}
	t.Fatal("no route requiring authz.Fund was registered even though a fund role is configured")
}

// TRADE AND FUND ARE INDEPENDENT IN BOTH DIRECTIONS. A funder must not be able
// to submit an order either, or the separation only holds one way — which is not
// a separation.
func TestAFundTokenCannotTrade(t *testing.T) {
	m := mux("treasury", authz.Fund)
	// The orders surface is registered elsewhere; declaring the same pattern here
	// is enough to ask the capability question, which is the whole assertion.
	m.Handle(authz.Trade, "POST /v1/orders", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(`{}`)), "treasury"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a FUND token submitted an order: status = %d, want 403", rr.Code)
	}
}

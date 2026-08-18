package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
)

// THE SECOND SIGNATURE IS ITS OWN AUTHORITY (#539, #410 act one).
//
// datamaster has carried a complete maker-checker workflow since #495/#498 —
// propose an override, sign it as a different person, list what is pending — and
// the gateway routed none of it. Under network-policies.yaml the gateway is
// datamaster's only permitted caller, so the whole control was reachable by
// nobody: an override could not be proposed at all, armed or unarmed, and #444's
// forged-actor fix guarded a surface nothing could touch.
//
// These three routes are that repair. What is worth testing is not that an
// approver can reach them — it is the pair of properties that make the
// capability mean something:
//
//   - A TRADER CANNOT SIGN. If Approve collapsed into Trade, every trader would
//     hold the second signature on every other trader's proposal and four-eyes
//     would be a formality that reads as a control in the audit trail.
//   - AN APPROVER CANNOT TRADE. The reverse direction is what makes this a
//     separate authority rather than a subset of one, and it is the test
//     authz.Fund's comment sets for admitting any new capability at all.
//
// Four-eyes itself — that the approver is a different PERSON from the proposer —
// is not tested here and must not be. datamaster compares authenticated
// subjects (normalised for case and surrounding space) and owns that rule with
// its own tests; restating it at the edge would be the second implementation
// this repository keeps paying for.

func overrideProposeRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/exceptions/EX1/override",
		strings.NewReader(`{"reason":"vendor is stale","chosen_price":"1.23"}`))
}

func overrideApproveRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/exceptions/EX1/override/approve",
		strings.NewReader(`{"proposal_id":"p1","decision":"approve"}`))
}

func pendingOverridesRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/v1/exceptions/pending-overrides", nil)
}

// A TRADE TOKEN CANNOT SIGN AN OVERRIDE.
func TestOverrideRoutesRefuseATradeToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func() *http.Request
	}{
		{"propose", overrideProposeRequest},
		{"approve", overrideApproveRequest},
		{"pending", pendingOverridesRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(&fakeBackend{resp: Response{Status: http.StatusOK}}, Roles{Approve: "compliance"})
			m := mux("trader", authz.Trade)
			h.Routes(m)

			rr := httptest.NewRecorder()
			m.ServeHTTP(rr, as(tc.req(), "trader"))

			if rr.Code != http.StatusForbidden {
				t.Fatalf("a TRADE token reached the %s surface: status = %d, want 403.\n"+
					"An approver capability a trader already holds is not a second signature — it is the "+
					"same person signing twice, recorded in the trail as two.", tc.name, rr.Code)
			}
		})
	}
}

// NOR CAN A READ TOKEN, which is the capability the pending queue is most likely
// to be mistakenly filed under: listing what awaits a signature is a read, but
// the queue names the proposer of every unsigned change to the marks the book is
// valued at, and it is the working surface of the control rather than a report.
func TestOverrideRoutesRefuseAReadToken(t *testing.T) {
	h := New(&fakeBackend{resp: Response{Status: http.StatusOK}}, Roles{Approve: "compliance"})
	m := mux("analyst", authz.Read)
	h.Routes(m)

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(pendingOverridesRequest(), "analyst"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a READ token listed the pending overrides: status = %d, want 403", rr.Code)
	}
}

// AND AN APPROVER CAN — without this, the refusals above are satisfied by three
// routes nobody can reach, which is the #535 shape and the whole defect #539
// exists to end.
func TestOverrideRoutesAdmitAnApproveToken(t *testing.T) {
	for _, tc := range []struct {
		name     string
		req      func() *http.Request
		wantPath string
	}{
		{"propose", overrideProposeRequest, "/v1/exceptions/EX1/override"},
		{"approve", overrideApproveRequest, "/v1/exceptions/EX1/override/approve"},
		{"pending", pendingOverridesRequest, "/v1/exceptions/pending-overrides"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeBackend{resp: Response{
				Status: http.StatusOK, ContentType: "application/json", Body: []byte(`{}`),
			}}
			h := New(be, Roles{Approve: "compliance"})
			m := mux("compliance", authz.Approve)
			h.Routes(m)

			rr := httptest.NewRecorder()
			m.ServeHTTP(rr, as(tc.req(), "compliance"))

			if rr.Code != http.StatusOK {
				t.Fatalf("an APPROVE token was refused on %s: status = %d (%s)", tc.name, rr.Code, rr.Body.String())
			}
			if be.last.Service != ServiceDataMaster {
				t.Errorf("forwarded to %q, want %q", be.last.Service, ServiceDataMaster)
			}
			// The gateway path IS the upstream path. datamaster registers these three
			// patterns verbatim, so a rewrite here would 404 upstream.
			if be.last.Path != tc.wantPath {
				t.Errorf("upstream path = %q, want %q unchanged", be.last.Path, tc.wantPath)
			}
			// THE PRINCIPAL MUST TRAVEL, and it is the reason this surface can exist at
			// all. datamaster takes the actor off X-Kanz-Principal-Subject and refuses a
			// body naming anyone else (#444); it takes the tenant off the same header
			// and answers 404 to a caller from another tenant. Forwarding without one is
			// an override signed by nobody.
			if be.last.Principal == nil || be.last.Principal.Subject != "u1" || be.last.Principal.Tenant != "t1" {
				t.Fatalf("principal forwarded = %+v, want the authenticated caller", be.last.Principal)
			}
		})
	}
}

// WITH NO APPROVER NAMED, THE ROUTES ARE ABSENT — 404, NOT 403 (#535).
//
// authz.Approve is carried by no role unless API_GATEWAY_APPROVE_ROLE names one.
// A registered route whose capability nobody holds refuses EVERY principal that
// exists, and "you may not" is a false answer when the truth is "nobody may, in
// this deployment". That is indistinguishable from a control working as designed,
// which is how authz.Fund survived from #415 to #535.
func TestOverrideRoutesAreNotRegisteredWithoutAnApprover(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func() *http.Request
	}{
		{"propose", overrideProposeRequest},
		{"approve", overrideApproveRequest},
		{"pending", pendingOverridesRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(&fakeBackend{resp: Response{Status: http.StatusOK}}, Roles{}) // no API_GATEWAY_APPROVE_ROLE
			m := mux("compliance", authz.Approve)
			h.Routes(m)

			rr := httptest.NewRecorder()
			m.ServeHTTP(rr, as(tc.req(), "compliance"))

			if rr.Code != http.StatusNotFound {
				t.Fatalf("%s answered %d with no approver configured, want 404.\n"+
					"403 sends the operator looking for a role no deployment could grant them; 404 says "+
					"there is no override surface here, which is true and actionable.", tc.name, rr.Code)
			}
		})
	}
}

// AN APPROVER CANNOT TRADE — the reverse direction, and the half that decides
// whether Approve is a distinct authority or merely a subset of Trade. If the
// person who signs off large or contested changes can also originate them, the
// capability has bought a name and no separation.
func TestAnApproveTokenCannotTrade(t *testing.T) {
	h := New(&fakeBackend{resp: Response{Status: http.StatusAccepted}}, Roles{Approve: "compliance"})
	m := mux("compliance", authz.Approve)
	h.Routes(m)

	req := httptest.NewRequest(http.MethodPost, "/v1/model-portfolios/orders",
		strings.NewReader(`{"model_portfolio_id":"MP1"}`))

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, as(req, "compliance"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("an APPROVE token materialised orders: status = %d, want 403", rr.Code)
	}
}

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE ESG SCREEN IS REACHABLE, AND THE FILING ROUTES ARE NOT (#751 item 4).
//
// internal/sustainability.Screen was complete and uncalled: regulatory mounted
// no screening route, so an ESG exclusion policy had nowhere to be submitted.
// Mounting one is only half the answer — a route nothing can reach is not a
// caller, which is what #762 cost the MCP plane. This is the gateway half.

func TestScreening_ForwardsToRegulatoryAtItsOwnPath(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, ContentType: "application/json", Body: []byte(`{"status":"COMPLIANCE_STATUS_PASS"}`)}}
	h := New(be, Roles{})
	mux := testMux()
	h.Routes(mux)

	body := `{"portfolio_id":"p1","excluded_sectors":["TOBACCO"]}`
	rr := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/screening/esg", strings.NewReader(body)), "u1", "t1")
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if be.last.Service != ServiceRegulatory {
		t.Fatalf("service = %q, want %q", be.last.Service, ServiceRegulatory)
	}
	// regulatory serves this at the same path; a rewrite would 404 upstream and
	// read as a broken service rather than a misrouted gateway.
	if be.last.Path != "/v1/screening/esg" {
		t.Fatalf("upstream path = %q, want /v1/screening/esg", be.last.Path)
	}
	if string(be.last.Body) != body {
		t.Fatalf("body = %q, not forwarded verbatim — the exclusion policy is IN the body, so a "+
			"reshaped one is a different screen", be.last.Body)
	}
}

// THE FILING ROUTES ARE NOT PROXIED, and that is a decision rather than an
// oversight. A filing is signed and appends a link to the AUDIT-01 hash chain,
// so its capability is a different question from a read route's; answering it
// while wiring a screening route is how a capability ends up meaning nothing.
func TestScreening_TheFilingRoutesAreNotFronted(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, Body: []byte(`{}`)}}
	h := New(be, Roles{})
	mux := testMux()
	h.Routes(mux)

	for _, path := range []string{
		"/v1/filings/frtb", "/v1/filings/formpf", "/v1/filings/aifmd",
		"/v1/filings/tcfd", "/v1/filings/sfdr",
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)), "u1", "t1"))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s answered %d — the filing routes must not be proxied until their capability "+
				"is decided; a filing appends to the compliance chain", path, rr.Code)
		}
		if be.last.Service == ServiceRegulatory && strings.Contains(be.last.Path, "filings") {
			t.Errorf("%s was forwarded upstream", path)
		}
	}
}

func TestScreening_UnconfiguredUpstreamIsUnavailableNotMissing(t *testing.T) {
	h := New(nil, Roles{})
	mux := testMux()
	h.Routes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/screening/esg", strings.NewReader(`{}`)), "u1", "t1"))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an unconfigured regulatory upstream", rr.Code)
	}
}

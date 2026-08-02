package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This surface had NO tenant check at all (#222).
//
// handleHousehold read r.PathValue("id") and returned the household. The
// api-gateway injects the authenticated caller's tenant on every forwarded
// request (X-Kanz-Principal-Tenant, proxy/backend.go), and the handler never
// called r.Header.Get — a repo-wide grep found only two inbound readers of that
// header, and wealth was not one of them.
//
// So anyone who could reach this service could iterate household ids and read
// total value, holdings, weights and asset-class exposure for whichever tenant
// owned them. The blast radius today is bounded by every service running as one
// `__system__` tenant (#223) — bounded by a second defect, not by a control, and
// it goes live the moment a second tenant is provisioned.

func TestHouseholdRefusesACallerFromAnotherTenant(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, asTenant(http.MethodGet, "/v1/households/HH1", "some-other-tenant"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a caller authenticated as another tenant read "+
			"this instance's household", rec.Code)
	}
	if b := rec.Body.String(); strings.Contains(b, "total_value") {
		t.Errorf("the household payload reached a foreign tenant: %s", b)
	}
}

// Fails CLOSED when the header is absent. An unauthenticated-at-this-layer
// request is not a permitted one: absence means nobody established who is
// asking, which is exactly the state every request was in before this change.
func TestHouseholdRefusesWhenTheTenantHeaderIsAbsent(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, asTenant(http.MethodGet, "/v1/households/HH1", ""))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 on a missing %s — a request with no established "+
			"caller must not be served", rec.Code, HeaderPrincipalTenant)
	}
}

// NO ORACLE: "exists but is not yours" and "does not exist" must be
// indistinguishable, or iterating ids enumerates other tenants' households.
func TestForeignHouseholdLooksExactlyLikeAMissingOne(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)

	foreign := httptest.NewRecorder()
	s.ServeHTTP(foreign, asTenant(http.MethodGet, "/v1/households/HH1", "some-other-tenant"))

	absent := httptest.NewRecorder()
	s.ServeHTTP(absent, asTenant(http.MethodGet, "/v1/households/NOPE", testTenant))

	if foreign.Code != absent.Code {
		t.Errorf("cross-tenant %d != missing %d — the status alone enumerates other tenants' "+
			"households", foreign.Code, absent.Code)
	}
	if foreign.Body.String() != absent.Body.String() {
		t.Errorf("bodies differ, so the response still separates \"not yours\" from \"not there\":"+
			"\n  cross-tenant: %s\n  missing:      %s", foreign.Body.String(), absent.Body.String())
	}
}

// The instance's own tenant is still served — without this the guard above is
// satisfied by a service that 404s everything.
func TestHouseholdStillServesItsOwnTenant(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, asTenant(http.MethodGet, "/v1/households/HH1", testTenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the instance's own tenant: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "total_value") {
		t.Errorf("the household payload is missing for its rightful tenant: %s", rec.Body.String())
	}
}

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// All four routes on this surface were unscoped (#222).
//
// golden_records, exceptions and exception_overrides are tenant-scoped tables
// with RLS, and every handler went straight from r.PathValue to the store
// without reading the tenant the api-gateway had already injected on the
// request. A repo-wide grep for X-Kanz-Principal-Tenant found only two inbound
// readers in the whole estate; datamaster was not one of them.
//
// The WRITE matters most here. POST /v1/exceptions/{id}/override records a
// human's chosen price for a disputed security — an unscoped write is not a
// disclosure bug, it is one tenant overriding another tenant's reference data.

// everyRoute drives all four, so a new one cannot be added without a decision
// about whether it belongs in this list.
func everyRoute(s *Server, tenant string) map[string]*httptest.ResponseRecorder {
	out := map[string]*httptest.ResponseRecorder{}

	for name, path := range map[string]string{
		"security":   "/v1/securities/SEC1",
		"price":      "/v1/prices/SEC1",
		"exceptions": "/v1/exceptions",
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, asTenant(http.MethodGet, path, tenant))
		out[name] = rec
	}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postAsTenant("/v1/exceptions/EX1/override", tenant, `{"chosen_price":"1.23","reason":"x"}`))
	out["override(write)"] = rec
	return out
}

func TestEveryRouteRefusesACallerFromAnotherTenant(t *testing.T) {
	s, _ := newServer(t)

	for route, rec := range everyRoute(s, "some-other-tenant") {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 — a caller authenticated as another tenant "+
				"reached this instance's data", route, rec.Code)
		}
	}
}

// Fails CLOSED with no header: absence means nobody established who is asking,
// which is the state every request was in before this change.
func TestEveryRouteRefusesWhenTheTenantHeaderIsAbsent(t *testing.T) {
	s, _ := newServer(t)

	for route, rec := range everyRoute(s, "") {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 on a missing %s", route, rec.Code, HeaderPrincipalTenant)
		}
	}
}

// The instance's own tenant is still served — otherwise the guard above is
// satisfied by a service that refuses everything. Not asserting 200 on each:
// these paths legitimately 404 on unseeded ids. Asserting only that the refusal
// is NOT the tenant guard, by checking the own-tenant response differs from the
// foreign one on at least one route that has data.
func TestOwnTenantIsNotBlanketRefused(t *testing.T) {
	s, _ := newServer(t)

	own := everyRoute(s, testTenant)
	foreign := everyRoute(s, "some-other-tenant")

	same := 0
	for route := range own {
		if own[route].Code == foreign[route].Code && own[route].Body.String() == foreign[route].Body.String() {
			same++
		}
	}
	if same == len(own) {
		t.Errorf("every route answered a foreign tenant exactly as it answered its own (%d/%d) — "+
			"the guard is indistinguishable from a service that is simply broken", same, len(own))
	}
}

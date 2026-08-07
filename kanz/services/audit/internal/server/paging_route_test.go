// THE READ ROUTES STAY BOUNDED WHATEVER THE CALLER PASSES (#304).
//
// #304 was filed against the `full-log` report template, but the template was
// never the only way to ask this service for a tenant's entire append-only log.
// `GET /v1/audit/events?limit=0` did it with no template at all: the handler
// parsed the limit with an atoiOr that accepted any n >= 0, and
// audit.Filter.Limit == 0 does not mean "no rows" — postgres.go emits NO LIMIT
// CLAUSE for it. So the caller-supplied zero was a switch that turned a capped
// read into the unbounded one, on the route that did not need fixing.
//
// These tests hold both routes to the same rule: a bound is always applied, and
// a request for one that cannot be served is REFUSED rather than quietly
// adjusted.
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/audit/internal/report"
)

func getAs(t *testing.T, srv *Server, target, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(auth.HeaderPrincipalTenant, tenant)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// ?limit=0 IS REFUSED, and the store is never reached.
func TestQuery_RefusesLimitZeroBecauseZeroRemovesTheLimitClause(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	rr := getAs(t, srv, "/v1/audit/events?limit=0", "acme")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)\n\n"+
			"?limit=0 reaches the store as Filter{Limit: 0}, which emits no LIMIT clause at all — the "+
			"caller gets the tenant's ENTIRE append-only audit log from a route that looks capped",
			rr.Code, rr.Body.String())
	}
	if spy.got.Limit != 0 || spy.got.Tenant != "" {
		t.Errorf("the store was queried with %+v despite the refusal — the request must be rejected "+
			"before the read, not after it", spy.got)
	}
}

// ABOVE THE CAP IS REFUSED, NOT CLAMPED.
func TestQuery_RefusesALimitAboveTheCap(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	rr := getAs(t, srv, "/v1/audit/events?limit=999999999", "acme")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a limit far above MaxPageSize=%d was accepted (body: %s)",
			rr.Code, report.MaxPageSize, rr.Body.String())
	}
}

// A LEGITIMATE LIMIT STILL WORKS — otherwise the two tests above would pass
// against a handler that refuses everything.
func TestQuery_AcceptsALimitWithinTheCap(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	rr := getAs(t, srv, "/v1/audit/events?limit=50", "acme")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if spy.got.Limit != 50 {
		t.Fatalf("store queried with Limit=%d, want 50", spy.got.Limit)
	}
}

// THE REPORT ROUTE APPLIES THE TEMPLATE'S PAGE SIZE BY DEFAULT.
//
// The store sees DefaultPageSize+1, not DefaultPageSize: report.Generate asks
// for one extra row to decide whether a further page exists, then returns at
// most the page size. That +1 is the has-more probe, and asserting the exact
// value here is deliberate — it is the difference between "the cap is applied"
// and "the cap is applied AND completeness is being measured".
func TestReport_AppliesTheTemplatesPageSizeByDefault(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	rr := getAs(t, srv, "/v1/audit/reports/full-log", "acme")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if want := report.DefaultPageSize + 1; spy.got.Limit != want {
		t.Fatalf("full-log queried the store with Limit=%d, want %d (DefaultPageSize + the has-more "+
			"probe). Limit=0 here is the original defect: a tenant's entire WORM history in one read",
			spy.got.Limit, want)
	}
	if spy.got.Tenant != "acme" {
		t.Errorf("report queried tenant %q, want acme", spy.got.Tenant)
	}
}

// ?after= REACHES THE STORE AS THE CURSOR.
func TestReport_PassesTheCursorThroughToTheStore(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	rr := getAs(t, srv, "/v1/audit/reports/full-log?after=4242", "acme")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if spy.got.AfterSeq != 4242 {
		t.Fatalf("store queried with AfterSeq=%d, want 4242 — the cursor is dropped, so every page "+
			"restarts at the beginning and the export never terminates", spy.got.AfterSeq)
	}
}

// A MALFORMED CURSOR IS REFUSED. Silently treating it as 0 would restart the
// export from the beginning of the log while the caller believes it resumed —
// duplicated evidence, and no signal that it happened.
func TestReport_RefusesAMalformedCursor(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "9999999999999999999999"} {
		t.Run(bad, func(t *testing.T) {
			spy := &filterSpy{}
			srv := newAuditServer(spy)

			rr := getAs(t, srv, "/v1/audit/reports/full-log?after="+bad, "acme")

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("?after=%s gave status %d, want 400 — an unparseable cursor silently becomes 0 "+
					"and the export restarts from the beginning (body: %s)", bad, rr.Code, rr.Body.String())
			}
		})
	}
}

// THE REPORT ROUTE HONOURS THE SAME CAP AS THE QUERY ROUTE. The template's own
// Limit must not be a way around it.
func TestReport_RefusesALimitAboveTheCap(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	rr := getAs(t, srv, "/v1/audit/reports/full-log?limit=0", "acme")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("?limit=0 on the report route gave %d, want 400", rr.Code)
	}

	rr = getAs(t, srv, "/v1/audit/reports/full-log?limit=999999999", "acme")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("?limit=999999999 on the report route gave %d, want 400", rr.Code)
	}
}

// Tenant scoping on the audit read API.
//
// audit_log is deliberately NOT RLS'd — it is the CROSS-TENANT compliance
// record, so the database will happily return every tenant's rows. That makes
// this handler the only thing standing between a caller and the whole estate's
// audit history, and it was taking the tenant from `?tenant=` — caller-supplied
// input, with no principal check and no auth middleware anywhere in the service.
//
// The fix is the platform's existing convention, not a new one: the api-gateway
// is the sole identity authority and injects X-Kanz-Principal-Tenant from the
// verified token. tv-sync/brokerapi already reads exactly this header, and its
// own comment records that it used to read a bespoke header "nothing set and any
// caller could" — the same bug, already fixed once here.
package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/audit/internal/audit"

	"github.com/eighred/kanz/pkg/auth"
)

// filterSpy is an audit.Store that records the Filter it was queried with.
type filterSpy struct{ got audit.Filter }

func (s *filterSpy) Append(context.Context, *audit.Record) (*audit.Record, error) {
	return nil, nil
}
func (s *filterSpy) Get(context.Context, string) (*audit.Record, bool, error) { return nil, false, nil }
func (s *filterSpy) Query(_ context.Context, f audit.Filter) ([]*audit.Record, error) {
	s.got = f
	return nil, nil
}
func (s *filterSpy) Scan(context.Context, func(*audit.Record) error) error { return nil }
func (s *filterSpy) Head(context.Context) (audit.Head, error)              { return audit.Head{}, nil }
func (s *filterSpy) Ping(context.Context) error                            { return nil }

func newAuditServer(st audit.Store) *Server {
	r := &Readiness{}
	r.Set(true)
	return New(r, slog.New(slog.NewTextHandler(io.Discard, nil)), WithStore(st))
}

// The tenant must come from the AUTHENTICATED principal, never from the query
// string. A caller naming someone else's tenant must not read it.
func TestQuery_ScopesToThePrincipalsTenantNotTheQueryString(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events?tenant=victim-corp", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if spy.got.Tenant != "acme" {
		t.Fatalf("queried tenant = %q, want %q — a caller-supplied ?tenant= reached the store, "+
			"and audit_log is not RLS'd, so this reads another tenant's entire audit history",
			spy.got.Tenant, "acme")
	}
}

// An unscoped request is refused. audit_log is cross-tenant by design, so an
// empty tenant filter does not return "nothing" — it returns EVERY tenant's
// records. Deny-by-default is the only safe reading of a missing principal.
func TestQuery_RefusesARequestCarryingNoPrincipal(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — an unscoped query against a non-RLS'd cross-tenant "+
			"log must be refused, not answered with every tenant's history", rr.Code)
	}
	if spy.got.Tenant != "" {
		t.Fatalf("store was queried with tenant %q — the refusal must happen BEFORE the store is reached", spy.got.Tenant)
	}
}

// NON-VACUITY: a properly scoped request must still be answered, and the other
// filters must still reach the store. A handler that refused everything would
// pass both tests above while breaking the compliance read path entirely.
func TestQuery_ScopedRequestStillQueriesWithItsOtherFilters(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events?kind=authz&event_type=order.submitted&limit=7", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if spy.got.Tenant != "acme" {
		t.Fatalf("tenant = %q, want acme", spy.got.Tenant)
	}
	if spy.got.EventType != "order.submitted" || spy.got.Limit != 7 || spy.got.Kind != audit.Kind("authz") {
		t.Fatalf("filters = %+v — scoping the tenant must not drop the caller's other filters", spy.got)
	}
}

// oneRecord is a store holding exactly one record, owned by the given tenant.
type oneRecord struct {
	filterSpy
	rec *audit.Record
}

func (s *oneRecord) Get(context.Context, string) (*audit.Record, bool, error) {
	return s.rec, true, nil
}

// Fetching a single record by id must also be tenant-scoped. The query endpoint
// was the reported hole, but `GET /v1/audit/events/{id}` reads the same non-RLS'd
// cross-tenant log and took no tenant into account at all — a caller who knows or
// guesses an event id read another tenant's audit record.
//
// 404, not 403: on a cross-tenant boundary, "forbidden" confirms the record
// EXISTS, which is itself a disclosure. The caller learns only that they have no
// such record.
func TestGet_RefusesARecordBelongingToAnotherTenant(t *testing.T) {
	st := &oneRecord{rec: &audit.Record{EventID: "e1", TenantID: "victim-corp"}}
	srv := newAuditServer(st)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events/e1", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — acme read victim-corp's audit record (body: %s)",
			rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "victim-corp") {
		t.Fatalf("response leaked the owning tenant: %s", rr.Body.String())
	}
}

// NON-VACUITY: a caller must still read their OWN record.
func TestGet_ReturnsTheCallersOwnRecord(t *testing.T) {
	st := &oneRecord{rec: &audit.Record{EventID: "e1", TenantID: "acme"}}
	srv := newAuditServer(st)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events/e1", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the owner must still read their own record (body: %s)",
			rr.Code, rr.Body.String())
	}
}

// --- lineage / verify / reports ---
//
// These three were left unscoped when the query and get endpoints were fixed,
// and were flagged rather than patched because the same tenant filter is not
// right for all of them. Resolved by looking at what each actually reads:
//
//   report.Generate    store.Query(tmpl.Filter) — Filter HAS a Tenant field, so
//                      it scopes cleanly and its content is per-tenant records.
//   lineage.Reconstruct store.Get + Query{Correlation} — the target record
//                      carries TenantID, so it scopes too.
//   report.Verify      store.All + store.Head — the hash chain is ONE sequence
//                      across every tenant. Verifying a subset proves nothing
//                      about the chain, so tenant-scoping it would BREAK the
//                      tamper-evidence it exists to provide. It stays global,
//                      and only anonymous access is removed.

// A caller must not walk the lineage of another tenant's event. 404, not 403,
// for the same reason as the single-record fetch.
func TestLineage_RefusesAnEventBelongingToAnotherTenant(t *testing.T) {
	st := &oneRecord{rec: &audit.Record{EventID: "e1", TenantID: "victim-corp", CorrelationID: "c1"}}
	srv := newAuditServer(st)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/lineage/e1", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — acme walked victim-corp's causation graph (body: %s)",
			rr.Code, rr.Body.String())
	}
}

func TestLineage_RefusesARequestCarryingNoPrincipal(t *testing.T) {
	st := &oneRecord{rec: &audit.Record{EventID: "e1", TenantID: "acme"}}
	srv := newAuditServer(st)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/lineage/e1", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

// A report must be generated over the CALLER's records only. The template's
// filter decides what the report contains, so an unscoped one renders every
// tenant's audit history into a downloadable CSV.
func TestReport_ScopesTheTemplateFilterToThePrincipalsTenant(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/reports/authz-decisions", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if spy.got.Tenant != "acme" {
		t.Fatalf("report queried tenant = %q, want acme — the report renders every tenant's "+
			"audit history into a downloadable artifact", spy.got.Tenant)
	}
	// NON-VACUITY: scoping must not discard the template's own filter.
	if spy.got.Kind != audit.KindAuthzDecision {
		t.Fatalf("kind = %q — scoping the tenant dropped the template's filter", spy.got.Kind)
	}
}

func TestReport_RefusesARequestCarryingNoPrincipal(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/reports/authz-decisions", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

// Verify stays GLOBAL by design — the chain spans every tenant and a partial
// verification is not a verification. What must go is ANONYMOUS access.
func TestVerify_RefusesARequestCarryingNoPrincipal(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/verify", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — chain attestation must not be anonymous", rr.Code)
	}
}

// NON-VACUITY for verify: an authenticated caller still gets the attestation,
// and it is still computed over the WHOLE chain. A "fix" that tenant-scoped it
// would pass the test above while silently destroying the tamper-evidence.
//
// NOTE SINCE #118: newAuditServer passes no WithVerifyRoles, so this exercises
// the UNRESTRICTED posture — the one a deployment must opt into explicitly with
// AUDIT_ALLOW_UNRESTRICTED_VERIFY, because the composition root refuses to start
// with neither that nor AUDIT_VERIFY_ROLES set. That is deliberate here: this
// test is about the attestation being WHOLE-CHAIN, and mixing the capability
// gate into it would make a role-provisioning mistake look like a scoping
// regression. The capability itself is covered in verify_capability_test.go.
func TestVerify_AuthenticatedCallerStillGetsAGlobalAttestation(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/verify", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if spy.got.Tenant != "" {
		t.Fatalf("verify queried with tenant %q — the chain must be verified WHOLE; "+
			"a per-tenant subset proves nothing about it", spy.got.Tenant)
	}
}

// The SOC2 evidence endpoint was NOT in the reported set and nobody flagged it,
// but it is the same defect on the same surface: soc2.CollectFromStore queries
// with only a time window, so the evidence report was assembled from every
// tenant's records. Like the report endpoint, its output is designed to leave
// the building — an auditor receives it.
func TestSOC2Evidence_ScopesToThePrincipalsTenant(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/v1/soc2/evidence", nil)
	req.Header.Set(auth.HeaderPrincipalTenant, "acme")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if spy.got.Tenant != "acme" {
		t.Fatalf("evidence queried tenant = %q, want acme — the SOC2 report is compiled from "+
			"every tenant's audit records and handed to an auditor", spy.got.Tenant)
	}
}

func TestSOC2Evidence_RefusesARequestCarryingNoPrincipal(t *testing.T) {
	spy := &filterSpy{}
	srv := newAuditServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/v1/soc2/evidence", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

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

	"github.com/kanz-eng/kanz/services/audit/internal/audit"
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
func (s *filterSpy) All(context.Context) ([]*audit.Record, error) { return nil, nil }
func (s *filterSpy) Head(context.Context) (audit.Head, error)     { return audit.Head{}, nil }
func (s *filterSpy) Ping(context.Context) error                   { return nil }

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
	req.Header.Set(HeaderPrincipalTenant, "acme")
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
	req.Header.Set(HeaderPrincipalTenant, "acme")
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
	req.Header.Set(HeaderPrincipalTenant, "acme")
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
	req.Header.Set(HeaderPrincipalTenant, "acme")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the owner must still read their own record (body: %s)",
			rr.Code, rr.Body.String())
	}
}

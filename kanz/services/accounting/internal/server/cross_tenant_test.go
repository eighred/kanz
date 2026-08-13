package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// EVERY /v1 ROUTE ON THE BOOK OF RECORD WAS UNSCOPED (#415 step 1 prerequisite).
//
// accounting is a SINGLE-TENANT INSTANCE: main pins its RLS pool with
// pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant), and 0001_ledger.sql runs
// FORCE ROW LEVEL SECURITY against that GUC. But the HTTP surface read no
// principal at all — a repo-wide grep for a principal header found nothing in
// this service — so every route went from r.PathValue straight to the store.
//
// THE WRITES ARE THE POINT, AND THEY MOVE MONEY. POST cash-movements posts a
// subscription or a redemption to the IBOR; POST nav and POST reconcile write
// the fund's valuation and its break list. A caller from any other tenant could
// name any portfolio id and move cash in this instance's book.
//
// This is #222 exactly — the hole datamaster and wealth had, in the service that
// moves capital rather than the one that prices it. It is a PREREQUISITE for
// fronting accounting at the gateway (#415 step 1): exposing these routes before
// they are scoped turns a pod-to-pod gap into an internet-reachable one.
//
// FAILS CLOSED ON BOTH SIDES, which is what auth.RequireCallerTenantIs already
// encodes: an absent header and an unset instance tenant both mean nobody
// established who is asking, and neither is permission. So a deployment that
// forgets WithTenant serves nothing rather than serving everyone.

// testTenant is the tenant this instance serves; otherTenant is anybody else.
const (
	testTenant  = "fund-alpha"
	otherTenant = "someone-else"
)

func tenantServer(t *testing.T) *Server {
	t.Helper()
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, ledger.NewMemoryStore(), "USD",
		WithTenant(testTenant), WithCashPublisher(&fakeCashPublisher{}))
}

// everyV1Route drives all three writable routes, so a new one cannot be added
// without a decision about whether it belongs in this list.
func everyV1Route(s *Server, tenant string) map[string]*httptest.ResponseRecorder {
	out := map[string]*httptest.ResponseRecorder{}
	for name, body := range map[string]string{
		"cash-movements": `{"movement_id":"M1","kind":"subscription","amount":"100"}`,
		"nav":            `{}`,
		"reconcile":      `{}`,
	} {
		path := "/v1/portfolios/PF1/" + name
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if tenant != "" {
			req.Header.Set(auth.HeaderPrincipalTenant, tenant)
		}
		req.Header.Set(auth.HeaderPrincipalSubject, "alice@kanz")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		out[name] = rec
	}
	return out
}

func TestEveryRouteRefusesACallerFromAnotherTenant(t *testing.T) {
	s := tenantServer(t)
	for name, rec := range everyV1Route(s, otherTenant) {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: a caller from %q got HTTP %d, want 404.\n"+
				"This instance's RLS pool is pinned to %q, so a foreign caller has no business "+
				"here whatever the database holds — and on a WRITE that is not a disclosure "+
				"bug, it is one tenant moving another tenant's cash.",
				name, otherTenant, rec.Code, testTenant)
		}
	}
}

func TestEveryRouteRefusesACallerWithNoTenant(t *testing.T) {
	s := tenantServer(t)
	for name, rec := range everyV1Route(s, "") {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: an unscoped caller got HTTP %d, want 404. An absent header means "+
				"nobody established who is asking, which is not permission.", name, rec.Code)
		}
	}
}

// THE INSTANCE'S OWN TENANT IS STILL SERVED. Without this, the refusals above
// are satisfied by a surface that refuses everyone — a trading-operations outage
// wearing the shape of a fix.
func TestTheInstancesOwnTenantIsStillServed(t *testing.T) {
	s := tenantServer(t)
	for name, rec := range everyV1Route(s, testTenant) {
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s: the instance's OWN tenant was refused (404) — the gate is on backwards", name)
		}
	}
}

// A SERVER WITH NO TENANT SERVES NOTHING. A deployment that forgets WithTenant
// must not fall back to "well, it authenticated": an unset instance tenant means
// nobody can establish whose book this is.
func TestAServerWithNoConfiguredTenantRefusesEveryone(t *testing.T) {
	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, ledger.NewMemoryStore(), "USD", WithCashPublisher(&fakeCashPublisher{}))
	for name, rec := range everyV1Route(s, testTenant) {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: an instance with no configured tenant answered HTTP %d, want 404 — "+
				"a missing tenant is not a wildcard", name, rec.Code)
		}
	}
}

// PROBES ARE NOT TENANT-SCOPED, and must not be: kubelet does not carry a
// principal, and a readiness probe that 404s takes the pod out of service.
func TestProbesAreNotTenantScoped(t *testing.T) {
	s := tenantServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s answered 404 with no principal — kubelet carries none, and a probe "+
				"behind the tenant gate takes the pod out of service", path)
		}
	}
}

// postV1 builds a /v1 request carrying the principal headers the api-gateway
// injects. Every /v1 route is tenant-scoped now, so a test that omits them is
// testing the refusal rather than the handler.
func postV1(method, path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set(auth.HeaderPrincipalTenant, testTenant)
	req.Header.Set(auth.HeaderPrincipalSubject, "alice@kanz")
	return req
}

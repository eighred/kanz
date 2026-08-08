// WHO MAY READ THE ESTATE-WIDE CHAIN ATTESTATION (#118).
//
// GET /v1/audit/verify is the one read on this surface that is deliberately NOT
// tenant-scoped, and it cannot be: the hash chain is ONE sequence across every
// tenant, so verifying a per-tenant subset proves nothing about it. That makes
// its attestation the single cross-tenant answer this service gives, and the
// record count in it tells any authenticated caller how much OTHER tenants'
// activity the platform is carrying.
//
// The ruling (2026-08-08) is that a tenant may see its own vault while the
// estate-wide figure belongs to operators and monitoring. So the restriction is
// on WHO MAY ASK — scoping the attestation itself would destroy the property it
// exists to prove.
package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/audit/internal/audit"
	"github.com/eighred/kanz/services/audit/internal/server"
)

// emptyStore is enough for /verify: an empty chain verifies cleanly, so a 200
// means the handler ran and a 403 means it refused before reaching the store.
type emptyStore struct{}

func (emptyStore) Append(context.Context, *audit.Record) (*audit.Record, error) { return nil, nil }
func (emptyStore) Get(context.Context, string) (*audit.Record, bool, error)     { return nil, false, nil }
func (emptyStore) Query(context.Context, audit.Filter) ([]*audit.Record, error) { return nil, nil }
func (emptyStore) Scan(context.Context, func(*audit.Record) error) error        { return nil }
func (emptyStore) Head(context.Context) (audit.Head, error)                     { return audit.Head{}, nil }
func (emptyStore) Ping(context.Context) error                                   { return nil }

func verifyAs(t *testing.T, roles []string, tenant string, srvRoles []string) *httptest.ResponseRecorder {
	t.Helper()
	srv := server.New(readyServer(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		server.WithStore(emptyStore{}), server.WithVerifyRoles(srvRoles))

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/verify", nil)
	if tenant != "" {
		auth.SetPrincipalHeaders(req.Header, "user:someone", tenant, roles)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func readyServer() *server.Readiness {
	r := &server.Readiness{}
	r.Set(true)
	return r
}

// AN OPERATOR ROLE READS THE ATTESTATION.
func TestVerify_AllowsAPrincipalHoldingAVerifyRole(t *testing.T) {
	rr := verifyAs(t, []string{"kanz-operator"}, "acme", []string{"kanz-operator", "kanz-monitoring"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a principal holding kanz-operator (body: %s).\n\n"+
			"Over-restricting a tamper check is its own failure: if nobody can verify, nothing "+
			"detects tampering.", rr.Code, rr.Body.String())
	}
}

// THE MONITORING ROLE TOO — the ruling names both, and a check that only
// admitted operators would silently take verification away from the thing that
// actually runs it on a schedule.
func TestVerify_AllowsTheMonitoringRole(t *testing.T) {
	rr := verifyAs(t, []string{"kanz-monitoring"}, "acme", []string{"kanz-operator", "kanz-monitoring"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for kanz-monitoring (body: %s)", rr.Code, rr.Body.String())
	}
}

// A TENANT'S OWN SERVICE ACCOUNT IS REFUSED. This is the leak the ruling closes:
// authenticated, legitimately scoped to its own data everywhere else, and asking
// for a number that describes the whole estate.
func TestVerify_RefusesAnAuthenticatedPrincipalWithoutAVerifyRole(t *testing.T) {
	rr := verifyAs(t, []string{"tenant-user"}, "acme", []string{"kanz-operator", "kanz-monitoring"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an authenticated principal holding no verify role "+
			"(body: %s).\n\n"+
			"The attestation covers EVERY tenant's records, so its count discloses how much other "+
			"tenants' activity this platform carries. A tenant may see its own vault; this figure is "+
			"not its own.", rr.Code, rr.Body.String())
	}
}

// NO ROLES AT ALL is refused for the same reason — the empty set must not read
// as a wildcard.
func TestVerify_RefusesAPrincipalWithNoRoles(t *testing.T) {
	rr := verifyAs(t, nil, "acme", []string{"kanz-operator"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a principal with no roles — an empty role set must not "+
			"read as 'all roles'", rr.Code)
	}
}

// ANONYMOUS IS STILL 401, NOT 403. The capability check must not mask the
// authentication one: those are different incidents and an operator debugging a
// 403 would look at role provisioning rather than at a missing principal.
func TestVerify_StillRefusesAnonymousBeforeTheCapabilityCheck(t *testing.T) {
	rr := verifyAs(t, nil, "", []string{"kanz-operator"})
	if rr.Code == http.StatusForbidden {
		t.Fatal("an anonymous request returned 403 — the capability check ran before authentication, " +
			"so a missing principal is being reported as a missing role")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an unauthenticated request", rr.Code)
	}
}

// THE EXPLICIT ADMISSION PATH. An empty verifyRoles means the deployment set
// AUDIT_ALLOW_UNRESTRICTED_VERIFY — the composition root refuses to start
// otherwise — so it must serve, not deny. A deny here would turn a stated choice
// into a silent outage of the tamper check.
func TestVerify_EmptyRolesServesAnyAuthenticatedPrincipal(t *testing.T) {
	rr := verifyAs(t, []string{"tenant-user"}, "acme", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when verifyRoles is empty (the explicit "+
			"AUDIT_ALLOW_UNRESTRICTED_VERIFY posture) — body: %s", rr.Code, rr.Body.String())
	}
}

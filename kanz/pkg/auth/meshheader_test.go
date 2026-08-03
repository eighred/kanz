package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The consolidated identity-header contract (#258). These assertions are the
// behaviour four services and the gateway each carried their own copy of, and
// they are pinned here because there is now only one place left to break them.

func TestSetPrincipalHeaders(t *testing.T) {
	h := http.Header{}
	SetPrincipalHeaders(h, "u1", "acme", []string{"analyst", "pm"})
	if got := h.Get(HeaderPrincipalSubject); got != "u1" {
		t.Errorf("subject = %q, want u1", got)
	}
	if got := h.Get(HeaderPrincipalTenant); got != "acme" {
		t.Errorf("tenant = %q, want acme", got)
	}
	if got := h.Get(HeaderPrincipalRoles); got != "analyst,pm" {
		t.Errorf("roles = %q, want analyst,pm", got)
	}
}

// An empty role list must not become an empty header: an upstream splitting
// "" on "," gets a single empty role, which is a role name nobody wrote.
func TestSetPrincipalHeadersOmitsEmptyRoles(t *testing.T) {
	h := http.Header{}
	SetPrincipalHeaders(h, "u1", "acme", nil)
	if _, present := h[http.CanonicalHeaderKey(HeaderPrincipalRoles)]; present {
		t.Errorf("roles header present with no roles: %q", h.Get(HeaderPrincipalRoles))
	}
}

func TestRequireCallerTenant(t *testing.T) {
	t.Run("scoped request passes the tenant through", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil)
		r.Header.Set(HeaderPrincipalTenant, "acme")
		w := httptest.NewRecorder()
		got, ok := RequireCallerTenant(w, r)
		if !ok || got != "acme" {
			t.Fatalf("got (%q, %v), want (acme, true)", got, ok)
		}
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Errorf("wrote a response on the allow path: %d %q", w.Code, w.Body.String())
		}
	})

	// DENY BY DEFAULT. An empty tenant reaching a store is not "no records" — for
	// audit, whose log is deliberately not RLS'd, it is EVERY tenant's records.
	t.Run("unscoped request is refused with 401", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil)
		w := httptest.NewRecorder()
		if got, ok := RequireCallerTenant(w, r); ok || got != "" {
			t.Fatalf("got (%q, %v), want (\"\", false)", got, ok)
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not the {\"error\": …} shape callers parse: %v", err)
		}
		if body["error"] == "" {
			t.Error("refusal carried no error message")
		}
	})
}

func TestRequireCallerTenantIs(t *testing.T) {
	const miss = "household not found"

	t.Run("the instance's own tenant is served", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/v1/households/h1", nil)
		r.Header.Set(HeaderPrincipalTenant, "acme")
		w := httptest.NewRecorder()
		if !RequireCallerTenantIs(w, r, "acme", miss) {
			t.Fatalf("refused the instance's own tenant: %d %q", w.Code, w.Body.String())
		}
	})

	// NO EXISTENCE ORACLE. All three refusals must be byte-identical to each
	// other, and callers must be able to make them identical to a genuine miss —
	// otherwise id enumeration is a cross-tenant directory.
	refusals := map[string]struct {
		callerTenant   string
		instanceTenant string
	}{
		"another tenant":        {"other", "acme"},
		"no header at all":      {"", "acme"},
		"instance tenant unset": {"acme", ""},
	}
	var seen string
	for name, tc := range refusals {
		t.Run(name+" is refused as a miss", func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/households/h1", nil)
			if tc.callerTenant != "" {
				r.Header.Set(HeaderPrincipalTenant, tc.callerTenant)
			}
			w := httptest.NewRecorder()
			if RequireCallerTenantIs(w, r, tc.instanceTenant, miss) {
				t.Fatal("allowed — this is the #222 cross-tenant read")
			}
			if w.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (403 would confirm the row exists)", w.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not the {\"error\": …} shape: %v", err)
			}
			if body["error"] != miss {
				t.Errorf("error = %q, want %q — the refusal must be the caller's genuine-miss body",
					body["error"], miss)
			}
			if seen == "" {
				seen = w.Body.String()
			} else if w.Body.String() != seen {
				t.Errorf("refusal body %q differs from %q — the difference IS the oracle",
					w.Body.String(), seen)
			}
		})
	}
}

func TestCallerTenantIsEmptyWithoutAPrincipal(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := CallerTenant(r); got != "" {
		t.Errorf("CallerTenant = %q, want \"\" — an unauthenticated request has no tenant", got)
	}
}

// THE UPSTREAM HALF OF THE SEAM (#268).
//
// SetPrincipalHeaders had no reader for as long as it existed. These assertions
// are deliberately written as ROUND TRIPS through both halves rather than as two
// sets pinning each side's encoding: the encoding is not the contract, the
// survival of the principal across the mesh is, and two independently-correct
// halves that disagree on a separator are exactly the failure a paired assertion
// catches and a split one does not.

func TestPrincipalHeadersRoundTrip(t *testing.T) {
	for name, want := range map[string]*Principal{
		"subject, tenant and several roles": {Subject: "u1", Tenant: "acme", Roles: []string{"analyst", "pm"}},
		"a single role":                     {Subject: "u2", Tenant: "beta", Roles: []string{"steward"}},
		// nil roles must come back nil, not []string{""} — the writer omits the
		// header entirely, and a single empty role would be a role name nobody
		// wrote that a policy lookup then misses for the wrong reason.
		"no roles at all": {Subject: "svc", Tenant: "acme", Roles: nil},
	} {
		t.Run(name, func(t *testing.T) {
			h := http.Header{}
			SetPrincipalHeaders(h, want.Subject, want.Tenant, want.Roles)

			got, ok := PrincipalFromHeaders(h)
			if !ok {
				t.Fatalf("PrincipalFromHeaders rejected what SetPrincipalHeaders wrote: %v", h)
			}
			if got.Subject != want.Subject || got.Tenant != want.Tenant {
				t.Errorf("identity = %q/%q, want %q/%q", got.Subject, got.Tenant, want.Subject, want.Tenant)
			}
			if len(got.Roles) != len(want.Roles) {
				t.Fatalf("roles = %#v, want %#v", got.Roles, want.Roles)
			}
			for i := range want.Roles {
				if got.Roles[i] != want.Roles[i] {
					t.Errorf("roles = %#v, want %#v", got.Roles, want.Roles)
					break
				}
			}
		})
	}
}

// The round trip has to survive the thing that consumes it, not merely compare
// equal. A role list that arrives as one unsplit "analyst,pm", or as a single
// empty string, grants nothing — and the governed read then fails with "no role
// grants action", which reads like a policy problem and is a transport problem.
func TestRoundTrippedPrincipalStillAuthorizes(t *testing.T) {
	h := http.Header{}
	SetPrincipalHeaders(h, "u1", "acme", []string{"viewer", "analyst"})
	p, ok := PrincipalFromHeaders(h)
	if !ok {
		t.Fatal("PrincipalFromHeaders rejected a fully-populated header set")
	}
	az := NewPolicyAuthorizer(&Policy{Roles: map[string][]Action{"analyst": {ActionRiskRead}}})
	d := az.Authorize(context.Background(), Request{
		Principal: p,
		Action:    ActionRiskRead,
		Resource:  Resource{Type: ResourcePortfolio, ID: "PF-1", Tenant: "acme"},
	})
	if !d.Allow {
		t.Fatalf("a principal off the wire was denied its own grant: %s", d.Reason)
	}
}

// FAIL CLOSED. Neither field alone identifies a caller, and a Principal carrying
// only one of them is worse than none: PolicyAuthorizer denies a tenant-less
// principal outright while lineage's governor would still serve it every non-PII
// dataset, so the two disagree about what a half-identified caller may read.
func TestPrincipalFromHeadersFailsClosedOnPartialIdentity(t *testing.T) {
	for name, h := range map[string]http.Header{
		"nothing at all": {},
		"tenant only":    {HeaderPrincipalTenant: []string{"acme"}},
		"subject only":   {HeaderPrincipalSubject: []string{"u1"}},
		"roles only":     {HeaderPrincipalRoles: []string{"admin"}},
	} {
		t.Run(name, func(t *testing.T) {
			p, ok := PrincipalFromHeaders(h)
			if ok || p != nil {
				t.Fatalf("want (nil, false), got (%+v, %v) — a caller this service cannot name "+
					"must not become a Principal", p, ok)
			}
		})
	}
}

func TestRequirePrincipal(t *testing.T) {
	// reached and seen are separate so "the middleware let it through" and "the
	// middleware put the right caller on the context" fail distinguishably.
	var seen *Principal
	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen, _ = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := RequirePrincipal(next)

	t.Run("a gateway-injected request reaches the handler with its principal", func(t *testing.T) {
		seen, reached = nil, false
		r := httptest.NewRequest(http.MethodGet, "/v1/lineage/dataset/kanz.risk/Exposure", nil)
		SetPrincipalHeaders(r.Header, "u1", "acme", []string{"steward"})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !reached {
			t.Fatal("handler not reached for an authenticated request")
		}
		if seen == nil {
			t.Fatal("handler reached with a nil principal — the reconstruction did not run")
		}
		if seen.Subject != "u1" || seen.Tenant != "acme" || !seen.HasRole("steward") {
			t.Errorf("principal on ctx = %+v, want u1/acme/[steward]", seen)
		}
	})

	t.Run("an unidentified request is refused and never reaches the handler", func(t *testing.T) {
		seen, reached = nil, false
		r := httptest.NewRequest(http.MethodGet, "/v1/lineage/dataset/kanz.risk/Exposure", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if reached {
			t.Fatal("handler ran for a request carrying no principal — that is the #268 defect")
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("refusal body is not the {\"error\": …} shape every surface returns: %v", err)
		}
		if body["error"] == "" {
			t.Errorf("refusal carries no reason: %s", w.Body.String())
		}
	})

	t.Run("the infrastructure probes are exempt", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/readyz", "/livez", "/metrics"} {
			seen, reached = nil, false
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if !reached {
				t.Errorf("%s was refused — the kubelet and Prometheus carry no principal, "+
					"so refusing it makes the pod unready and unscrapable", path)
			}
			if seen != nil {
				t.Errorf("%s got a principal: %+v", path, seen)
			}
		}
	})

	// The exemption is an exact-path set, not a prefix match: a governed route
	// that merely begins with a probe name must not inherit the exemption.
	t.Run("only the exact probe paths are exempt", func(t *testing.T) {
		for _, path := range []string{"/metrics/v1/lineage", "/healthzz", "/readyz/v1/catalog"} {
			seen, reached = nil, false
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if reached {
				t.Errorf("%s was served without a principal — the probe exemption matches too widely", path)
			}
		}
	})
}

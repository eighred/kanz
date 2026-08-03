package auth

import (
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

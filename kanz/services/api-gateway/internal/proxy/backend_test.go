package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// SVCWIRE-01e — contract test of the concrete mesh backend (01c) + the proxy
// handler (01b) end-to-end against httptest upstreams: an authenticated request
// reaches the right upstream with the principal propagated, the upstream reply
// is returned verbatim, and a cross-tenant copilot denial passes through.

func TestMeshBackend_ForwardsRequestAndPrincipal(t *testing.T) {
	var got *http.Request
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answer":"42"}`))
	}))
	defer upstream.Close()

	be := NewMeshBackend(map[Service]string{ServiceCopilot: upstream.URL}, upstream.Client())
	resp, err := be.Forward(context.Background(), Request{
		Service:   ServiceCopilot,
		Method:    http.MethodPost,
		Path:      "/v1/ask",
		Body:      []byte(`{"question":"q"}`),
		Principal: &middleware.Principal{Subject: "u1", Tenant: "t1", Roles: []string{"analyst", "pm"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK || string(resp.Body) != `{"answer":"42"}` {
		t.Fatalf("response not verbatim: %d %q", resp.Status, resp.Body)
	}
	if got.URL.Path != "/v1/ask" || gotBody != `{"question":"q"}` {
		t.Fatalf("upstream got path=%q body=%q", got.URL.Path, gotBody)
	}
	// The gateway-verified principal is propagated as trusted mesh identity.
	if got.Header.Get(auth.HeaderPrincipalSubject) != "u1" || got.Header.Get(auth.HeaderPrincipalTenant) != "t1" {
		t.Fatalf("principal headers not forwarded: %v", got.Header)
	}
	if got.Header.Get(auth.HeaderPrincipalRoles) != "analyst,pm" {
		t.Fatalf("roles header = %q, want analyst,pm", got.Header.Get(auth.HeaderPrincipalRoles))
	}
}

func TestMeshBackend_ForwardsQueryAndStatusPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("as_of") != "2026-06-30T00:00:00Z" {
			t.Errorf("query not forwarded: %v", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusNotFound) // upstream's own status must pass through
		_, _ = w.Write([]byte(`{"error":"instrument not found"}`))
	}))
	defer upstream.Close()

	be := NewMeshBackend(map[Service]string{ServiceDataMaster: upstream.URL}, upstream.Client())
	resp, err := be.Forward(context.Background(), Request{
		Service: ServiceDataMaster, Method: http.MethodGet, Path: "/v1/securities/AAPL",
		Query: url.Values{"as_of": {"2026-06-30T00:00:00Z"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 passthrough", resp.Status)
	}
}

func TestMeshBackend_UnconfiguredService_Unavailable(t *testing.T) {
	be := NewMeshBackend(map[Service]string{ServiceWealth: "https://wealth:8080"}, nil)
	if !be.Configured() {
		t.Fatal("expected Configured() true")
	}
	_, err := be.Forward(context.Background(), Request{Service: ServiceCopilot, Method: http.MethodPost, Path: "/v1/ask"})
	if err != ErrBackendUnavailable {
		t.Fatalf("err = %v, want ErrBackendUnavailable", err)
	}
}

// End-to-end through the Handler + MeshBackend: an authenticated /v1/ask is
// forwarded to the copilot upstream, and a cross-tenant probe the upstream
// denies (403) is returned to the client verbatim — the gateway is a faithful
// identity-propagating forwarder; tenant scoping is enforced downstream where
// the portfolio's owning tenant is known.
func TestEndToEnd_AskForwardedAndCrossTenantDenied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The fake copilot serves tenant "t1" only; a forwarded principal from
		// another tenant is denied (the downstream tenant boundary).
		if r.Header.Get(auth.HeaderPrincipalTenant) != "t1" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"cross-tenant denied"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answer":"grounded","grounded":true}`))
	}))
	defer upstream.Close()

	h := New(NewMeshBackend(map[Service]string{ServiceCopilot: upstream.URL}, upstream.Client()))
	mux := testMux()
	h.Routes(mux)

	ask := func(tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"q"}`))
		ctx := middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: "u1", Tenant: tenant, Roles: []string{"analyst"}})
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req.WithContext(ctx))
		return rr
	}

	if rr := ask("t1"); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "grounded") {
		t.Fatalf("same-tenant ask: %d %s", rr.Code, rr.Body.String())
	}
	if rr := ask("t2"); rr.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant ask: status = %d, want 403 passthrough", rr.Code)
	}
}

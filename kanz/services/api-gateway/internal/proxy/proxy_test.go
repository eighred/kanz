package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kanz-eng/kanz/services/api-gateway/internal/middleware"
)

// fakeBackend records the last forwarded request and returns a scripted reply.
type fakeBackend struct {
	last Request
	resp Response
	err  error
}

func (f *fakeBackend) Forward(_ context.Context, req Request) (Response, error) {
	f.last = req
	return f.resp, f.err
}

func authed(req *http.Request, sub, tenant string) *http.Request {
	ctx := middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: sub, Tenant: tenant, Roles: []string{"analyst"}})
	return req.WithContext(ctx)
}

func TestRoutes_NilBackend_503(t *testing.T) {
	h := New(nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/households/h1"},
		{http.MethodGet, "/v1/securities/s1"},
		{http.MethodGet, "/v1/prices/s1"},
		{http.MethodGet, "/v1/exceptions"},
	} {
		rr := httptest.NewRecorder()
		req := authed(httptest.NewRequest(tc.method, tc.path, nil), "u1", "t1")
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status = %d, want 503", tc.method, tc.path, rr.Code)
		}
	}
}

func TestAsk_Unauthenticated_401(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, Body: []byte(`{"answer":"x"}`)}}
	h := New(be)
	mux := http.NewServeMux()
	h.Routes(mux)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"q"}`))
	mux.ServeHTTP(rr, req) // no principal on ctx
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if be.last.Service != "" {
		t.Fatalf("backend was called for an anonymous /v1/ask")
	}
}

func TestAsk_ForwardsPrincipalAndBody(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, ContentType: "application/json", Body: []byte(`{"answer":"42"}`)}}
	h := New(be)
	mux := http.NewServeMux()
	h.Routes(mux)
	rr := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"q"}`)), "u1", "t1")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if be.last.Service != ServiceCopilot {
		t.Fatalf("service = %q, want copilot", be.last.Service)
	}
	if be.last.Principal == nil || be.last.Principal.Subject != "u1" || be.last.Principal.Tenant != "t1" {
		t.Fatalf("principal not forwarded: %+v", be.last.Principal)
	}
	if string(be.last.Body) != `{"question":"q"}` {
		t.Fatalf("body = %q, not forwarded verbatim", be.last.Body)
	}
	if rr.Body.String() != `{"answer":"42"}` {
		t.Fatalf("response body = %q, want upstream body verbatim", rr.Body.String())
	}
}

func TestRead_ForwardsToDataMaster(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, Body: []byte(`[]`)}}
	h := New(be)
	mux := http.NewServeMux()
	h.Routes(mux)
	rr := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/prices/AAPL?as_of=2026-06-30T00:00:00Z", nil), "u1", "t1")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if be.last.Service != ServiceDataMaster {
		t.Fatalf("service = %q, want datamaster", be.last.Service)
	}
	if be.last.Path != "/v1/prices/AAPL" {
		t.Fatalf("path = %q, want /v1/prices/AAPL", be.last.Path)
	}
	if be.last.Query.Get("as_of") != "2026-06-30T00:00:00Z" {
		t.Fatalf("query not forwarded: %v", be.last.Query)
	}
}

func TestForward_BackendUnavailable_503(t *testing.T) {
	be := &fakeBackend{err: ErrBackendUnavailable}
	h := New(be)
	mux := http.NewServeMux()
	h.Routes(mux)
	rr := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/households/h1", nil), "u1", "t1")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestForward_UpstreamFault_502(t *testing.T) {
	be := &fakeBackend{err: context.DeadlineExceeded}
	h := New(be)
	mux := http.NewServeMux()
	h.Routes(mux)
	rr := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/households/h1", nil), "u1", "t1")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

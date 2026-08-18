package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// fakeBackend records the last forwarded request and returns a scripted reply. It also
// records the CONTEXT's deadline, which is the only place a test can observe the bound
// the handler put on the call.
type fakeBackend struct {
	last     Request
	resp     Response
	err      error
	deadline time.Time
	hadDL    bool
	// onForward runs while the call is "in flight", which is the only moment a test can
	// observe the upstream context alive — handle cancels it the instant it returns.
	onForward func(ctx context.Context)
}

func (f *fakeBackend) Forward(ctx context.Context, req Request) (Response, error) {
	f.last = req
	f.deadline, f.hadDL = ctx.Deadline()
	if f.onForward != nil {
		f.onForward(ctx)
	}
	return f.resp, f.err
}

// testMux is the router the gateway actually serves /v1 on (SEC-M2). These are all READ
// routes, so the analyst role that authed() carries is enough — and that is the fix: an
// analyst can read the book and cannot reach POST /v1/orders.
func testMux() *authz.Mux {
	return authz.NewMux(authz.Grants{"analyst": {authz.Read}}, nil)
}

func authed(req *http.Request, sub, tenant string) *http.Request {
	ctx := middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: sub, Tenant: tenant, Roles: []string{"analyst"}})
	return req.WithContext(ctx)
}

func TestRoutes_NilBackend_503(t *testing.T) {
	h := New(nil, Roles{})
	mux := testMux()
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

// TestAsk_Unauthenticated_401 exercises the HANDLER'S OWN requirePrincipal guard, by calling
// it directly rather than through the router. Through the router an anonymous caller is
// refused earlier and differently (403: no principal carries any capability — see
// internal/authz); this asserts the handler does not forward somebody's question upstream
// with nobody attached to it either.
func TestAsk_Unauthenticated_401(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, Body: []byte(`{"answer":"x"}`)}}
	h := New(be, Roles{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"q"}`))
	h.handle(ServiceCopilot, true, nil)(rr, req) // no principal on ctx
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if be.last.Service != "" {
		t.Fatalf("backend was called for an anonymous /v1/ask")
	}
}

func TestAsk_ForwardsPrincipalAndBody(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, ContentType: "application/json", Body: []byte(`{"answer":"42"}`)}}
	h := New(be, Roles{})
	mux := testMux()
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
	h := New(be, Roles{})
	mux := testMux()
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
	h := New(be, Roles{})
	mux := testMux()
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
	h := New(be, Roles{})
	mux := testMux()
	h.Routes(mux)
	rr := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/households/h1", nil), "u1", "t1")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

// TestForward_EveryRouteIsBounded is the regression test for the leak in #235.
//
// A proxied call used to inherit r.Context() unchanged, and that context has NO deadline —
// net/http cancels it when the CLIENT goes away, never when the UPSTREAM stops answering.
// So a wedged wealth or datamaster pod that stopped writing without closing its socket held
// a gateway goroutine and an fd forever, and the gateway that is the sole ingress for
// ORDERS would eventually stop accepting connections because a READ surface was sick.
//
// The assertion is on ctx.Deadline() rather than on elapsed time, deliberately: it proves
// the exact budget for the exact route in microseconds, where a test that actually waited
// out a 30s bound would be the slowest test in the suite and would still only prove that
// SOME bound existed.
func TestForward_EveryRouteIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		body               string
		want               time.Duration
	}{
		{"wealth read", http.MethodGet, "/v1/households/h1", "", readForwardTimeout},
		{"datamaster read", http.MethodGet, "/v1/prices/AAPL", "", readForwardTimeout},
		{"tv-sync broker read", http.MethodGet, "/v1/broker/accounts", "", readForwardTimeout},
		// /v1/ask is not a read: the copilot runs a tool-use turn against a model, so it
		// gets a budget of its own. If this ever collapses to readForwardTimeout, a long
		// ask starts failing at the gateway while the copilot is working correctly.
		{"copilot ask", http.MethodPost, "/v1/ask", `{"question":"q"}`, copilotForwardTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeBackend{resp: Response{Status: 200, Body: []byte(`{}`)}}
			mux := testMux()
			New(be, Roles{}).Routes(mux)

			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			req := authed(httptest.NewRequest(tc.method, tc.path, body), "u1", "t1")
			start := time.Now()
			mux.ServeHTTP(httptest.NewRecorder(), req)

			if be.last.Service == "" {
				t.Fatalf("the backend was never called — this test asserted nothing about the bound")
			}
			if !be.hadDL {
				t.Fatalf("%s %s reached the backend with NO deadline on its context. A wedged "+
					"upstream then holds this gateway's goroutine and socket until it recovers, "+
					"and the gateway is the only path an order has to the spine (#235).",
					tc.method, tc.path)
			}
			// A window, not an equality: the deadline is stamped a little AFTER start, so
			// the measured budget is want plus however long routing took. A second of
			// slack is orders of magnitude more than that, and still far tighter than the
			// gap between the two budgets.
			budget := be.deadline.Sub(start)
			if budget < tc.want || budget > tc.want+time.Second {
				t.Fatalf("%s %s was bounded at ~%s, want ~%s. The per-upstream budgets are "+
					"sized in proxy.forwardBudget and ordered against the server's own "+
					"WriteTimeout by test/arch/probe_deadline_nesting_test.go — changing one "+
					"without the other makes the connection bound the real limit, and that one "+
					"fires by severing the socket with no status and no log line.",
					tc.method, tc.path, budget, tc.want)
			}
		})
	}
}

// TestForward_ClientHangupCancelsTheUpstreamCall pins the OTHER half of the deadline: it
// is derived from r.Context(), not from context.Background().
//
// A budget built on Background would survive the caller. The client hangs up, net/http
// cancels the request context, and the gateway goes on holding an upstream call — and the
// goroutine and fd behind it — for the rest of the budget. That is the same leak the
// deadline was added to close, just slower.
// It has to be asserted from INSIDE Forward. handle cancels its context on return, so
// once ServeHTTP is back every upstream context is cancelled and the check would pass
// whatever the parent was.
func TestForward_ClientHangupCancelsTheUpstreamCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cancelled bool
	be := &fakeBackend{
		resp: Response{Status: 200, Body: []byte(`{}`)},
		onForward: func(upstream context.Context) {
			cancel() // the client hangs up while the upstream call is in flight
			select {
			case <-upstream.Done():
				cancelled = true
			case <-time.After(time.Second):
				cancelled = false
			}
		},
	}
	mux := testMux()
	New(be, Roles{}).Routes(mux)

	req := authed(httptest.NewRequest(http.MethodGet, "/v1/households/h1", nil).WithContext(ctx), "u1", "t1")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if be.last.Service == "" {
		t.Fatalf("the backend was never called — this test asserted nothing")
	}
	if !cancelled {
		t.Fatalf("cancelling the inbound request did NOT cancel the upstream call's context. " +
			"The budget is built on context.Background() somewhere instead of on r.Context(), " +
			"so a caller that hangs up leaves the gateway holding a goroutine and a socket for " +
			"the remainder of the budget — the leak of #235, slowed down rather than fixed.")
	}
}

// TestBroker_AnonymousIsRefused pins the reason tv-sync is behind this gateway.
//
// tv-sync AUTHENTICATES NOTHING. It reads the tenant out of the principal header and
// trusts it, because the gateway is the sole identity authority and the mesh is what
// stops anyone else reaching the service. If a request for somebody's positions could
// reach the backend without a principal, the whole arrangement is decoration — and an
// auth-disabled dev gateway must not be the thing that decides.
func TestBroker_AnonymousIsRefused(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, Body: []byte(`{"positions":[]}`)}}
	mux := testMux()
	New(be, Roles{}).Routes(mux)

	for _, path := range []string{
		"/v1/broker/accounts",
		"/v1/broker/accounts/fund-alpha/positions",
		"/v1/broker/accounts/fund-alpha/orders",
		"/v1/broker/accounts/fund-alpha/executions",
		"/v1/broker/accounts/fund-alpha/state",
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil)) // no principal
		// TWO layers refuse this, and either is a pass. The capability router gets there
		// first (403: nobody carries Read — SEC-M2), and behind it the handler's own
		// requirePrincipal would refuse too (401). What must never happen is a 200.
		if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 401 or 403 — an anonymous caller reached somebody's book", path, rr.Code)
		}
		// THE ASSERTION THAT MATTERS: the request never became an upstream call.
		if be.last.Service != "" {
			t.Fatalf("%s: the backend was called for an ANONYMOUS request", path)
		}
	}
}

// The gateway path is /v1/broker/... (inside the edge chain: version → signing → AUTH
// → metrics → quota). The UPSTREAM path is /broker/... — tv-sync knows nothing about
// /v1. If the prefix were not stripped, every request would 404 at tv-sync; if the
// route lived outside /v1, it would skip the auth chain entirely. Both halves matter.
func TestBroker_ForwardsToTVSyncWithTheStrippedPathAndThePrincipal(t *testing.T) {
	be := &fakeBackend{resp: Response{Status: 200, ContentType: "application/json",
		Body: []byte(`{"positions":[{"instrument":"BTC-USD","qty":"1"}]}`)}}
	mux := testMux()
	New(be, Roles{}).Routes(mux)

	rr := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/broker/accounts/fund-alpha/positions", nil), "alice", "acme")
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if be.last.Service != ServiceTVSync {
		t.Fatalf("forwarded to %q, want tv-sync", be.last.Service)
	}
	if be.last.Path != "/broker/accounts/fund-alpha/positions" {
		t.Fatalf("upstream path = %q — tv-sync serves /broker/..., it has never heard of /v1", be.last.Path)
	}
	if be.last.Principal == nil || be.last.Principal.Tenant != "acme" {
		t.Fatalf("the principal did not reach the upstream: %+v — tv-sync scopes the book by it, "+
			"and it authenticates nothing itself", be.last.Principal)
	}
}

package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identityclient"
	"github.com/eighred/kanz/services/web-bff/internal/clientip"
	"github.com/eighred/kanz/services/web-bff/internal/oidc"
	"github.com/eighred/kanz/services/web-bff/internal/session"
)

// idToken builds an unsigned JWT whose payload carries sub/tenant, so claimsOf
// decodes an identity for the session (the BFF never verifies it — the gateway
// does).
func idToken(sub, tenant string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." +
		enc(map[string]string{"sub": sub, "tenant": tenant}) + "."
}

type harness struct {
	srv     *Server
	gateway *gatewayCapture
}

type gatewayCapture struct {
	srv       *httptest.Server
	auth      string
	path      string
	hadCookie bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	// Fake SSO: discovery + token endpoint.
	var sso *httptest.Server
	ssoMux := http.NewServeMux()
	ssoMux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 sso.URL,
			"authorization_endpoint": sso.URL + "/authorize",
			"token_endpoint":         sso.URL + "/token",
		})
	})
	ssoMux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ACCESS-TOK", "id_token": idToken("u-1", "acme"),
			"token_type": "Bearer", "expires_in": 3600,
		})
	})
	sso = httptest.NewServer(ssoMux)
	t.Cleanup(sso.Close)

	// Fake gateway: record what the proxy forwarded.
	gc := &gatewayCapture{}
	gc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gc.auth = r.Header.Get("Authorization")
		gc.path = r.URL.Path
		_, gc.hadCookie = r.Header["Cookie"]
		_, _ = w.Write([]byte(`{"answer":"ok"}`))
	}))
	t.Cleanup(gc.srv.Close)

	oidcClient, err := oidc.New(oidc.Config{Issuer: sso.URL, ClientID: "kanz-web", RedirectURL: "https://app/auth/callback", Scope: "openid profile"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(&Readiness{}, Options{
		Identity:      testIdentity(t, ""),
		ClientIP:      testClientIP(t),
		OIDC:          oidcClient,
		Sessions:      session.NewManager(time.Hour),
		GatewayURL:    gc.srv.URL,
		SecureCookies: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{srv: srv, gateway: gc}
}

// login drives GET /auth/login then GET /auth/callback and returns the session
// cookie the browser would hold.
func (h *harness) login(t *testing.T) *http.Cookie {
	t.Helper()
	// 1. /auth/login → redirect to SSO authorize; pull the state.
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("/auth/login status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")
	if state == "" || loc.Query().Get("code_challenge") == "" {
		t.Fatalf("authorize redirect missing state/challenge: %s", loc)
	}

	// 2. /auth/callback with a code + the state → sets the session cookie.
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?code=CODE&state="+state, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("/auth/callback status = %d, want 302 (body %s)", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatal("callback did not set a session cookie")
	return nil
}

func TestLoginCallbackProxyFlow(t *testing.T) {
	h := newHarness(t)
	cookie := h.login(t)

	// Proxy an API call with the session cookie.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ask", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if h.gateway.auth != "Bearer ACCESS-TOK" {
		t.Errorf("gateway saw auth = %q, want Bearer ACCESS-TOK", h.gateway.auth)
	}
	if h.gateway.path != "/v1/ask" {
		t.Errorf("gateway saw path = %q, want /v1/ask (/api stripped)", h.gateway.path)
	}
	if h.gateway.hadCookie {
		t.Error("session cookie was forwarded to the gateway — it must not be")
	}
}

func TestProxyRequiresSession(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ask", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated proxy = %d, want 401", rec.Code)
	}
	if h.gateway.auth != "" {
		t.Error("gateway was called for an unauthenticated request")
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?code=CODE&state=forged", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("callback with forged state = %d, want 400", rec.Code)
	}
}

func TestMeAndLogout(t *testing.T) {
	h := newHarness(t)
	cookie := h.login(t)

	// /auth/me reports the decoded identity.
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/me = %d, want 200", rec.Code)
	}
	var me struct{ Subject, Tenant string }
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Subject != "u-1" || me.Tenant != "acme" {
		t.Fatalf("/auth/me = %+v, want u-1/acme", me)
	}

	// Logout clears the session.
	req = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/logout = %d, want 200", rec.Code)
	}
	// The same cookie is now dead.
	req = httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/auth/me after logout = %d, want 401", rec.Code)
	}
}

func TestHealthAndReady(t *testing.T) {
	readiness := &Readiness{}
	srv, err := New(readiness, Options{
		Identity:   testIdentity(t, ""),
		ClientIP:   testClientIP(t),
		OIDC:       mustOIDC(t),
		Sessions:   session.NewManager(time.Hour),
		GatewayURL: "http://gw.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before ready = %d, want 503", rec.Code)
	}
	readiness.Set(true)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz after ready = %d, want 200", rec.Code)
	}
}

func mustOIDC(t *testing.T) *oidc.Client {
	t.Helper()
	c, err := oidc.New(oidc.Config{Issuer: "https://sso.example", ClientID: "kanz-web", RedirectURL: "https://app/cb"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// testClientIP is a resolver that trusts nothing, so tests attribute a request
// to its peer — the safe default, and the one a unit test should exercise.
func testClientIP(t *testing.T) *clientip.Resolver {
	t.Helper()
	r, err := clientip.NewResolver("", nil)
	if err != nil {
		t.Fatalf("clientip.NewResolver: %v", err)
	}
	return r
}

// testIdentity points an identity client at baseURL, or at an address that
// cannot answer when baseURL is empty — the login routes are then wired and
// reachable, which is what New() requires, without pretending a provider exists.
func testIdentity(t *testing.T, baseURL string) *identityclient.Client {
	t.Helper()
	if baseURL == "" {
		baseURL = "http://identity.invalid"
	}
	return identityclient.New(baseURL, "", time.Second)
}

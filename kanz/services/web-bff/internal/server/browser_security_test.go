package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web-bff/internal/session"
)

func TestBrowserBoundaryRejectsBeforeBodySessionOrUpstream(t *testing.T) {
	srv := bffWithIdentity(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	srv.secureCookies = true
	id, err := srv.sessions.Create(session.Session{Subject: "operator", Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		origins []string
		sites   []string
	}{
		{"cross-site", []string{"https://attacker.invalid"}, []string{"cross-site"}},
		{"sibling-site", []string{"https://sibling.example.test"}, []string{"same-site"}},
		{"legacy-other-origin", []string{"https://attacker.invalid"}, nil},
		{"scheme-downgrade", []string{"http://app.example.test"}, nil},
		{"wrong-port", []string{"https://app.example.test:444"}, nil},
		{"opaque", []string{"null"}, nil},
		{"empty-origin", []string{""}, nil},
		{"origin-path", []string{"https://app.example.test/path"}, nil},
		{"origin-userinfo", []string{"https://user@app.example.test"}, nil},
		{"multiple-origin", []string{"https://app.example.test", "https://attacker.invalid"}, nil},
		{"multiple-fetch", nil, []string{"same-origin", "cross-site"}},
		{"inconsistent-fetch", []string{"https://app.example.test"}, []string{"cross-site"}},
		{"inconsistent-origin", []string{"https://attacker.invalid"}, []string{"same-origin"}},
	}
	for _, path := range []string{"/auth/login", "/auth/redeem", "/auth/logout", "/api/identity/invites", "/api/v1/orders", "/api//v1/orders"} {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, "https://app.example.test"+path, nil)
				r.Body = poisonReadCloser{t: t}
				r.Header["Origin"] = tc.origins
				r.Header["Sec-Fetch-Site"] = tc.sites
				r.Header.Set("X-Forwarded-Host", "attacker.invalid")
				r.Header.Set("X-Forwarded-Proto", "http")
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
				w := httptest.NewRecorder()
				srv.ServeHTTP(w, r)
				if w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
					t.Fatalf("status/cookies = %d/%v", w.Code, w.Result().Cookies())
				}
				if _, ok := srv.sessions.Get(id); !ok {
					t.Fatal("rejected request deleted the session")
				}
				assertBrowserHeaders(t, w.Header(), true)
			})
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodTrace} {
		r := httptest.NewRequest(method, "https://app.example.test/api/v1/orders", nil)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Body = poisonReadCloser{t: t}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("unsafe %s = %d", method, w.Code)
		}
	}
}

func assertBrowserHeaders(t *testing.T, h http.Header, secure bool) {
	t.Helper()
	for key, want := range map[string]string{
		"Content-Security-Policy": browserCSP, "X-Content-Type-Options": "nosniff",
		"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
		"Cross-Origin-Opener-Policy": "same-origin", "Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy": "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
	} {
		if values := h.Values(key); len(values) != 1 || values[0] != want {
			t.Errorf("%s = %v, want exactly %q", key, values, want)
		}
	}
	wantHSTS := ""
	if secure {
		wantHSTS = "max-age=31536000"
	}
	if h.Get("Strict-Transport-Security") != wantHSTS {
		t.Errorf("HSTS = %q, want %q", h.Get("Strict-Transport-Security"), wantHSTS)
	}
}

func TestBrowserBoundaryRealTLSConcurrentLogout(t *testing.T) {
	srv := bffWithIdentity(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	srv.secureCookies = true
	network := httptest.NewTLSServer(srv)
	t.Cleanup(network.Close)
	client := network.Client()
	client.Timeout = 5 * time.Second
	id, err := srv.sessions.Create(session.Session{Subject: "operator", Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			r, err := http.NewRequest(http.MethodPost, network.URL+"/auth/logout", strings.NewReader("{}"))
			if err != nil {
				t.Error(err)
				return
			}
			r.Header.Set("Origin", "https://attacker.invalid")
			r.Header.Set("Sec-Fetch-Site", "same-site")
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
			res, err := client.Do(r)
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			_, _ = io.Copy(io.Discard, res.Body)
			if res.StatusCode != http.StatusForbidden || len(res.Cookies()) != 0 {
				t.Errorf("forged logout: %d, cookies %v", res.StatusCode, res.Cookies())
			}
			assertBrowserHeaders(t, res.Header, true)
		})
	}
	wg.Wait()
	if _, ok := srv.sessions.Get(id); !ok {
		t.Fatal("concurrent forged logout destroyed the session")
	}
	r, err := http.NewRequest(http.MethodPost, network.URL+"/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", network.URL)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("legitimate logout = %d", res.StatusCode)
	}
	if _, ok := srv.sessions.Get(id); ok {
		t.Fatal("legitimate logout did not delete the session")
	}
	assertBrowserHeaders(t, res.Header, true)
}

func TestBrowserHeadersCoverStaticErrorsRedirectsAndHTTPDevelopment(t *testing.T) {
	srv, _ := staticBFF(t)
	for _, path := range []string{"/", "/venues", "/assets/app.js", "/assets/missing.js", "/auth/me", "/readyz", "/healthz", "/auth/not-found"} {
		t.Run(path, func(t *testing.T) { assertBrowserHeaders(t, get(t, srv, path).Header(), false) })
	}
	h := newHarness(t)
	assertBrowserHeaders(t, get(t, h.srv, "/auth/login").Header(), false)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r := httptest.NewRequest(method, "/auth/callback", nil)
		r.Header.Set("Origin", "https://sso.example.test")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		h.srv.ServeHTTP(w, r)
		if w.Code == http.StatusForbidden {
			t.Errorf("safe OIDC %s rejected by browser boundary", method)
		}
		assertBrowserHeaders(t, w.Header(), false)
	}
	for _, headers := range []http.Header{
		{"Origin": {"http://localhost:5173"}},
		{"Origin": {"http://localhost:5173"}, "Sec-Fetch-Site": {"same-origin"}},
		{}, // Non-browser clients still require session authentication downstream.
	} {
		r := httptest.NewRequest(http.MethodPost, "http://localhost:5173/auth/logout", nil)
		r.Header = headers
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("development/non-browser logout = %d", w.Code)
		}
	}
}

func TestGatewayCannotOverrideBrowserPolicyOverRealHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src * 'unsafe-inline'")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Strict-Transport-Security", "max-age=0")
		w.Header().Set("Access-Control-Allow-Origin", "https://attacker.invalid")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)
	srv := bffWithIdentity(t, "http://127.0.0.1:1", upstream.URL)
	srv.secureCookies = true
	network := httptest.NewServer(srv)
	t.Cleanup(network.Close)
	id, err := srv.sessions.Create(session.Session{AccessToken: "test-only", Subject: "operator", Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest(http.MethodPost, network.URL+"/api/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", strings.Replace(network.URL, "http:", "https:", 1))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	res, err := network.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("upstream response = %d", res.StatusCode)
	}
	assertBrowserHeaders(t, res.Header, true)
	if values := res.Header.Values("Cache-Control"); len(values) != 1 || values[0] != "no-store" {
		t.Fatalf("authenticated response cache policy = %v", values)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "" || res.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("upstream granted cross-origin access to BFF sessions")
	}
}

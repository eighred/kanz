package server

import (
	"context"
	"encoding/json"
	"github.com/eighred/kanz/services/web-bff/internal/oidc"
	"github.com/eighred/kanz/services/web-bff/internal/session"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionInventoryAndRevocationBoundary(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, e := h.srv.sessions.Create(ctx, session.Session{Subject: "alice", Tenant: "a", AccessToken: "synthetic"})
	if e != nil {
		t.Fatal(e)
	}
	b, e := h.srv.sessions.Create(ctx, session.Session{Subject: "alice", Tenant: "a", AccessToken: "synthetic"})
	if e != nil {
		t.Fatal(e)
	}
	call := func(method, path, cookie, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://app"+path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.srv.ServeHTTP(w, r)
		return w
	}
	w := call("GET", "/auth/sessions", a, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "synthetic") || strings.Contains(w.Body.String(), a) || strings.Contains(w.Body.String(), b) {
		t.Fatal("credential leaked")
	}
	var result struct {
		Sessions []session.Summary `json:"sessions"`
	}
	if e = json.Unmarshal(w.Body.Bytes(), &result); e != nil || len(result.Sessions) != 2 {
		t.Fatal(e)
	}
	var target string
	for _, v := range result.Sessions {
		if !v.Current {
			target = v.ID
		}
	}
	path := "/auth/sessions/" + target + "/revoke"
	if w = call("POST", path, a, "https://evil.test"); w.Code != 403 {
		t.Fatal("CSRF", w.Code)
	}
	if w = call("POST", path, a, "http://app"); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w = call("GET", "/auth/me", b, ""); w.Code != 401 {
		t.Fatal("revoked cookie active", w.Code)
	}
	if w = call("GET", "/auth/me", a, ""); w.Code != 200 {
		t.Fatal("wrong cookie revoked", w.Code)
	}
}
func TestOIDCStateRequiresInitiatingBrowser(t *testing.T) {
	h := newHarness(t)
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, httptest.NewRequest("GET", "/auth/login", nil))
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing state binding")
	}
	path := "/auth/callback?code=CODE&state=" + cookies[0].Value
	w = httptest.NewRecorder()
	h.srv.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 400 {
		t.Fatal("unbound callback accepted")
	}
	r := httptest.NewRequest("GET", path, nil)
	r.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	if w.Code != 302 {
		t.Fatal("forgery consumed valid state", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("callback replay accepted")
	}
}

func TestCallbackNeverPromotesUnsignedIdentityToOwner(t *testing.T) {
	h := newHarness(t)
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			writeJSON(w, 200, map[string]string{"issuer": upstream.URL, "authorization_endpoint": upstream.URL + "/authorize", "token_endpoint": upstream.URL + "/token", "jwks_uri": upstream.URL + "/jwks"})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "synthetic-access", "id_token": "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhZG1pbiIsInRlbmFudCI6InZpY3RpbSJ9.", "expires_in": 3600})
	}))
	defer upstream.Close()
	c, e := oidc.New(oidc.Config{Issuer: upstream.URL, ClientID: "kanz-web", RedirectURL: "http://app/auth/callback"})
	if e != nil {
		t.Fatal(e)
	}
	h.srv.oidc = c
	if e = h.srv.sessions.PutPending(context.Background(), "bound-state", "verifier"); e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("GET", "/auth/callback?code=CODE&state=bound-state", nil)
	r.AddCookie(&http.Cookie{Name: "kanz_login", Value: "bound-state"})
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unsigned ownership accepted", w.Code)
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == sessionCookie && cookie.Value != "" {
			t.Fatal("unsigned identity received session")
		}
	}
}

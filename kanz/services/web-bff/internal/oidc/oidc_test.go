package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeSSO stands in for the Eighred SSO discovery + token endpoints.
type fakeSSO struct {
	srv        *httptest.Server
	issuerOK   bool
	tokenReply map[string]any
	tokenForm  url.Values
}

func newFakeSSO(t *testing.T) *fakeSSO {
	t.Helper()
	f := &fakeSSO{issuerOK: true, tokenReply: map[string]any{
		"access_token": "ACCESS", "refresh_token": "REFRESH", "id_token": "ID",
		"token_type": "Bearer", "expires_in": 3600,
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := f.srv.URL
		if !f.issuerOK {
			iss = "https://elsewhere.example"
		}
		writeJSON(w, map[string]any{
			"issuer":                 iss,
			"authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint":         f.srv.URL + "/token",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.tokenForm = r.PostForm
		writeJSON(w, f.tokenReply)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newClient(t *testing.T, f *fakeSSO) *Client {
	t.Helper()
	c, err := New(Config{Issuer: f.srv.URL, ClientID: "kanz-web", RedirectURL: "https://app/cb", Scope: "openid profile"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewPKCE_ChallengeIsS256OfVerifier(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); p.Challenge != want {
		t.Fatalf("challenge = %q, want S256(verifier) = %q", p.Challenge, want)
	}
	if p2, _ := NewPKCE(); p2.Verifier == p.Verifier {
		t.Fatal("two PKCE verifiers collided — not random")
	}
}

func TestAuthCodeURL(t *testing.T) {
	f := newFakeSSO(t)
	c := newClient(t, f)
	got, err := c.AuthCodeURL(context.Background(), "STATE123", "CHALLENGE")
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id": "kanz-web", "redirect_uri": "https://app/cb", "response_type": "code",
		"state": "STATE123", "code_challenge": "CHALLENGE", "code_challenge_method": "S256",
		"scope": "openid profile",
	} {
		if q.Get(k) != want {
			t.Errorf("authorize %s = %q, want %q", k, q.Get(k), want)
		}
	}
}

func TestExchange(t *testing.T) {
	f := newFakeSSO(t)
	c := newClient(t, f)
	tok, err := c.Exchange(context.Background(), "CODE", "VERIFIER")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "ACCESS" || tok.RefreshToken != "REFRESH" || tok.IDToken != "ID" {
		t.Fatalf("token = %+v", tok)
	}
	if tok.Expiry.IsZero() {
		t.Fatal("expiry not set")
	}
	// The exchange presented the PKCE verifier + auth-code grant.
	if f.tokenForm.Get("grant_type") != "authorization_code" || f.tokenForm.Get("code") != "CODE" ||
		f.tokenForm.Get("code_verifier") != "VERIFIER" || f.tokenForm.Get("redirect_uri") != "https://app/cb" {
		t.Fatalf("token form = %v", f.tokenForm)
	}
}

func TestExchange_ErrorResponse(t *testing.T) {
	f := newFakeSSO(t)
	f.tokenReply = map[string]any{"error": "invalid_grant", "error_description": "bad code"}
	c := newClient(t, f)
	if _, err := c.Exchange(context.Background(), "CODE", "V"); err == nil {
		t.Fatal("expected an error on an OAuth error response")
	}
}

func TestDiscovery_IssuerMismatch(t *testing.T) {
	f := newFakeSSO(t)
	f.issuerOK = false
	c := newClient(t, f)
	if _, err := c.AuthCodeURL(context.Background(), "s", "c"); err == nil {
		t.Fatal("expected an error on issuer mismatch")
	}
}

func TestNew_Validation(t *testing.T) {
	for _, cfg := range []Config{
		{ClientID: "x", RedirectURL: "y"},
		{Issuer: "i", RedirectURL: "y"},
		{Issuer: "i", ClientID: "x"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) should have failed validation", cfg)
		}
	}
}

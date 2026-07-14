package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testSecret = "test-secret"

// mintJWT builds a minimal HS256 token for tests, signed with testSecret.
func mintJWT(t *testing.T, claims jwtClaims) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload := enc(claims)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(header + "." + payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + payload + "." + sig
}

func TestJWTAuthenticator(t *testing.T) {
	a := NewJWTAuthenticator(testSecret)

	t.Run("valid", func(t *testing.T) {
		tok := mintJWT(t, jwtClaims{Subject: "user-1", Tenant: "acme", Roles: []string{"risk.read"}})
		p, err := a.Authenticate(tok)
		if err != nil {
			t.Fatal(err)
		}
		if p.Subject != "user-1" || p.Tenant != "acme" || !p.HasRole("risk.read") {
			t.Errorf("principal = %+v", p)
		}
	})
	t.Run("bad signature", func(t *testing.T) {
		tok := mintJWT(t, jwtClaims{Subject: "u"})
		if _, err := NewJWTAuthenticator("other-secret").Authenticate(tok); err == nil {
			t.Error("want error for wrong-secret signature")
		}
	})
	t.Run("expired", func(t *testing.T) {
		tok := mintJWT(t, jwtClaims{Subject: "u", Expiry: time.Now().Add(-time.Hour).Unix()})
		if _, err := a.Authenticate(tok); err == nil {
			t.Error("want error for expired token")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		if _, err := a.Authenticate("not.a.jwt.at.all"); err == nil {
			t.Error("want error for malformed token")
		}
	})
}

func TestAuthMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	authn := NewJWTAuthenticator(testSecret)

	call := func(mw http.Handler, token string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		mw.ServeHTTP(rr, req)
		return rr.Code
	}

	t.Run("missing token 401", func(t *testing.T) {
		h := Auth(authn, "", nil)(ok)
		if code := call(h, ""); code != http.StatusUnauthorized {
			t.Errorf("code = %d, want 401", code)
		}
	})
	t.Run("valid token passes", func(t *testing.T) {
		h := Auth(authn, "", nil)(ok)
		tok := mintJWT(t, jwtClaims{Subject: "u", Roles: []string{"risk.read"}})
		if code := call(h, tok); code != http.StatusOK {
			t.Errorf("code = %d, want 200", code)
		}
	})
	t.Run("insufficient role 403", func(t *testing.T) {
		h := Auth(authn, "risk.admin", nil)(ok)
		tok := mintJWT(t, jwtClaims{Subject: "u", Roles: []string{"risk.read"}})
		if code := call(h, tok); code != http.StatusForbidden {
			t.Errorf("code = %d, want 403", code)
		}
	})
	t.Run("required role granted", func(t *testing.T) {
		h := Auth(authn, "risk.read", nil)(ok)
		tok := mintJWT(t, jwtClaims{Subject: "u", Roles: []string{"risk.read"}})
		if code := call(h, tok); code != http.StatusOK {
			t.Errorf("code = %d, want 200", code)
		}
	})
	// A nil authenticator means "I cannot establish who you are", which is never
	// "come in". It used to be a pass-through, and POST /v1/orders sits behind
	// this chain. 503, not 401: the fault is the server's, not the caller's.
	t.Run("nil authenticator refuses", func(t *testing.T) {
		var reached bool
		sink := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
		h := Auth(nil, "", nil)(sink)

		req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("code = %d, want 503", rr.Code)
		}
		if reached {
			t.Error("POST /v1/orders reached the handler with no authenticator")
		}
	})
}

// TestAuthStashesPrincipal: a downstream handler sees the principal on ctx
// (the per-tenant rate limiter depends on this).
func TestAuthStashesPrincipal(t *testing.T) {
	var got *Principal
	sink := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = PrincipalFromContext(r.Context())
	})
	h := Auth(NewJWTAuthenticator(testSecret), "", nil)(sink)
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer "+mintJWT(t, jwtClaims{Subject: "u", Tenant: "acme"}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got == nil || got.Tenant != "acme" {
		t.Errorf("principal not stashed: %+v", got)
	}
}

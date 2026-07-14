// Package middleware holds the api-gateway edge controls (API-01d): auth,
// per-tenant rate limiting, idempotency-key dedup, request signing, and API
// version negotiation — each an http.Handler decorator composed by Chain.
package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Principal is the authenticated caller (API-01d). It is a MINIMAL, gateway-
// local identity: AUTH-01a will introduce the canonical kanz/pkg/auth.Principal
// (OIDC subject, tenant, roles, claims) and an OIDC/JWKS Authenticator; this
// middleware consumes the Authenticator interface, so that swap touches only
// the constructor, not the chain.
type Principal struct {
	Subject string
	Tenant  string
	Roles   []string
}

// HasRole reports whether the principal carries role.
func (p *Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

type principalCtxKey struct{}

// WithPrincipal stashes the authenticated principal on ctx; downstream
// middleware (per-tenant rate limiting) and handlers read it.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext returns the authenticated principal, or nil.
func PrincipalFromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalCtxKey{}).(*Principal)
	return p
}

// Authenticator validates a bearer token and returns the caller's Principal.
// AUTH-01a implements this over OIDC/JWKS; JWTAuthenticator is the bundled
// minimal implementation so the gateway is testable + runnable before AUTH-01.
type Authenticator interface {
	Authenticate(token string) (*Principal, error)
}

// ErrUnauthenticated is returned by an Authenticator when the token is
// missing, malformed, expired, or fails verification.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// Auth is the authentication+authorization middleware. It extracts the bearer
// token, authenticates it, requires requiredRole (when non-empty,
// deny-by-default), and stashes the Principal on ctx. A nil authenticator
// REFUSES every request (503): it means this process cannot establish who the
// caller is, and POST /v1/orders sits behind this chain (SEC-M1).
func Auth(authn Authenticator, requiredRole string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// No authenticator means this process cannot establish who the caller
			// is — which is never a reason to let them in. It refuses rather than
			// passing through, so no composition root, present or future, can
			// serve /v1/* anonymously by omitting a check. 503, not 401: nothing
			// is wrong with the caller's credentials; we never had a validator to
			// judge them with, and that is the server's fault to fix.
			if authn == nil {
				writeError(w, http.StatusServiceUnavailable, "authentication unavailable")
				return
			}
			token, ok := bearerToken(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			p, err := authn.Authenticate(token)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "invalid token")
				return
			}
			if requiredRole != "" && !p.HasRole(requiredRole) {
				writeError(w, http.StatusForbidden, "insufficient role")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// JWTAuthenticator is a minimal HS256 JWT validator — the bundled stand-in for
// AUTH-01's OIDC/JWKS integration. It verifies the signature against a shared
// secret and decodes sub/tenant/roles/exp. Deliberately stdlib-only (no JWT
// dep): AUTH-01a swaps in real asymmetric OIDC validation behind the same
// Authenticator interface. NOT for production identity on its own.
type JWTAuthenticator struct {
	secret []byte
	now    func() time.Time
}

// NewJWTAuthenticator returns a validator over the HS256 secret.
func NewJWTAuthenticator(secret string) *JWTAuthenticator {
	return &JWTAuthenticator{secret: []byte(secret), now: time.Now}
}

type jwtClaims struct {
	Subject string   `json:"sub"`
	Tenant  string   `json:"tenant"`
	Roles   []string `json:"roles"`
	Expiry  int64    `json:"exp"`
}

// Authenticate verifies a compact JWS (header.payload.signature), checks the
// HS256 signature + expiry, and maps the claims to a Principal.
func (a *JWTAuthenticator) Authenticate(token string) (*Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrUnauthenticated
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(signing))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	// Constant-time compare guards against signature-timing oracles.
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return nil, ErrUnauthenticated
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrUnauthenticated
	}
	var c jwtClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, ErrUnauthenticated
	}
	if c.Expiry != 0 && a.now().After(time.Unix(c.Expiry, 0)) {
		return nil, ErrUnauthenticated
	}
	if c.Subject == "" {
		return nil, ErrUnauthenticated
	}
	return &Principal{Subject: c.Subject, Tenant: c.Tenant, Roles: c.Roles}, nil
}

var _ Authenticator = (*JWTAuthenticator)(nil)

// writeError writes a JSON error body with the given status. Shared by every
// middleware so the gateway's error shape is uniform.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

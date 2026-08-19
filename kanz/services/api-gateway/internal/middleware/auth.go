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

	"github.com/eighred/kanz/pkg/auth"
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

	// Portfolios is the portfolio allow-list this principal is entitled to, from
	// the token's `portfolios` claim. Roles say WHAT a caller may do; this says
	// WHICH portfolios they may do it to — the gateway carries it onto every
	// order command so the OMS, which holds the order, can authorize the caller
	// it cannot otherwise identify.
	Portfolios []string

	// IssuedAt is when the token was minted (its `iat`), zero when the token
	// carried no such claim.
	//
	// IT IS HERE SO THE REVOCATION CHECK CAN BE WRITTEN ONCE, over both
	// authenticators, instead of once per arm (#532). A revocation mark says
	// "every token for this subject minted before T is refused", so the check is
	// unanswerable without this value — and reading it back out of the raw token
	// at the point of use would mean parsing a credential twice and trusting the
	// second parse.
	//
	// ZERO IS NOT "NOW". It means the claim was absent, and Revoking treats a
	// subject it cannot date as unproven rather than as fresh.
	IssuedAt time.Time
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
				// "COULD NOT JUDGE" IS NOT "JUDGED AND REFUSED" (#532). Both used to
				// answer 401, which the web client acts on by destroying the session
				// and redirecting to log in — for every signed-in user at once, at
				// the exact moment the identity service is unreachable. The two
				// causes are: a JWKS cache past its ceiling (auth.ErrKeysStale, the
				// signing-key axis) and a revocation feed past its ceiling
				// (ErrAuthUnavailable, the per-subject axis).
				//
				// DELIBERATELY NOT LOGGED HERE. This branch runs for EVERY request
				// while the condition lasts, so a log line would be a flood at the
				// worst possible moment — thousands of identical records competing
				// with the incident. The posture is already reported once per cause
				// and where it can be alerted on: kanz_api_gateway_revocations_usable
				// and kanz_api_gateway_oidc_keys_unrevalidated, plus an ERROR per
				// retry from the loops in cmd/api-gateway that are trying to fix it.
				if errors.Is(err, ErrAuthUnavailable) || errors.Is(err, auth.ErrKeysStale) {
					writeError(w, http.StatusServiceUnavailable, "authentication unavailable")
					return
				}
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
// secret and decodes sub/tenant/roles/portfolios, requiring the same claim set
// its OIDC sibling does. Deliberately stdlib-only (no JWT dep): AUTH-01a swaps
// in real asymmetric OIDC validation behind the same Authenticator interface.
// NOT for production identity on its own, and since #242 it is reachable only
// with API_GATEWAY_ALLOW_DEV_HS256=true.
type JWTAuthenticator struct {
	secret []byte
	now    func() time.Time
}

// NewJWTAuthenticator returns a validator over the HS256 secret.
func NewJWTAuthenticator(secret string) *JWTAuthenticator {
	return &JWTAuthenticator{secret: []byte(secret), now: time.Now}
}

// jwtLeeway absorbs clock skew on the exp/nbf comparisons, matching the default
// pkg/auth applies to the OIDC path (OIDCConfig.Leeway). A dev token minted on a
// laptop and checked in a container is exactly where a few seconds of drift
// turns into an unexplainable 401.
const jwtLeeway = time.Minute

// hs256Alg is the ONLY value accepted in the JWS header's `alg`.
//
// This is belt-and-braces, and it is worth saying which part is load-bearing.
// The alg-confusion attack does NOT work against this validator without it: the
// HMAC is computed unconditionally over header.payload, so `alg: none` yields a
// token whose empty signature simply fails hmac.Equal (proven by
// TestJWTAuthenticator_AlgNoneRefused). The check is here so that the refusal is
// a STATED rule rather than an emergent property of the code's shape — the next
// person to add an algorithm branch, a key-id lookup, or an early return for an
// unsigned token has to delete a line that says no, instead of quietly removing
// the accident that was protecting them. Its OIDC sibling states the same rule
// as an allow-list (pkg/auth.asymmetricAlgs).
const hs256Alg = "HS256"

type jwtHeader struct {
	Alg string `json:"alg"`
}

type jwtClaims struct {
	Subject string   `json:"sub"`
	Tenant  string   `json:"tenant"`
	Roles   []string `json:"roles"`
	Issuer  string   `json:"iss"`
	// Audience is RFC 7519's `aud`, which is a string OR an array of strings;
	// see jwtAudience.
	Audience jwtAudience `json:"aud"`
	// Expiry is `exp`, and it is REQUIRED — see Authenticate.
	Expiry int64 `json:"exp"`
	// NotBefore is `nbf`, optional (RFC 7519 §4.1.5) but honoured when present.
	NotBefore int64 `json:"nbf"`
	// Portfolios is auth.ClaimPortfolios — the caller's portfolio entitlement.
	Portfolios []string `json:"portfolios"`
	// IssuedAt is `iat`, optional (RFC 7519 §4.1.6). Decoded because
	// per-subject revocation (#532) dates the token against the subject's
	// revocation mark; 0 means the claim was absent, which the revocation check
	// treats as undatable rather than as fresh.
	IssuedAt int64 `json:"iat"`
}

// jwtAudience decodes `aud` in both RFC 7519 shapes — a bare string and an
// array of strings. Accepting only the shape this estate's own minter emits
// would make a spec-legal token from any other tool fail with the same opaque
// "unauthenticated" as a forged one, which is the kind of refusal that gets a
// check deleted rather than debugged.
type jwtAudience []string

func (a *jwtAudience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = jwtAudience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a jwtAudience) has(want string) bool {
	for _, v := range a {
		if v == want {
			return true
		}
	}
	return false
}

// Authenticate verifies a compact JWS (header.payload.signature), checks the
// HS256 signature and the same claim set the OIDC path checks — alg, iss, aud,
// exp, nbf, sub — and maps the claims to a Principal.
//
// EVERY REJECTION COLLAPSES TO ErrUnauthenticated, deliberately: an
// authentication boundary must not tell a caller which check it failed. Same
// stance, and the same reasoning, as pkg/auth.ErrUnauthenticated.
//
// `exp` IS MANDATORY, AND THAT IS THE #242 FIX. This read `c.Expiry != 0 &&
// …expired`, so a token that simply omitted the claim skipped the check and was
// valid FOREVER — against a symmetric secret with no revocation path, on the
// platform's sole identity authority. Absence must never be the permissive
// case: "no expiry stated" and "expiry checked, and fine" looked identical, and
// the more dangerous of the two was the one that cost nothing to mint.
//
// This comment used to add that "the OIDC sibling gets this right for free
// (jwt.Expected.Time makes exp mandatory)". THAT WAS FALSE, and it is why the
// same defect sat unexamined on the production arm until #367. go-jose checks
// expiry as `c.Expiry != nil && …`, so an absent claim skipped the check there
// exactly as it did here. Both arms now refuse it explicitly — see the matching
// note in pkg/auth/oidc.go.
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
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrUnauthenticated
	}
	var h jwtHeader
	if err := json.Unmarshal(rawHeader, &h); err != nil {
		return nil, ErrUnauthenticated
	}
	if h.Alg != hs256Alg {
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
	// A token with no expiry is a permanent credential. Refuse it before
	// anything else about the claims is considered.
	//
	// It IS belt-and-braces and the honest version of that is worth writing
	// down: the real repair is one line below, where the old `c.Expiry != 0 &&`
	// guard came off the comparison — an absent claim now decodes to 0, which is
	// 1970, which is expired. Removing THIS line alone does not reopen the hole
	// (verified by mutation). It stays because a validator whose handling of a
	// missing claim depends on the epoch happening to be in the past is one
	// refactor from being wrong again, and because the intent should be readable
	// without deriving it.
	if c.Expiry == 0 {
		return nil, ErrUnauthenticated
	}
	now := a.now()
	if now.After(time.Unix(c.Expiry, 0).Add(jwtLeeway)) {
		return nil, ErrUnauthenticated
	}
	// nbf stays OPTIONAL — RFC 7519 makes it so, and unlike a missing exp an
	// absent nbf widens nothing (a token with no not-before is valid from when
	// it was minted, which is what every token here is). Honoured when present:
	// a minter that post-dates a token means it, and accepting it early would
	// silently discard the only forward-dated control this format has.
	if c.NotBefore != 0 && now.Before(time.Unix(c.NotBefore, 0).Add(-jwtLeeway)) {
		return nil, ErrUnauthenticated
	}
	// iss/aud bind the token to THIS credential path. See pkg/auth.DevHS256Issuer
	// for what that does and does not buy against a shared symmetric secret.
	if c.Issuer != auth.DevHS256Issuer {
		return nil, ErrUnauthenticated
	}
	if !c.Audience.has(auth.DevHS256Audience) {
		return nil, ErrUnauthenticated
	}
	if c.Subject == "" {
		return nil, ErrUnauthenticated
	}
	// A ZERO `iat` STAYS ZERO. This arm mints nothing — internal/devtoken does —
	// so a token from an older build may carry no `iat` at all. Defaulting it to
	// now() would date an undatable token as fresh, which is the one answer that
	// would let a revoked token through.
	var issuedAt time.Time
	if c.IssuedAt != 0 {
		issuedAt = time.Unix(c.IssuedAt, 0).UTC()
	}
	return &Principal{
		Subject:    c.Subject,
		Tenant:     c.Tenant,
		Roles:      c.Roles,
		Portfolios: c.Portfolios,
		IssuedAt:   issuedAt,
	}, nil
}

var _ Authenticator = (*JWTAuthenticator)(nil)

// writeError writes a JSON error body with the given status. Shared by every
// middleware so the gateway's error shape is uniform.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

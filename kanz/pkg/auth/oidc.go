package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ErrUnauthenticated is returned when a token is missing, malformed, expired,
// signed by an unknown/unaccepted key, or fails issuer/audience validation.
// The cause is deliberately not surfaced to the caller — an authentication
// boundary should not leak why verification failed to the client.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// ErrKeysStale is returned when the cached JWKS has outlived MaxKeyAge and the
// provider could not be reached to refetch it. It is deliberately NOT
// ErrUnauthenticated: the token itself may be perfectly good, so the caller
// should surface an auth-backend outage (503), not a credential rejection.
//
// Failing closed here is the deliberate trade: an IdP unreachable for longer
// than MaxKeyAge becomes an authentication outage. The alternative — keep
// trusting key material we can no longer vouch for — hands an attacker who can
// keep the JWKS endpoint unreachable an unbounded extension on a revoked key,
// which is exactly the control this cache exists to preserve.
var ErrKeysStale = errors.New("auth: cached signing keys are past MaxKeyAge and the provider is unreachable")

// Authenticator validates a bearer token and returns the caller's Principal.
// It takes a context because verification may need to fetch signing keys from
// the OIDC provider (on a cold cache or after key rotation).
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Principal, error)
}

// asymmetricAlgs is the allow-list of accepted JWS algorithms. It is
// asymmetric-only ON PURPOSE: an OIDC provider signs with its private key and
// publishes the public half via JWKS. Accepting HS* here would open the
// classic alg-confusion attack — a token signed with HMAC using the public key
// as the shared secret would verify. The bundled HS256 stand-in lives only in
// the gateway's dev-mode validator, never here.
var asymmetricAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
}

// OIDCConfig configures an OIDCAuthenticator.
type OIDCConfig struct {
	// Issuer is the OIDC issuer URL; it must match the token `iss` exactly and
	// (unless JWKSURI is set) is used for discovery.
	Issuer string
	// Audience is the expected `aud`; a token must list it.
	Audience string
	// JWKSURI, when set, is used directly and discovery is skipped. Empty ⇒
	// discovered from {Issuer}/.well-known/openid-configuration.
	JWKSURI string
	// TenantClaim names the claim carrying the tenant (default "tenant").
	TenantClaim string
	// RolesClaim names the claim carrying roles (default "roles"); the value
	// may be a JSON array of strings or a single space-delimited string.
	RolesClaim string
	// Leeway absorbs clock skew on exp/nbf checks (default 1m).
	Leeway time.Duration
	// MinRefreshInterval rate-limits forced JWKS refetches triggered by an
	// unknown key id, so a stream of bogus `kid`s can't hammer the provider
	// (default 1m). It is a FLOOR on how often we may refetch.
	MinRefreshInterval time.Duration
	// MaxKeyAge is the CEILING on how long cached key material may be trusted
	// without refetching (default 5m). It is what makes revocation work: a key
	// withdrawn at the provider stops verifying within one MaxKeyAge window
	// even though no unknown `kid` is ever presented to force a cache miss.
	//
	// It must exceed MinRefreshInterval, and NewOIDCAuthenticator refuses a
	// config where it does not: a ceiling below the floor would expire the keys
	// before a refetch is permitted, so every request past the ceiling would be
	// refused by a refresh that the rate limiter has already suppressed.
	MaxKeyAge time.Duration
	// HTTPClient fetches discovery + JWKS documents (default: a 10s client).
	HTTPClient *http.Client
}

// OIDCAuthenticator validates JWTs against the asymmetric signing keys an OIDC
// provider publishes via JWKS. Keys are cached and refetched lazily on an
// unknown `kid` (rate-limited) and unconditionally once the cache passes
// MaxKeyAge, so steady-state verification is fully offline, key rotation needs
// no restart, and key REVOCATION takes effect without one.
type OIDCAuthenticator struct {
	cfg   OIDCConfig
	httpc *http.Client
	now   func() time.Time

	mu      sync.RWMutex
	jwksURI string // resolved (possibly via discovery)
	keys    jose.JSONWebKeySet
	// lastRefresh is the last SUCCESSFUL fetch — it drives staleness, so a
	// failed fetch cannot pass off old keys as fresh. lastAttempt is every
	// fetch, success or not — it drives the rate limit, so a provider that is
	// down is retried once per MinRefreshInterval rather than once per request.
	lastRefresh time.Time
	lastAttempt time.Time
}

var _ Authenticator = (*OIDCAuthenticator)(nil)

// NewOIDCAuthenticator validates the config and returns the authenticator.
// Network calls are deferred to the first Authenticate (or the first cache
// miss), so a momentarily-unreachable provider doesn't fail process startup —
// the same lazy-connect stance the gateway's upstream mTLS dial takes.
func NewOIDCAuthenticator(cfg OIDCConfig) (*OIDCAuthenticator, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("auth: OIDC issuer is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("auth: OIDC audience is required")
	}
	if cfg.TenantClaim == "" {
		cfg.TenantClaim = "tenant"
	}
	if cfg.RolesClaim == "" {
		cfg.RolesClaim = "roles"
	}
	if cfg.Leeway == 0 {
		cfg.Leeway = time.Minute
	}
	if cfg.MinRefreshInterval == 0 {
		cfg.MinRefreshInterval = time.Minute
	}
	if cfg.MaxKeyAge == 0 {
		cfg.MaxKeyAge = 5 * time.Minute
	}
	// The ceiling must sit above the floor or the two fight: keys would expire
	// before the rate limiter permits the refetch that would renew them, and
	// every request past the ceiling would fail ErrKeysStale until the interval
	// elapsed. Refuse at construction rather than thrash in production.
	if cfg.MaxKeyAge <= cfg.MinRefreshInterval {
		return nil, fmt.Errorf("auth: OIDC MaxKeyAge (%s) must exceed MinRefreshInterval (%s)", cfg.MaxKeyAge, cfg.MinRefreshInterval)
	}
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	return &OIDCAuthenticator{cfg: cfg, httpc: httpc, now: time.Now, jwksURI: cfg.JWKSURI}, nil
}

// Authenticate verifies the compact JWS, checks iss/aud/exp/nbf, and maps the
// claims to a Principal. Any verification failure collapses to
// ErrUnauthenticated; transport/provider errors (discovery or JWKS fetch, and
// ErrKeysStale) are returned distinctly so the caller can tell "bad token"
// (401) from "auth backend unavailable" (503).
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, token string) (*Principal, error) {
	tok, err := jwt.ParseSigned(token, asymmetricAlgs)
	if err != nil || len(tok.Headers) != 1 {
		return nil, ErrUnauthenticated
	}
	key, err := a.keyFor(ctx, tok.Headers[0].KeyID)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return nil, ErrUnauthenticated
		}
		return nil, err // provider unavailable — distinct from a bad token
	}

	var std jwt.Claims
	custom := map[string]any{}
	if err := tok.Claims(key, &std, &custom); err != nil {
		return nil, ErrUnauthenticated
	}
	expected := jwt.Expected{
		Issuer:      a.cfg.Issuer,
		AnyAudience: jwt.Audience{a.cfg.Audience},
		Time:        a.now(),
	}
	if err := std.ValidateWithLeeway(expected, a.cfg.Leeway); err != nil {
		return nil, ErrUnauthenticated
	}
	if std.Subject == "" {
		return nil, ErrUnauthenticated
	}

	return &Principal{
		Subject: std.Subject,
		Tenant:  stringClaim(custom[a.cfg.TenantClaim]),
		Roles:   rolesClaim(custom[a.cfg.RolesClaim]),
		Claims:  custom,
	}, nil
}

// keyFor returns the verification key for kid.
//
// Two things force a refetch. An unknown kid (rate-limited) picks up a key
// rotated IN after startup. Cache age past MaxKeyAge drops a key withdrawn at
// the provider — revocation cannot depend on an unknown kid, because after an
// overlap rollout the withdrawn key is one we already hold and no cache miss
// will ever occur. Within the window verification stays fully offline: a
// refetch per request would turn every token check into load on the IdP.
//
// An empty kid is accepted only when the set holds exactly one key (the
// unambiguous single-key case).
func (a *OIDCAuthenticator) keyFor(ctx context.Context, kid string) (any, error) {
	if a.expired() {
		if err := a.refresh(ctx); err != nil {
			return nil, err // provider unavailable — distinct from a bad token
		}
		if a.expired() {
			// The refetch was rate-limited away behind an earlier failure, so
			// the cache is still past its ceiling. Refuse rather than fall back
			// on key material we can no longer vouch for.
			return nil, ErrKeysStale
		}
	}
	if k, ok := a.lookup(kid); ok {
		return k, nil
	}
	if err := a.refresh(ctx); err != nil {
		return nil, err
	}
	if k, ok := a.lookup(kid); ok {
		return k, nil
	}
	return nil, ErrUnauthenticated
}

// expired reports whether the cached key set has outlived MaxKeyAge. A cold
// cache is not "expired" — it is empty, and keyFor's lookup miss already forces
// the first fetch on the path that handles a provider that has never answered.
func (a *OIDCAuthenticator) expired() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.keys.Keys) > 0 && a.now().Sub(a.lastRefresh) >= a.cfg.MaxKeyAge
}

func (a *OIDCAuthenticator) lookup(kid string) (any, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if kid == "" {
		if len(a.keys.Keys) == 1 {
			return a.keys.Keys[0].Key, true
		}
		return nil, false
	}
	if ks := a.keys.Key(kid); len(ks) > 0 {
		return ks[0].Key, true
	}
	return nil, false
}

// refresh resolves the JWKS URI (via discovery on first use) and refetches the
// key set. It is rate-limited: once keys are cached, refetches happen at most
// once per MinRefreshInterval, bounding the load a stream of bogus kids — or a
// provider that is down and re-tried by every expired-cache request — can put
// on the provider.
//
// Returning nil when suppressed is not "refreshed": callers that need fresh
// keys must re-check (see keyFor's second expired() test), because the whole
// point of the limiter is that it sometimes declines to fetch.
func (a *OIDCAuthenticator) refresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.keys.Keys) > 0 && a.now().Sub(a.lastAttempt) < a.cfg.MinRefreshInterval {
		return nil // recently attempted by us or a racing caller; kid is just unknown
	}
	// Recorded before the fetch so a failure still counts against the limiter.
	a.lastAttempt = a.now()
	if a.jwksURI == "" {
		uri, err := a.discover(ctx)
		if err != nil {
			return err
		}
		a.jwksURI = uri
	}
	ks, err := a.fetchJWKS(ctx, a.jwksURI)
	if err != nil {
		return err
	}
	a.keys = ks
	a.lastRefresh = a.now()
	return nil
}

func (a *OIDCAuthenticator) discover(ctx context.Context) (string, error) {
	url := strings.TrimRight(a.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := a.getJSON(ctx, url, &doc); err != nil {
		return "", err
	}
	// The discovered issuer must equal the configured one; a mismatch means we
	// resolved keys for a different issuer than we'll validate tokens against.
	if doc.Issuer != a.cfg.Issuer {
		return "", fmt.Errorf("auth: discovery issuer %q != configured %q", doc.Issuer, a.cfg.Issuer)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("auth: discovery document has no jwks_uri")
	}
	return doc.JWKSURI, nil
}

func (a *OIDCAuthenticator) fetchJWKS(ctx context.Context, uri string) (jose.JSONWebKeySet, error) {
	var ks jose.JSONWebKeySet
	if err := a.getJSON(ctx, uri, &ks); err != nil {
		return jose.JSONWebKeySet{}, err
	}
	if len(ks.Keys) == 0 {
		return jose.JSONWebKeySet{}, errors.New("auth: JWKS has no keys")
	}
	return ks, nil
}

func (a *OIDCAuthenticator) getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func stringClaim(v any) string {
	s, _ := v.(string)
	return s
}

// rolesClaim normalizes the roles claim, which providers encode either as a
// JSON array of strings or a single space-delimited string (the OAuth `scope`
// convention).
func rolesClaim(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.Fields(t)
	default:
		return nil
	}
}

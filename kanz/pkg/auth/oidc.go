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

// ErrKeysStale is returned when the cached JWKS has outlived MaxKeyAge PLUS
// KeyGracePeriod and the provider could not be reached to refetch it. It is
// deliberately NOT ErrUnauthenticated: the token itself may be perfectly good,
// so the caller should surface an auth-backend outage (503), not a credential
// rejection.
//
// Failing closed EVENTUALLY is the deliberate trade. Continuing to trust key
// material we can no longer vouch for, with no bound, would hand an attacker who
// can keep the JWKS endpoint unreachable an unlimited extension on a revoked key
// — exactly the control this cache exists to preserve. Failing closed the
// INSTANT the ceiling passes is the opposite error, and it is the one this
// platform would notice first: the gateway is the sole ingress for orders, so a
// two-minute IdP blip would become a trading outage. KeyGracePeriod is where
// those two meet; see its doc for the window the trade buys an attacker.
var ErrKeysStale = errors.New("auth: cached signing keys are past MaxKeyAge + KeyGracePeriod and the provider is unreachable")

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
	// WITHOUT ATTEMPTING a refetch (default 5m). It is what makes revocation
	// work: a key withdrawn at the provider stops verifying within one MaxKeyAge
	// window even though no unknown `kid` is ever presented to force a cache
	// miss.
	//
	// It must exceed MinRefreshInterval, and NewOIDCAuthenticator refuses a
	// config where it does not: a ceiling below the floor would expire the keys
	// before a refetch is permitted, so every request past the ceiling would be
	// refused by a refresh that the rate limiter has already suppressed.
	MaxKeyAge time.Duration
	// KeyGracePeriod is how much longer past MaxKeyAge the cached keys keep
	// VERIFYING when the refetch could not be made (default 15m). Past
	// MaxKeyAge + KeyGracePeriod, verification fails closed with ErrKeysStale.
	//
	// THIS WIDENS THE REVOCATION WINDOW ON PURPOSE, FROM 5 MINUTES TO 20, AND
	// THAT IS THE WHOLE POINT OF THE SETTING — do not "tidy" it away. A key
	// revoked at the provider is trusted here for at most MaxKeyAge +
	// KeyGracePeriod after the last successful fetch. Bought with those extra
	// fifteen minutes: an IdP that is briefly unreachable stops being an
	// authentication outage on the platform's sole ingress for orders. Refusing
	// every token the moment a 5-minute cache expires means a provider blip, a
	// DNS wobble or an IdP rolling restart halts trading; that failure is far
	// more likely than a key revocation landing inside the same window, and it
	// is not any less severe.
	//
	// THE WINDOW IS BOUNDED AND IT IS VISIBLE. Bounded, because past it this
	// still fails closed — an attacker who can hold the JWKS endpoint down does
	// NOT get an indefinite extension, which was the whole objection to a plain
	// fallback. Visible, because KeysUnrevalidated reports the degraded posture
	// for as long as it lasts and the gateway exports it as
	// kanz_gateway_oidc_keys_unrevalidated, alerted by
	// GatewayOIDCKeysUnrevalidated. A silent widening would be indefensible; an
	// alerted one is a fifteen-minute deadline somebody is told about.
	//
	// Like MaxKeyAge it must exceed MinRefreshInterval, for the same reason one
	// level out: a grace shorter than the refresh floor would elapse before the
	// rate limiter ever permitted the retry that could end it, so the window
	// would exist on paper and never be usable.
	KeyGracePeriod time.Duration
	// HTTPClient fetches discovery + JWKS documents (default: a 10s client).
	HTTPClient *http.Client
}

// The three intervals form ONE ordering, not three independent knobs:
//
//	MinRefreshInterval  <  MaxKeyAge          (the ceiling must clear the floor)
//	MinRefreshInterval  <  KeyGracePeriod     (the grace must outlast the floor)
//
// The defaults are named constants so the relationship can be asserted at
// COMPILE TIME rather than described in a comment somebody edits around — the
// same stance pkg/bus takes for dedupClaimLease vs maxTunedAckWait (#237), and
// the same reason: the pair that must not invert lived in two files as
// unrelated literals, and it inverted.
const (
	defaultMinRefreshInterval = time.Minute
	defaultMaxKeyAge          = 5 * time.Minute
	defaultKeyGracePeriod     = 15 * time.Minute
)

// Compile-time assertions on the DEFAULT ordering. Editing any of the three
// constants into an inverted pair makes one of these expressions negative and
// the package stops compiling. NewOIDCAuthenticator enforces the same two
// relations for CALLER-SUPPLIED values, which a constant cannot see.
const (
	_ = uint(defaultMaxKeyAge - defaultMinRefreshInterval - 1)
	_ = uint(defaultKeyGracePeriod - defaultMinRefreshInterval - 1)
)

// OIDCAuthenticator validates JWTs against the asymmetric signing keys an OIDC
// provider publishes via JWKS. Keys are cached and refetched lazily on an
// unknown `kid` (rate-limited) and unconditionally once the cache passes
// MaxKeyAge, so steady-state verification is fully offline, key rotation needs
// no restart, and key REVOCATION takes effect without one.
//
// When that refetch cannot be made, verification continues on the cached set
// for a bounded KeyGracePeriod and then fails closed (ErrKeysStale), so a brief
// IdP outage is survived rather than turned into an authentication outage on
// the platform's sole ingress. KeysUnrevalidated exports that posture.
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
	// refreshFailed records the OUTCOME of the last attempt that actually went
	// out (an attempt the rate limiter suppressed leaves it alone, because the
	// last thing we learned about the provider is still the last thing we know).
	// It exists so the degraded-posture gauge means "we tried and could not"
	// rather than "we have not tried" — a gateway with no traffic since
	// MaxKeyAge has an old cache and a perfectly healthy IdP, and paging for
	// that at 03:00 is how an alert gets deleted.
	refreshFailed bool
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
		cfg.MinRefreshInterval = defaultMinRefreshInterval
	}
	if cfg.MaxKeyAge == 0 {
		cfg.MaxKeyAge = defaultMaxKeyAge
	}
	if cfg.KeyGracePeriod == 0 {
		cfg.KeyGracePeriod = defaultKeyGracePeriod
	}
	// The ceiling must sit above the floor or the two fight: keys would expire
	// before the rate limiter permits the refetch that would renew them, and
	// every request past the ceiling would fail ErrKeysStale until the interval
	// elapsed. Refuse at construction rather than thrash in production.
	if cfg.MaxKeyAge <= cfg.MinRefreshInterval {
		return nil, fmt.Errorf("auth: OIDC MaxKeyAge (%s) must exceed MinRefreshInterval (%s)", cfg.MaxKeyAge, cfg.MinRefreshInterval)
	}
	// Same relation, one interval out. A grace window shorter than the refresh
	// floor is a window nothing can ever be done in: the first request past
	// MaxKeyAge attempts the refetch, and every request after it is suppressed
	// by the rate limiter until MinRefreshInterval elapses — by which time the
	// grace has already run out. The setting would read as fifteen minutes of
	// tolerance and deliver none, which is worse than not having it.
	if cfg.KeyGracePeriod <= cfg.MinRefreshInterval {
		return nil, fmt.Errorf("auth: OIDC KeyGracePeriod (%s) must exceed MinRefreshInterval (%s) — "+
			"a grace shorter than the refresh floor elapses before a retry is ever permitted",
			cfg.KeyGracePeriod, cfg.MinRefreshInterval)
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
// THE THIRD STATE IS THE GRACE WINDOW. Past MaxKeyAge with a refetch that did
// not happen — the provider is unreachable, or the rate limiter suppressed the
// retry behind an earlier failure — verification CONTINUES on the cached set
// until MaxKeyAge + KeyGracePeriod, and only then fails closed. That is a
// deliberate widening of the revocation window from 5 minutes to 20; the trade
// and its bound are argued at KeyGracePeriod, and the posture is exported for
// as long as it lasts (KeysUnrevalidated).
//
// An empty kid is accepted only when the set holds exactly one key (the
// unambiguous single-key case).
func (a *OIDCAuthenticator) keyFor(ctx context.Context, kid string) (any, error) {
	if a.pastMaxKeyAge() {
		err := a.refresh(ctx)
		switch {
		case err == nil && !a.pastMaxKeyAge():
			// Refetched. The keys are ours to vouch for again.
		case a.pastGrace():
			// The grace is spent. Refuse rather than keep leaning on key
			// material we have now failed to revalidate for twenty minutes.
			if err != nil {
				return nil, err // provider unavailable — distinct from a bad token
			}
			return nil, ErrKeysStale // the retry was rate-limited away
		default:
			// INSIDE THE GRACE WINDOW. Fall through and verify on the cached
			// set. Do NOT collapse this branch into the one above: an
			// authentication outage on the first unreachable-IdP request is
			// what the grace exists to prevent.
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

// pastMaxKeyAge reports whether the cached key set has outlived MaxKeyAge, i.e.
// a refetch is now due. A cold cache is not "past" anything — it is empty, and
// keyFor's lookup miss already forces the first fetch on the path that handles
// a provider that has never answered.
func (a *OIDCAuthenticator) pastMaxKeyAge() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.keys.Keys) > 0 && a.now().Sub(a.lastRefresh) >= a.cfg.MaxKeyAge
}

// pastGrace reports whether the cached key set has outlived MaxKeyAge PLUS
// KeyGracePeriod — the point at which verification fails closed.
func (a *OIDCAuthenticator) pastGrace() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.keys.Keys) > 0 && a.now().Sub(a.lastRefresh) >= a.cfg.MaxKeyAge+a.cfg.KeyGracePeriod
}

// KeysUnrevalidated reports whether this authenticator is RIGHT NOW leaning on
// key material it could not revalidate: the cache is past MaxKeyAge and the
// last fetch that actually went out failed. It is the read behind the gateway's
// kanz_gateway_oidc_keys_unrevalidated gauge.
//
// IT IS A LEVEL, NOT AN EVENT, AND THAT IS THE REQUIREMENT. A counter
// incremented on entering the grace window cannot answer "are we degraded now",
// which is the only question worth asking with a fifteen-minute deadline
// running. A scrape-time read of the live state can, and cannot go stale the
// way a value pushed from the request path does — an IdP outage that also stops
// traffic would freeze a pushed gauge at whatever it last held.
//
// IT STAYS TRUE PAST THE GRACE WINDOW, deliberately. Once MaxKeyAge +
// KeyGracePeriod is spent the gateway is refusing tokens instead of stretching
// them, which is WORSE, not resolved — a gauge that fell back to 0 there would
// clear its own alert at the exact moment the degradation became an outage.
//
// Both conditions are required. Past MaxKeyAge with no failed attempt is a
// quiet gateway whose cache simply aged, not a degraded one; a failed attempt
// inside MaxKeyAge (an unknown-kid refetch that could not reach the provider)
// is not degraded either, because the keys in hand are still ones we vouched
// for inside the ceiling.
func (a *OIDCAuthenticator) KeysUnrevalidated() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.keys.Keys) > 0 && a.refreshFailed &&
		a.now().Sub(a.lastRefresh) >= a.cfg.MaxKeyAge
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
			a.refreshFailed = true
			return err
		}
		a.jwksURI = uri
	}
	ks, err := a.fetchJWKS(ctx, a.jwksURI)
	if err != nil {
		a.refreshFailed = true
		return err
	}
	a.keys = ks
	a.lastRefresh = a.now()
	// Cleared only on a fetch that actually landed — which is what lowers the
	// degraded-posture gauge, per the ruling: "lower the gauge only when a
	// refetch succeeds".
	a.refreshFailed = false
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

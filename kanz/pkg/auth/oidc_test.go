package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// oidcFixture is a fake OIDC provider: an httptest server exposing a discovery
// document + a JWKS whose key set can be rotated between calls.
type oidcFixture struct {
	srv      *httptest.Server
	mu       *atomicJWKS
	jwksHits *int32
}

type atomicJWKS struct{ v atomic.Value } // holds jose.JSONWebKeySet

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	f := &oidcFixture{mu: &atomicJWKS{}, jwksHits: new(int32)}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   f.srv.URL,
			"jwks_uri": f.srv.URL + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(f.jwksHits, 1)
		ks, _ := f.mu.v.Load().(jose.JSONWebKeySet)
		_ = json.NewEncoder(w).Encode(ks)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// signer mints a key pair, publishes the public half under kid, and returns a
// token-signing closure.
type signer struct {
	kid  string
	priv *rsa.PrivateKey
}

func newSigner(t *testing.T, kid string) *signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{kid: kid, priv: k}
}

func (s *signer) jwk() jose.JSONWebKey {
	return jose.JSONWebKey{Key: &s.priv.PublicKey, KeyID: s.kid, Algorithm: "RS256", Use: "sig"}
}

func (s *signer) sign(t *testing.T, std jwt.Claims, custom map[string]any) string {
	t.Helper()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: s.priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", s.kid),
	)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(sig).Claims(std).Claims(custom).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *oidcFixture) publish(keys ...jose.JSONWebKey) {
	f.mu.v.Store(jose.JSONWebKeySet{Keys: keys})
}

func (f *oidcFixture) auth(t *testing.T, mut func(*OIDCConfig)) *OIDCAuthenticator {
	t.Helper()
	cfg := OIDCConfig{Issuer: f.srv.URL, Audience: "kanz-api", HTTPClient: f.srv.Client()}
	if mut != nil {
		mut(&cfg)
	}
	a, err := NewOIDCAuthenticator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// pinClock freezes the authenticator's clock and returns an advance function,
// so key-cache ages can be crossed without sleeping.
func pinClock(a *OIDCAuthenticator) func(time.Duration) {
	var mu sync.Mutex
	at := time.Now()
	a.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return at }
	return func(d time.Duration) { mu.Lock(); defer mu.Unlock(); at = at.Add(d) }
}

func baseClaims(iss string) jwt.Claims {
	now := time.Now()
	return jwt.Claims{
		Issuer:   iss,
		Subject:  "user-1",
		Audience: jwt.Audience{"kanz-api"},
		Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(now),
	}
}

func TestOIDCAuthenticate_Valid(t *testing.T) {
	f := newOIDCFixture(t)
	s := newSigner(t, "k1")
	f.publish(s.jwk())
	a := f.auth(t, nil)

	tok := s.sign(t, baseClaims(f.srv.URL), map[string]any{
		"tenant": "acme",
		"roles":  []any{"risk.reader", "admin"},
	})
	p, err := a.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Subject != "user-1" || p.Tenant != "acme" {
		t.Fatalf("principal = %+v", p)
	}
	if !p.HasRole("admin") || !p.HasRole("risk.reader") || p.HasRole("nope") {
		t.Fatalf("roles = %v", p.Roles)
	}
	if p.Claims["tenant"] != "acme" {
		t.Fatalf("claims not preserved: %v", p.Claims)
	}

	// A second verify with the same kid must hit the cache, not the provider.
	if _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(f.jwksHits); got != 1 {
		t.Fatalf("jwks fetched %d times, want 1 (cached)", got)
	}
}

func TestOIDCAuthenticate_RolesAsScopeString(t *testing.T) {
	f := newOIDCFixture(t)
	s := newSigner(t, "k1")
	f.publish(s.jwk())
	a := f.auth(t, func(c *OIDCConfig) { c.RolesClaim = "scope" })

	tok := s.sign(t, baseClaims(f.srv.URL), map[string]any{"scope": "risk.reader risk.writer"})
	p, err := a.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasRole("risk.reader") || !p.HasRole("risk.writer") {
		t.Fatalf("roles = %v", p.Roles)
	}
}

// THE OIDC PATH IS THE PRODUCTION PATH, AND IT MUST CARRY THE PORTFOLIO SCOPE
// (#225). It decoded roles and tenant and left `portfolios` in the untyped
// Claims bag, so the api-gateway's bridge to its edge Principal dropped the
// caller's entitlement without a compile error or a failing test — while the
// dev HS256 arm carried it and had one.
func TestOIDCAuthenticate_CarriesPortfolioScope(t *testing.T) {
	f := newOIDCFixture(t)
	s := newSigner(t, "k1")
	f.publish(s.jwk())
	a := f.auth(t, nil)

	tok := s.sign(t, baseClaims(f.srv.URL), map[string]any{
		"tenant":     "acme",
		"roles":      []any{"trader"},
		"portfolios": []any{"flagship", "research"},
	})
	p, err := a.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(p.Portfolios) != 2 || p.Portfolios[0] != "flagship" || p.Portfolios[1] != "research" {
		t.Fatalf("portfolios = %v, want [flagship research] — without this the OMS refuses "+
			"every cancel and amend this caller issues", p.Portfolios)
	}

	// Same claim in the space-delimited shape some IdPs emit; one decoder, both
	// shapes, so a provider swap does not silently produce an empty scope.
	tok = s.sign(t, baseClaims(f.srv.URL), map[string]any{
		"tenant":     "acme",
		"portfolios": "flagship research",
	})
	if p, err = a.Authenticate(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	if len(p.Portfolios) != 2 {
		t.Fatalf("space-delimited portfolios = %v, want two entries", p.Portfolios)
	}
}

// ABSENT IS EMPTY, NEVER UNRESTRICTED — the sibling of the dev arm's
// TestJWTAuthenticator_AbsentPortfolioClaimIsEmptyNotUnrestricted. This is the
// live production shape until Eighred SSO issues the claim (#99): a valid token
// with no `portfolios`, which the capital path must read as no entitlement.
func TestOIDCAuthenticate_AbsentPortfolioClaimIsEmptyNotUnrestricted(t *testing.T) {
	f := newOIDCFixture(t)
	s := newSigner(t, "k1")
	f.publish(s.jwk())
	a := f.auth(t, nil)

	tok := s.sign(t, baseClaims(f.srv.URL), map[string]any{"tenant": "acme", "roles": []any{"trader"}})
	p, err := a.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Portfolios) != 0 {
		t.Fatalf("portfolios = %v, want empty — absence must not be widened", p.Portfolios)
	}
	if PortfolioEntitled(p.Portfolios, "flagship") {
		t.Fatal("an absent claim entitled the caller to a portfolio on the capital path")
	}
}

func TestOIDCAuthenticate_Rejections(t *testing.T) {
	f := newOIDCFixture(t)
	s := newSigner(t, "k1")
	other := newSigner(t, "k1") // same kid, different key → bad signature
	f.publish(s.jwk())

	now := time.Now()
	cases := map[string]func() string{
		"wrong-audience": func() string {
			c := baseClaims(f.srv.URL)
			c.Audience = jwt.Audience{"someone-else"}
			return s.sign(t, c, nil)
		},
		"wrong-issuer": func() string {
			c := baseClaims("https://evil.example")
			return s.sign(t, c, nil)
		},
		"expired": func() string {
			c := baseClaims(f.srv.URL)
			c.Expiry = jwt.NewNumericDate(now.Add(-2 * time.Hour))
			return s.sign(t, c, nil)
		},
		"no-subject": func() string {
			c := baseClaims(f.srv.URL)
			c.Subject = ""
			return s.sign(t, c, nil)
		},
		"bad-signature": func() string {
			return other.sign(t, baseClaims(f.srv.URL), nil)
		},
		"malformed":      func() string { return "not.a.jwt" },
		"empty":          func() string { return "" },
		"hs256-rejected": func() string { return hs256Token(t, baseClaims(f.srv.URL)) },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			a := f.auth(t, nil)
			_, err := a.Authenticate(context.Background(), mk())
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("got %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestOIDCAuthenticate_KeyRotation(t *testing.T) {
	f := newOIDCFixture(t)
	k1 := newSigner(t, "k1")
	f.publish(k1.jwk())
	// Tiny refresh interval so an unknown-kid refetch isn't rate-limited.
	a := f.auth(t, func(c *OIDCConfig) { c.MinRefreshInterval = time.Nanosecond })

	if _, err := a.Authenticate(context.Background(), k1.sign(t, baseClaims(f.srv.URL), nil)); err != nil {
		t.Fatal(err)
	}
	// Provider rotates to a new key under a new kid.
	k2 := newSigner(t, "k2")
	f.publish(k2.jwk())
	if _, err := a.Authenticate(context.Background(), k2.sign(t, baseClaims(f.srv.URL), nil)); err != nil {
		t.Fatalf("post-rotation verify failed: %v", err)
	}
}

// A key WITHDRAWN at the provider must stop verifying without a restart. This
// is revocation, and it is a different path from rotation: after an overlap
// rollout the revoked key is one we already hold, so no unknown kid will ever
// force the cache miss that TestOIDCAuthenticate_KeyRotation relies on. Only
// the MaxKeyAge ceiling evicts it.
func TestOIDCAuthenticate_RevokedKeyStopsVerifying(t *testing.T) {
	f := newOIDCFixture(t)
	k1, k2 := newSigner(t, "k1"), newSigner(t, "k2")
	f.publish(k1.jwk(), k2.jwk()) // overlap rollout: both keys live
	a := f.auth(t, func(c *OIDCConfig) {
		c.MinRefreshInterval = time.Minute
		c.MaxKeyAge = 5 * time.Minute
	})
	advance := pinClock(a)

	k1tok := k1.sign(t, baseClaims(f.srv.URL), nil)
	if _, err := a.Authenticate(context.Background(), k1tok); err != nil {
		t.Fatalf("pre-revocation authenticate: %v", err)
	}

	// The provider revokes the compromised k1; only k2 stays published.
	f.publish(k2.jwk())

	// Inside MaxKeyAge the cache is still trusted and must NOT be refetched —
	// a refetch per request would turn every token check into load on the IdP.
	advance(4 * time.Minute)
	if _, err := a.Authenticate(context.Background(), k1tok); err != nil {
		t.Fatalf("within MaxKeyAge: %v", err)
	}
	if got := atomic.LoadInt32(f.jwksHits); got != 1 {
		t.Fatalf("jwks fetched %d times inside MaxKeyAge, want 1 (cached)", got)
	}

	// Past MaxKeyAge the withdrawn key must be gone.
	advance(2 * time.Minute)
	if _, err := a.Authenticate(context.Background(), k1tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked k1 still authenticates past MaxKeyAge (%v): revocation does not take effect", err)
	}
	// ...and the surviving key still verifies, off that same refetch.
	if _, err := a.Authenticate(context.Background(), k2.sign(t, baseClaims(f.srv.URL), nil)); err != nil {
		t.Fatalf("k2 after eviction: %v", err)
	}
	if got := atomic.LoadInt32(f.jwksHits); got != 2 {
		t.Fatalf("jwks fetched %d times, want 2 (one warm-up, one expiry)", got)
	}
}

// A cache past MaxKeyAge + KeyGracePeriod that cannot be refreshed fails
// CLOSED, and says why: continuing to trust keys we can no longer vouch for,
// with no bound, would let an attacker who keeps the JWKS endpoint unreachable
// extend a revoked key indefinitely. The grace is a BOUND, not a fallback.
func TestOIDCAuthenticate_StaleCacheProviderDown(t *testing.T) {
	f := newOIDCFixture(t)
	k1 := newSigner(t, "k1")
	f.publish(k1.jwk())
	a := f.auth(t, func(c *OIDCConfig) {
		c.MinRefreshInterval = time.Minute
		c.MaxKeyAge = 5 * time.Minute
		c.KeyGracePeriod = 15 * time.Minute
	})
	advance := pinClock(a)

	tok := k1.sign(t, baseClaims(f.srv.URL), nil)
	if _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatal(err)
	}

	f.srv.Close() // provider goes down with a warm cache
	advance(21 * time.Minute)

	// The first request past MaxKeyAge + grace attempts a refetch and reports
	// the backend problem — a 503 condition, not a rejected credential.
	_, err := a.Authenticate(context.Background(), tok)
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("got %v, want a provider error distinct from ErrUnauthenticated", err)
	}
	// Later requests inside MinRefreshInterval must still refuse, and must not
	// re-hammer the dead provider. ErrKeysStale is the proof of both: a fetch
	// that had been attempted would have surfaced a transport error instead.
	for i := 0; i < 5; i++ {
		if _, err := a.Authenticate(context.Background(), tok); !errors.Is(err, ErrKeysStale) {
			t.Fatalf("got %v, want ErrKeysStale", err)
		}
	}
}

// THE GRACE WINDOW, END TO END (#242's ruling).
//
// Three claims in one timeline, because they are one behaviour and asserting
// them apart would let a change satisfy each in isolation and none together:
//
//  1. Past MaxKeyAge with the provider down, a good token STILL VERIFIES. This
//     is the ruling's whole subject — a brief IdP outage must not become an
//     authentication outage on the platform's sole ingress for orders.
//  2. While that is true the gauge READS 1, and it reads 1 continuously rather
//     than pulsing once on entry. A counter cannot answer "are we degraded right
//     now", which is the only question worth asking with a 15-minute deadline
//     running, so the assertion is repeated across the window.
//  3. Past MaxKeyAge + KeyGracePeriod it fails closed. The widening is bounded
//     at 20 minutes of total trust; that bound is the reason the widening is
//     defensible at all.
func TestOIDCAuthenticate_GraceWindowSurvivesAnUnreachableIdP(t *testing.T) {
	f := newOIDCFixture(t)
	k1 := newSigner(t, "k1")
	f.publish(k1.jwk())
	a := f.auth(t, func(c *OIDCConfig) {
		c.MinRefreshInterval = time.Minute
		c.MaxKeyAge = 5 * time.Minute
		c.KeyGracePeriod = 15 * time.Minute
	})
	advance := pinClock(a)

	tok := k1.sign(t, baseClaims(f.srv.URL), nil)
	if _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("warm-up authenticate: %v", err)
	}
	if a.KeysUnrevalidated() {
		t.Fatal("gauge reads 1 on a fresh cache with a healthy provider")
	}

	f.srv.Close() // the IdP becomes unreachable with a warm cache

	// Inside MaxKeyAge nothing is even attempted: not degraded, gauge 0.
	advance(4 * time.Minute)
	if _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("inside MaxKeyAge with the IdP down: %v", err)
	}
	if a.KeysUnrevalidated() {
		t.Fatal("gauge reads 1 inside MaxKeyAge — the keys are still ones we vouched for")
	}

	// Past MaxKeyAge, inside the grace. Walked at three separate points across
	// the window (t = 6m, 12m, 19m) rather than sampled once, because "the gauge
	// is 1 WHILE we are degraded" is a different claim from "the gauge went to 1
	// when we became degraded", and only the first one is useful. `advance` is
	// relative, so the steps are deltas from t = 4m.
	for i, step := range []time.Duration{2 * time.Minute, 6 * time.Minute, 7 * time.Minute} {
		advance(step)
		if _, err := a.Authenticate(context.Background(), tok); err != nil {
			t.Fatalf("step %d inside the grace window: %v — the IdP being unreachable "+
				"must not refuse a good token before MaxKeyAge+KeyGracePeriod", i, err)
		}
		if !a.KeysUnrevalidated() {
			t.Fatalf("step %d: gauge reads 0 while verifying on keys that could not be revalidated — "+
				"the degraded posture is invisible, which is the half of the trade that makes the "+
				"widened revocation window defensible", i)
		}
	}

	// t = 19m. Two more minutes takes it past MaxKeyAge(5m) + grace(15m) = 20m.
	advance(2 * time.Minute)
	if _, err := a.Authenticate(context.Background(), tok); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("got %v past MaxKeyAge+KeyGracePeriod, want a provider/stale error — the grace "+
			"must be BOUNDED, or an attacker holding the JWKS endpoint down extends a revoked key forever", err)
	}
	// Still degraded, and the gauge must say so. It does NOT fall back to 0 when
	// the grace runs out: that is the moment the degradation becomes an outage,
	// and a gauge that cleared its own alert there would go quiet at the worst
	// possible time.
	if !a.KeysUnrevalidated() {
		t.Fatal("gauge fell to 0 past the grace window — it must stay 1 while the keys are unrevalidated")
	}
}

// A REFETCH THAT LANDS ENDS THE GRACE AND LOWERS THE GAUGE. The ruling is
// explicit that the posture clears only on a successful fetch, so this drives
// the provider back up and asserts both halves — fresh keys and a gauge at 0.
func TestOIDCAuthenticate_GraceClearsOnASuccessfulRefetch(t *testing.T) {
	f := newOIDCFixture(t)
	k1 := newSigner(t, "k1")
	f.publish(k1.jwk())

	// A switchable transport standing in for an IdP that goes away and comes
	// back: closing the httptest server cannot be undone.
	var down atomic.Bool
	client := &http.Client{Transport: gatedTransport{inner: f.srv.Client().Transport, down: &down}}
	a := f.auth(t, func(c *OIDCConfig) {
		c.MinRefreshInterval = time.Minute
		c.MaxKeyAge = 5 * time.Minute
		c.KeyGracePeriod = 15 * time.Minute
		c.HTTPClient = client
	})
	advance := pinClock(a)

	tok := k1.sign(t, baseClaims(f.srv.URL), nil)
	if _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	down.Store(true)
	advance(6 * time.Minute)
	if _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("inside the grace window: %v", err)
	}
	if !a.KeysUnrevalidated() {
		t.Fatal("gauge reads 0 inside the grace window")
	}

	// The IdP comes back. The next request past MinRefreshInterval refetches.
	down.Store(false)
	advance(2 * time.Minute)
	if _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("after the IdP recovered: %v", err)
	}
	if a.KeysUnrevalidated() {
		t.Fatal("gauge still reads 1 after a successful refetch — the posture never clears, so the " +
			"alert it feeds would fire forever and be muted")
	}
}

// A QUIET GATEWAY IS NOT A DEGRADED ONE. The cache ages past MaxKeyAge with no
// traffic and a perfectly healthy provider; nothing has been attempted, so
// nothing has failed, and the gauge must stay at 0.
//
// This is the false-page case, and it is the reason the gauge is not a plain
// age comparison: a critical alert that fires at 03:00 on an idle estate with
// nothing wrong is how an alerting layer gets muted (alerts/README.md).
func TestOIDCKeysUnrevalidated_QuietGatewayIsNotDegraded(t *testing.T) {
	f := newOIDCFixture(t)
	k1 := newSigner(t, "k1")
	f.publish(k1.jwk())
	a := f.auth(t, func(c *OIDCConfig) {
		c.MinRefreshInterval = time.Minute
		c.MaxKeyAge = 5 * time.Minute
	})
	advance := pinClock(a)

	if _, err := a.Authenticate(context.Background(), k1.sign(t, baseClaims(f.srv.URL), nil)); err != nil {
		t.Fatal(err)
	}
	advance(90 * time.Minute) // no requests at all across the window

	if a.KeysUnrevalidated() {
		t.Fatal("gauge reads 1 on an idle gateway whose provider is fine — \"we have not tried\" is " +
			"not \"we tried and could not\", and paging for the first is how the alert gets deleted")
	}
}

// gatedTransport fails every request while down is set, so a test can take an
// IdP away and bring it back. httptest.Server.Close is one-way.
type gatedTransport struct {
	inner http.RoundTripper
	down  *atomic.Bool
}

func (g gatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if g.down.Load() {
		return nil, errors.New("idp unreachable (test)")
	}
	return g.inner.RoundTrip(r)
}

func TestOIDCAuthenticate_UnknownKidRateLimited(t *testing.T) {
	f := newOIDCFixture(t)
	k1 := newSigner(t, "k1")
	f.publish(k1.jwk())
	a := f.auth(t, func(c *OIDCConfig) {
		c.MinRefreshInterval = time.Hour
		c.MaxKeyAge = 2 * time.Hour      // ceiling must clear the floor
		c.KeyGracePeriod = 2 * time.Hour // and so must the grace
	})

	if _, err := a.Authenticate(context.Background(), k1.sign(t, baseClaims(f.srv.URL), nil)); err != nil {
		t.Fatal(err)
	}
	before := atomic.LoadInt32(f.jwksHits)
	// A bogus kid must not trigger a refetch storm while within the interval.
	bogus := newSigner(t, "bogus")
	for i := 0; i < 5; i++ {
		if _, err := a.Authenticate(context.Background(), bogus.sign(t, baseClaims(f.srv.URL), nil)); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("got %v, want ErrUnauthenticated", err)
		}
	}
	if got := atomic.LoadInt32(f.jwksHits) - before; got != 0 {
		t.Fatalf("rate-limited refetch fired %d times, want 0", got)
	}
}

func TestOIDCAuthenticate_ProviderDownIsNotUnauthenticated(t *testing.T) {
	f := newOIDCFixture(t)
	s := newSigner(t, "k1")
	f.publish(s.jwk())
	a := f.auth(t, nil)
	f.srv.Close() // provider unreachable before any key is cached

	_, err := a.Authenticate(context.Background(), s.sign(t, baseClaims(f.srv.URL), nil))
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("got %v, want a transport error distinct from ErrUnauthenticated", err)
	}
}

func TestNewOIDCAuthenticator_Validation(t *testing.T) {
	if _, err := NewOIDCAuthenticator(OIDCConfig{Audience: "a"}); err == nil {
		t.Fatal("want error for missing issuer")
	}
	if _, err := NewOIDCAuthenticator(OIDCConfig{Issuer: "i"}); err == nil {
		t.Fatal("want error for missing audience")
	}
	// A staleness ceiling at or below the refresh floor is refused at
	// construction: keys would expire before a refetch is permitted, so every
	// request past the ceiling would fail on a refresh already rate-limited away.
	if _, err := NewOIDCAuthenticator(OIDCConfig{Issuer: "i", Audience: "a", MinRefreshInterval: time.Hour}); err == nil {
		t.Fatal("want error for default MaxKeyAge below MinRefreshInterval=1h")
	}
	if _, err := NewOIDCAuthenticator(OIDCConfig{Issuer: "i", Audience: "a", MinRefreshInterval: time.Minute, MaxKeyAge: time.Minute}); err == nil {
		t.Fatal("want error for MaxKeyAge equal to MinRefreshInterval")
	}
	// The SAME relation for the third interval (#242). A grace window shorter
	// than the refresh floor is a window nothing can be done in: the retry that
	// would end it is rate-limited away until after the grace has expired, so
	// the setting reads as tolerance and delivers none.
	if _, err := NewOIDCAuthenticator(OIDCConfig{
		Issuer: "i", Audience: "a",
		MinRefreshInterval: 5 * time.Minute,
		MaxKeyAge:          10 * time.Minute,
		KeyGracePeriod:     time.Minute,
	}); err == nil {
		t.Fatal("want error for KeyGracePeriod below MinRefreshInterval")
	}
	if _, err := NewOIDCAuthenticator(OIDCConfig{
		Issuer: "i", Audience: "a",
		MinRefreshInterval: 5 * time.Minute,
		MaxKeyAge:          10 * time.Minute,
		KeyGracePeriod:     5 * time.Minute,
	}); err == nil {
		t.Fatal("want error for KeyGracePeriod equal to MinRefreshInterval")
	}
	// The defaults must not themselves be in that state. The compile-time
	// assertions in oidc.go already fail the BUILD if the default constants
	// invert; this is the runtime half, over the values a zero-config
	// authenticator actually ends up holding.
	a, err := NewOIDCAuthenticator(OIDCConfig{Issuer: "i", Audience: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.MaxKeyAge <= a.cfg.MinRefreshInterval {
		t.Fatalf("default MaxKeyAge %s must exceed default MinRefreshInterval %s", a.cfg.MaxKeyAge, a.cfg.MinRefreshInterval)
	}
	if a.cfg.KeyGracePeriod <= a.cfg.MinRefreshInterval {
		t.Fatalf("default KeyGracePeriod %s must exceed default MinRefreshInterval %s", a.cfg.KeyGracePeriod, a.cfg.MinRefreshInterval)
	}
}

func TestPrincipalContext(t *testing.T) {
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Fatal("empty ctx should carry no principal")
	}
	ctx := WithPrincipal(context.Background(), &Principal{Subject: "s"})
	p, ok := PrincipalFromContext(ctx)
	if !ok || p.Subject != "s" {
		t.Fatalf("round-trip failed: %v %v", p, ok)
	}
}

// hs256Token mints an HMAC-signed JWT to prove the asymmetric-only allow-list
// rejects it (alg-confusion guard).
func hs256Token(t *testing.T, std jwt.Claims) string {
	t.Helper()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: []byte("shared-secret-shared-secret-1234")},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(sig).Claims(std).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

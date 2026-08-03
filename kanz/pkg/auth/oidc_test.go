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

// A cache past MaxKeyAge that cannot be refreshed fails CLOSED, and says why:
// trusting keys we can no longer vouch for would let an attacker who keeps the
// JWKS endpoint unreachable extend a revoked key indefinitely.
func TestOIDCAuthenticate_StaleCacheProviderDown(t *testing.T) {
	f := newOIDCFixture(t)
	k1 := newSigner(t, "k1")
	f.publish(k1.jwk())
	a := f.auth(t, func(c *OIDCConfig) {
		c.MinRefreshInterval = time.Minute
		c.MaxKeyAge = 5 * time.Minute
	})
	advance := pinClock(a)

	tok := k1.sign(t, baseClaims(f.srv.URL), nil)
	if _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatal(err)
	}

	f.srv.Close() // provider goes down with a warm cache
	advance(6 * time.Minute)

	// The first request past the ceiling attempts a refetch and reports the
	// backend problem — a 503 condition, not a rejected credential.
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

func TestOIDCAuthenticate_UnknownKidRateLimited(t *testing.T) {
	f := newOIDCFixture(t)
	k1 := newSigner(t, "k1")
	f.publish(k1.jwk())
	a := f.auth(t, func(c *OIDCConfig) {
		c.MinRefreshInterval = time.Hour
		c.MaxKeyAge = 2 * time.Hour // ceiling must clear the floor
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
	// The defaults must not themselves be in that state.
	a, err := NewOIDCAuthenticator(OIDCConfig{Issuer: "i", Audience: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.MaxKeyAge <= a.cfg.MinRefreshInterval {
		t.Fatalf("default MaxKeyAge %s must exceed default MinRefreshInterval %s", a.cfg.MaxKeyAge, a.cfg.MinRefreshInterval)
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

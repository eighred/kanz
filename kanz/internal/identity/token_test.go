package identity_test

// TOKEN ISSUANCE, VERIFIED BY THE GATEWAY'S OWN VERIFIER (#364).
//
// These tests do not assert on the JSON this package produces. They mint a
// token, serve the public JWKS on a local HTTP server, and hand the token to a
// real pkg/auth.OIDCAuthenticator — the exact type the gateway constructs. A
// test that inspected my own claims map would agree with itself: every field
// name, the array-vs-string encoding, the `aud` shape, the `kid` lookup and the
// signature algorithm are things the VERIFIER decides, and each of them is a way
// to mint a token that looks perfect and authenticates nobody.
//
// It also settles the thing this file exists for. Signing is ASYMMETRIC: the
// signer holds the private key, the verifier holds only the public half. The
// shortcut was HS256 against the gateway's shared secret, and internal/devtoken
// already records why that must not be the production path — "a shared HMAC
// secret is a symmetric credential every holder can forge with". The last test
// here is the one that proves the property: a token signed by a DIFFERENT key is
// refused, which is what "the gateway cannot mint an operator's token" means in
// practice.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
)

const (
	testIssuer   = "https://identity.kanz.internal"
	testAudience = "kanz"
)

func activeUser() *identity.User {
	return &identity.User{
		Subject:    "user:alice",
		Tenant:     "acme",
		Roles:      []string{"kanz-user", "kanz-trader"},
		Portfolios: []string{"pf-1", "pf-2"},
		Status:     identity.StatusActive,
	}
}

// signerWithJWKS returns a signer and an authenticator pointed at its key set,
// wired exactly as the gateway wires one: Issuer + Audience + JWKSURI, no
// discovery document.
func signerWithJWKS(t *testing.T) (*identity.Signer, *auth.OIDCAuthenticator) {
	t.Helper()
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	s, err := identity.NewSigner(key, testIssuer, testAudience, 0)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s, authenticatorFor(t, s)
}

func authenticatorFor(t *testing.T, s *identity.Signer) *auth.OIDCAuthenticator {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.JWKS())
	}))
	t.Cleanup(srv.Close)

	a, err := auth.NewOIDCAuthenticator(auth.OIDCConfig{
		Issuer:   testIssuer,
		Audience: testAudience,
		JWKSURI:  srv.URL,
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	return a
}

// THE CONTRACT, END TO END.
func TestAMintedTokenAuthenticatesAndCarriesItsClaims(t *testing.T) {
	s, a := signerWithJWKS(t)

	raw, expiry, err := s.Mint(activeUser())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !expiry.After(time.Now()) {
		t.Fatalf("expiry %v is not in the future", expiry)
	}

	p, err := a.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatalf("the gateway's own verifier rejected our token: %v\n\n"+
			"Every field name, the array encoding, the aud shape, the kid and the algorithm are "+
			"the VERIFIER's contract — this is the assertion that they were all met at once.", err)
	}

	if p.Subject != "user:alice" {
		t.Errorf("subject = %q, want user:alice", p.Subject)
	}
	if p.Tenant != "acme" {
		t.Errorf("tenant = %q, want acme — without it every RLS-scoped query runs unscoped or "+
			"not at all, and the order path refuses the caller outright", p.Tenant)
	}
	if strings.Join(p.Roles, ",") != "kanz-user,kanz-trader" {
		t.Errorf("roles = %v, want [kanz-user kanz-trader].\n\n"+
			"Roles must be a JSON ARRAY. The OIDC arm also accepts a space-delimited string, but "+
			"the HS256 arm rejects one outright — minting the shape only one arm reads is how a "+
			"token works in dev and fails in production.", p.Roles)
	}
	if strings.Join(p.Portfolios, ",") != "pf-1,pf-2" {
		t.Errorf("portfolios = %v, want [pf-1 pf-2] — this claim decides which portfolios the "+
			"caller may see, and an empty one DENIES on the capital path", p.Portfolios)
	}
}

// THE PROPERTY THAT MADE THIS ASYMMETRIC.
//
// A token signed by a different key is refused. With HS256 against a shared
// secret there would be no such thing as "a different key": the gateway, and
// anything else holding the secret, could mint this exact token for any subject
// and any role — including kanz-operator.
func TestATokenSignedByAnotherKeyIsRefused(t *testing.T) {
	s, a := signerWithJWKS(t) // `a` trusts s's key only

	otherKey, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	impostor, err := identity.NewSigner(otherKey, testIssuer, testAudience, 0)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	// Same issuer, same audience, same claims — everything except the key.
	raw, _, err := impostor.Mint(activeUser())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, err := a.Authenticate(context.Background(), raw); err == nil {
		t.Fatal("a token signed by a key the verifier does not hold was ACCEPTED.\n\n" +
			"This is the whole argument for asymmetric signing: the verifier must be unable to " +
			"produce, or accept, a token it did not get from the holder of the private key.")
	}
	_ = s
}

// AN EXPIRED TOKEN IS REFUSED — and the reason this is asserted rather than
// assumed: the verifier SKIPS the expiry check entirely when the claim is
// ABSENT, so "sessions expire" is a property of the minter always setting it.
//
// Minted through an injected clock rather than a negative TTL. A non-positive
// TTL falls back to the default, so the negative-duration trick would have
// produced a perfectly valid token and an assertion that passed for the wrong
// reason — which is how this test was written the first time.
func TestAnExpiredTokenIsRefused(t *testing.T) {
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	// Issued far enough in the past that an 8h TTL has lapsed and the verifier's
	// clock-skew leeway cannot cover it.
	past := time.Now().Add(-24 * time.Hour)
	s, err := identity.NewSigner(key, testIssuer, testAudience, 0,
		identity.WithClock(func() time.Time { return past }))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	a := authenticatorFor(t, s)

	raw, expiry, err := s.Mint(activeUser())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// NON-VACUITY: if the clock seam were ignored the expiry would be in the
	// future and the refusal below would prove nothing.
	if expiry.After(time.Now()) {
		t.Fatalf("expiry %v is in the future — the injected clock was ignored, so this test "+
			"would pass against a verifier that never checks expiry at all", expiry)
	}

	if _, err := a.Authenticate(context.Background(), raw); err == nil {
		t.Fatal("an expired token was accepted — a session that never ends is a credential " +
			"theft that never ends with it")
	}
}

// A NON-POSITIVE TTL FALLS BACK TO THE DEFAULT rather than minting a token born
// expired. Zero means "unset" at almost every call site.
func TestANonPositiveTTLFallsBackToTheDefault(t *testing.T) {
	key, _ := identity.GenerateKey()
	for _, ttl := range []time.Duration{0, -time.Hour} {
		s, err := identity.NewSigner(key, testIssuer, testAudience, ttl)
		if err != nil {
			t.Fatalf("NewSigner(%v): %v", ttl, err)
		}
		_, expiry, err := s.Mint(activeUser())
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if !expiry.After(time.Now()) {
			t.Errorf("ttl %v produced a past expiry %v — every session would be born dead", ttl, expiry)
		}
	}
}

// A DISABLED ACCOUNT CANNOT BE MINTED FOR, at the last point that sees the User.
func TestADisabledAccountCannotMint(t *testing.T) {
	s, _ := signerWithJWKS(t)

	u := activeUser()
	u.Status = identity.StatusDisabled
	if _, _, err := s.Mint(u); err == nil {
		t.Fatal("a disabled account was issued a token — disabling an account must end its " +
			"ability to obtain new authority, not merely stop it logging in somewhere else")
	}
	if _, _, err := s.Mint(nil); err == nil {
		t.Fatal("Mint(nil) succeeded")
	}
}

// THE PUBLISHED KEY SET CARRIES NO PRIVATE MATERIAL.
//
// The JWKS is served to anyone who can reach the identity service. A private
// scalar in it hands every reader the ability to mint an operator's token, and
// the leak would be invisible: every token still verifies.
func TestTheJWKSPublishesNoPrivateKey(t *testing.T) {
	s, _ := signerWithJWKS(t)

	body, err := json.Marshal(s.JWKS())
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	var probe struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("unmarshal JWKS: %v", err)
	}
	if len(probe.Keys) != 1 {
		t.Fatalf("JWKS has %d keys, want 1", len(probe.Keys))
	}
	// "d" is the EC private scalar in a JWK. Its presence is the leak.
	if _, leaked := probe.Keys[0]["d"]; leaked {
		t.Fatal("the published JWKS contains \"d\" — the EC PRIVATE SCALAR.\n\n" +
			"Anyone who can fetch it can mint a token for any subject and any role. Every token " +
			"would still verify, so nothing would look wrong.")
	}
	for _, want := range []string{"kty", "crv", "x", "y", "kid"} {
		if _, ok := probe.Keys[0][want]; !ok {
			t.Errorf("JWKS key is missing %q — the verifier looks a token's kid up in this set", want)
		}
	}
	if s.KeyID() == "" {
		t.Error("KeyID is empty — a token with no resolvable kid cannot be verified once a " +
			"second key exists")
	}
}

// THE KEY ID IS STABLE FOR A KEY, so a restart does not invalidate live tokens.
func TestTheKeyIDIsDerivedFromTheKeyNotRandom(t *testing.T) {
	key, _ := identity.GenerateKey()
	a, _ := identity.NewSigner(key, testIssuer, testAudience, 0)
	b, _ := identity.NewSigner(key, testIssuer, testAudience, 0)

	if a.KeyID() != b.KeyID() {
		t.Fatal("two signers over the SAME key advertise different key ids.\n\n" +
			"A restart would then publish a kid that no live token carries, and every session " +
			"issued before it would stop verifying at once.")
	}
	other, _ := identity.GenerateKey()
	c, _ := identity.NewSigner(other, testIssuer, testAudience, 0)
	if c.KeyID() == a.KeyID() {
		t.Fatal("two DIFFERENT keys share a key id — the verifier would pick the wrong one")
	}
}

func TestASignerNeedsAKeyIssuerAndAudience(t *testing.T) {
	key, _ := identity.GenerateKey()
	if _, err := identity.NewSigner(nil, testIssuer, testAudience, 0); err == nil {
		t.Error("NewSigner accepted a nil key")
	}
	if _, err := identity.NewSigner(key, "", testAudience, 0); err == nil {
		t.Error("NewSigner accepted an empty issuer — the verifier requires `iss` to match exactly")
	}
	if _, err := identity.NewSigner(key, testIssuer, "", 0); err == nil {
		t.Error("NewSigner accepted an empty audience — the verifier requires `aud` to contain it")
	}
}

package identity_test

// VERIFYING A TOKEN THIS SERVICE ISSUED (#364).
//
// The identity service is the one service on this platform that cannot sit
// behind the "the gateway is our only reachable caller" NetworkPolicy: /login and
// /invites/redeem exist to be reached by people holding no token. So a
// X-Kanz-Principal-* header on a request to it is a string the caller typed, and
// authenticated provisioning has to check the token itself.
//
// These tests are mostly about the ways a verifier can be made to accept a token
// nobody signed. Each of them produces a token that LOOKS valid.

import (
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/eighred/kanz/internal/identity"
)

var verifyNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func newSigner(t *testing.T, opts ...identity.SignerOption) (*identity.Signer, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	s, err := identity.NewSigner(key, testIssuer, testAudience, time.Hour, opts...)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s, key
}

func TestVerify_AcceptsItsOwnTokenAndCarriesTheClaims(t *testing.T) {
	s, _ := newSigner(t, identity.WithClock(func() time.Time { return verifyNow }))
	raw, _, err := s.Mint(activeUser())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	c, err := s.Verify(raw, verifyNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("Verify rejected a token this signer minted: %v", err)
	}
	if c.Subject != "user:alice" || c.Tenant != "acme" {
		t.Errorf("claims = subject %q tenant %q, want user:alice / acme", c.Subject, c.Tenant)
	}
	if !c.HasRole("kanz-trader") {
		t.Errorf("roles = %v, want kanz-trader present", c.Roles)
	}
	if len(c.Portfolios) != 2 {
		t.Errorf("portfolios = %v, want the two the account carries — an entitlement list that "+
			"does not survive verification silently widens or narrows access", c.Portfolios)
	}
	if c.Expiry.IsZero() {
		t.Error("expiry did not survive verification")
	}
}

// A TOKEN SIGNED BY ANOTHER KEY IS REFUSED. This is what "the gateway cannot
// mint an operator's token" means in practice.
func TestVerify_RefusesATokenSignedByAnotherKey(t *testing.T) {
	s, _ := newSigner(t)
	impostor, _ := newSigner(t)

	raw, _, err := impostor.Mint(activeUser())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(raw, verifyNow); !errors.Is(err, identity.ErrToken) {
		t.Fatalf("a token signed by a different key verified: %v", err)
	}
}

// ===== THE TWO THAT MAKE PUBLISHING /jwks.json SAFE =====

// ALG NONE IS REFUSED.
//
// The classic. A verifier that reads the algorithm out of the token's own header
// is being told by the attacker how to check the attacker's signature, and
// "none" means "do not".
func TestVerify_RefusesAlgNone(t *testing.T) {
	s, _ := newSigner(t)

	b64 := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	unsigned := b64(map[string]string{"alg": "none", "typ": "JWT"}) + "." +
		b64(map[string]any{
			"sub":   "user:mallory",
			"iss":   testIssuer,
			"aud":   testAudience,
			"exp":   verifyNow.Add(time.Hour).Unix(),
			"roles": []string{"kanz-operator"},
		}) + "."

	if _, err := s.Verify(unsigned, verifyNow); err == nil {
		t.Fatal("an UNSIGNED token was accepted — anyone can mint themselves kanz-operator")
	}
}

// AN HS256 TOKEN WHOSE "SECRET" IS THE PUBLIC KEY IS REFUSED.
//
// The attack this service is specifically exposed to: it publishes its public key
// at /jwks.json, on purpose, for the gateway to fetch. A verifier that accepted
// HS256 would make that published key a symmetric secret, and every reader of a
// public endpoint could mint an operator's token.
//
// THIS ASSERTS THE OUTCOME, NOT THE MECHANISM, and that is deliberate. Verify
// refuses this token for two independent reasons — the ES256 pin at parse time,
// and the fact that an *ecdsa.PublicKey cannot be an HMAC secret. A mutation
// confirmed the second alone is sufficient today: widening the pin to accept
// HS256 does NOT make this test pass. Asserting "the pin rejected it" would
// therefore be asserting something that is not what happens.
func TestVerify_RefusesAnHS256TokenSignedWithThePublishedPublicKey(t *testing.T) {
	s, key := newSigner(t)

	// The published public key, exactly as a reader of /jwks.json would get it.
	pub, err := jose.JSONWebKey{Key: key.Public()}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: pub}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(jwt.Claims{
		Subject:  "user:mallory",
		Issuer:   testIssuer,
		Audience: jwt.Audience{testAudience},
		Expiry:   jwt.NewNumericDate(verifyNow.Add(time.Hour)),
	}).Claims(map[string]any{"roles": []string{"kanz-operator"}}).Serialize()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Verify(raw, verifyNow); err == nil {
		t.Fatal("an HS256 token keyed on the PUBLISHED PUBLIC KEY was accepted — /jwks.json becomes " +
			"a public minting oracle for any role the caller types")
	}
}

// ===== EXPIRY =====

func TestVerify_RefusesAnExpiredToken(t *testing.T) {
	s, _ := newSigner(t, identity.WithClock(func() time.Time { return verifyNow }))
	raw, expiry, err := s.Mint(activeUser())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(raw, expiry.Add(-time.Second)); err != nil {
		t.Fatalf("a token one second before expiry was refused: %v", err)
	}
	if _, err := s.Verify(raw, expiry.Add(time.Second)); !errors.Is(err, identity.ErrToken) {
		t.Fatal("an expired token verified")
	}
}

// AND THERE IS NO LEEWAY. go-jose's default is a minute either side, a courtesy
// to clock skew between independent systems. Issuer and verifier are the same
// process here, so leeway only buys a minute of life after expiry.
func TestVerify_ExpiryHasNoLeeway(t *testing.T) {
	s, _ := newSigner(t, identity.WithClock(func() time.Time { return verifyNow }))
	raw, expiry, err := s.Mint(activeUser())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(raw, expiry.Add(30*time.Second)); err == nil {
		t.Fatal("a token 30s past expiry verified — the default one-minute leeway is still in effect")
	}
}

// A TOKEN WITH NO EXPIRY IS REFUSED RATHER THAN ACCEPTED FOREVER.
//
// This is the #367 defect on the arm this service owns: go-jose SKIPS the expiry
// check when the claim is absent, so an unbounded token validates cleanly. Mint
// always sets one, but the mitigation living only in the minter is what #367 was
// about — a verifier that would accept an unbounded token is one caller away from
// being handed one.
func TestVerify_RefusesATokenWithNoExpiryClaim(t *testing.T) {
	s, key := newSigner(t)

	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(jwt.Claims{
		Subject:  "user:alice",
		Issuer:   testIssuer,
		Audience: jwt.Audience{testAudience},
		// No Expiry.
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Verify(raw, verifyNow); !errors.Is(err, identity.ErrToken) {
		t.Fatal("a token with NO expiry claim verified — it would be accepted forever")
	}
}

// ===== ISSUER AND AUDIENCE =====

// A token minted for a DIFFERENT audience does not authorise this service.
// Without the check, a token issued for some other relying party is replayable
// here, which is the whole reason `aud` exists.
func TestVerify_RefusesAnotherIssuerOrAudience(t *testing.T) {
	_, key := newSigner(t)

	for _, c := range []struct{ name, issuer, audience string }{
		{"another issuer", "https://someone-else.example", testAudience},
		{"another audience", testIssuer, "some-other-service"},
	} {
		wrong, err := identity.NewSigner(key, c.issuer, c.audience, time.Hour,
			identity.WithClock(func() time.Time { return verifyNow }))
		if err != nil {
			t.Fatal(err)
		}
		raw, _, err := wrong.Mint(activeUser())
		if err != nil {
			t.Fatal(err)
		}
		// Same KEY, so the signature is valid — only the claims differ.
		right, err := identity.NewSigner(key, testIssuer, testAudience, time.Hour,
			identity.WithClock(func() time.Time { return verifyNow }))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := right.Verify(raw, verifyNow); !errors.Is(err, identity.ErrToken) {
			t.Errorf("%s: a validly SIGNED token for the wrong party verified", c.name)
		}
	}
}

// ===== MALFORMED =====

func TestVerify_RefusesGarbage(t *testing.T) {
	s, _ := newSigner(t)
	for _, raw := range []string{"", "   ", "not.a.token", "a.b", strings.Repeat("x", 500)} {
		if _, err := s.Verify(raw, verifyNow); err == nil {
			t.Errorf("Verify(%.20q) accepted garbage", raw)
		}
	}
}

// ROLES ARE MATCHED EXACTLY, not case-folded.
//
// Deliberately different from the dual-control subject comparison (#410), which
// folds case because a human types their own name. A role is a value this
// platform mints and matches against a fixed set; folding it would silently widen
// the set an operator configured.
func TestClaims_HasRoleIsExact(t *testing.T) {
	c := identity.Claims{Roles: []string{"kanz-operator"}}
	if !c.HasRole("kanz-operator") {
		t.Error("an exact role did not match")
	}
	for _, r := range []string{"KANZ-OPERATOR", "Kanz-Operator", "kanz-operator ", "operator", ""} {
		if c.HasRole(r) {
			t.Errorf("HasRole(%q) matched kanz-operator", r)
		}
	}
	var empty identity.Claims
	if empty.HasRole("kanz-operator") {
		t.Error("a claims value with no roles reported one")
	}
}

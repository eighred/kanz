package middleware

import (
	"testing"
	"time"

	"github.com/eighred/kanz/internal/devtoken"
)

// TestDevTokenAcceptedByGateway is the contract that matters: the gateway now
// refuses to run unauthenticated (SEC-M1), so every local interaction — the dev
// compose stack, the k6 load harness, the in-cluster proof — needs a bearer
// token. A minter whose tokens this gateway rejects is worse than no minter, so
// the test drives the REAL validator the edge chain runs, not a re-implementation
// of it.
func TestDevTokenAcceptedByGateway(t *testing.T) {
	const secret = "dev-secret"

	tok, err := devtoken.Mint(secret, devtoken.Claims{
		Subject:    "dev-user",
		Tenant:     "acme",
		Roles:      []string{"kanz-user"},
		Portfolios: []string{"PF1", "PF2"},
		TTL:        time.Hour,
	})
	if err != nil {
		t.Fatalf("Mint() = %v", err)
	}

	p, err := NewJWTAuthenticator(secret).Authenticate(tok)
	if err != nil {
		t.Fatalf("the gateway rejected a token kanz-devtoken minted: %v", err)
	}
	if p.Subject != "dev-user" || p.Tenant != "acme" {
		t.Errorf("principal = %+v", p)
	}
	if !p.HasRole("kanz-user") {
		t.Error("principal does not carry the role the gateway requires; every /v1 route would 403")
	}
	// THE ORDER PATH, NOT JUST THE READ PATH (#225). The minter had no way to
	// assert a portfolio entitlement at all, so every dev token authenticated
	// perfectly and could not place or pull a single order — the OMS denies an
	// empty allow-list. The claim name is spelled in two places (devtoken's
	// struct tag and the gateway's), which is exactly why this round-trip runs
	// through the real validator instead of asserting on the JSON.
	if len(p.Portfolios) != 2 || p.Portfolios[0] != "PF1" || p.Portfolios[1] != "PF2" {
		t.Fatalf("portfolios = %v, want [PF1 PF2] — a dev token that cannot carry an "+
			"entitlement cannot trade, and the local proof loop stops working", p.Portfolios)
	}
}

// A token minted with no --portfolio must still authenticate, and must still be
// unable to trade. Both halves matter: refusing to mint it would break every read
// harness, and widening the absence to "all portfolios" would make the dev
// credential more powerful than a production one.
func TestDevTokenWithoutPortfolioAuthenticatesButCarriesNoEntitlement(t *testing.T) {
	const secret = "dev-secret"

	tok, err := devtoken.Mint(secret, devtoken.Claims{Subject: "dev", Tenant: "acme", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Mint() = %v", err)
	}
	p, err := NewJWTAuthenticator(secret).Authenticate(tok)
	if err != nil {
		t.Fatalf("a portfolio-less dev token must still authenticate: %v", err)
	}
	if len(p.Portfolios) != 0 {
		t.Fatalf("portfolios = %v, want empty", p.Portfolios)
	}
}

// TestDevTokenExpires: a minted token is not a permanent credential.
func TestDevTokenExpires(t *testing.T) {
	const secret = "dev-secret"

	tok, err := devtoken.Mint(secret, devtoken.Claims{Subject: "dev", Tenant: "acme", TTL: -time.Minute})
	if err != nil {
		t.Fatalf("Mint() = %v", err)
	}
	if _, err := NewJWTAuthenticator(secret).Authenticate(tok); err == nil {
		t.Error("the gateway accepted an expired token")
	}
}

// TestDevTokenRefusesEmptySecret: a token signed with an empty secret is forgeable
// by anyone who knows it is empty, which is everyone reading this repository.
func TestDevTokenRefusesEmptySecret(t *testing.T) {
	if _, err := devtoken.Mint("", devtoken.Claims{Subject: "dev", Tenant: "acme"}); err == nil {
		t.Error("Mint() signed a token with an empty secret")
	}
}

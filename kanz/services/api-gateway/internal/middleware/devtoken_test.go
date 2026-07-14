package middleware

import (
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/devtoken"
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
		Subject: "dev-user",
		Tenant:  "acme",
		Roles:   []string{"kanz-user"},
		TTL:     time.Hour,
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

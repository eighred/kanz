package main

import (
	"testing"

	"github.com/eighred/kanz/pkg/auth"
)

// THE PRODUCTION AUTHENTICATOR HAD NO TEST FILE AT ALL, AND THAT IS HOW #225
// SURVIVED. infra/deploy/api-gateway-deploy.yaml sets API_GATEWAY_OIDC_ISSUER, so
// every deployed request is authenticated on this path — and the mapping onto the
// gateway's edge Principal silently dropped the caller's portfolio entitlement.
// The dev HS256 arm carried the same field and DID have a test, so the mutation
// asymmetry was exact: delete Portfolios from the dev arm and a test fails;
// delete it from this one and nothing did.
func TestEdgePrincipal_CarriesEveryFieldOfTheAuthenticatedPrincipal(t *testing.T) {
	shared := &auth.Principal{
		Subject:    "user-1",
		Tenant:     "acme",
		Roles:      []string{"trader", "kanz-user"},
		Portfolios: []string{"flagship", "research"},
		Claims:     map[string]any{"tenant": "acme"},
	}
	p := edgePrincipal(shared)

	if p.Subject != "user-1" {
		t.Errorf("subject = %q, want user-1", p.Subject)
	}
	if p.Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", p.Tenant)
	}
	if len(p.Roles) != 2 || p.Roles[0] != "trader" {
		t.Errorf("roles = %v, want [trader kanz-user]", p.Roles)
	}
	if len(p.Portfolios) != 2 || p.Portfolios[0] != "flagship" || p.Portfolios[1] != "research" {
		t.Fatalf("portfolios = %v, want [flagship research]. This is the #225 defect: the "+
			"gateway binds this list onto every order command (orders.go bindMetadata), and "+
			"without it the OMS refuses every cancel and amend NOT_ENTITLED while admitting a "+
			"submit into any portfolio in the tenant", p.Portfolios)
	}
}

// An absent claim must arrive as an EMPTY scope, never as "unrestricted". The
// OMS reads empty as no entitlement and refuses — loudly, on the first command —
// which is the intended behaviour until the IdP issues the claim (#99).
func TestEdgePrincipal_AbsentScopeIsEmptyNotUnrestricted(t *testing.T) {
	p := edgePrincipal(&auth.Principal{Subject: "user-1", Tenant: "acme", Roles: []string{"trader"}})
	if len(p.Portfolios) != 0 {
		t.Fatalf("portfolios = %v, want empty", p.Portfolios)
	}
}

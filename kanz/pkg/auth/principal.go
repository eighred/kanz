// Package auth is the platform-wide authentication/authorization layer
// (AUTH-01). It turns a bearer token into a typed Principal (subject, tenant,
// roles, claims) carried on the request context, the single identity every
// query/command authorizes against. AUTH-01a provides the OIDC/JWKS
// Authenticator; AUTH-01b layers deny-by-default authorization over it.
package auth

import "context"

// Principal is the authenticated caller. It is the canonical identity the whole
// platform shares: the api-gateway edge (API-01d), the authorization layer
// (AUTH-01b), and command-issuer binding (AUTH-01c) all read it from ctx.
type Principal struct {
	// Subject is the OIDC `sub` — the stable, unique principal identifier.
	Subject string
	// Tenant is the tenancy boundary (MT-01) the caller belongs to, sourced
	// from a configurable claim.
	Tenant string
	// Roles are the coarse RBAC roles asserted by the token.
	Roles []string
	// Portfolios is the ABAC portfolio allow-list from the ClaimPortfolios
	// claim, normalized once by the authenticator. Roles say WHAT a caller may
	// do; this says WHICH portfolios they may do it to.
	//
	// IT IS A FIELD AND NOT A Claims LOOKUP, AND THAT IS THE #225 REPAIR. The
	// same fact had two shapes — this list, and Claims["portfolios"] holding an
	// untyped []any that each reader re-normalized — so the api-gateway's OIDC
	// bridge could map "the principal" onto its edge type, satisfy the compiler,
	// and silently drop the entitlement. A field is dropped visibly: the
	// completeness guard in test/arch reads the struct literal.
	//
	// EMPTY IS NOT "ALL", AND IT IS NOT "NONE" EITHER — the two consumers answer
	// it differently and deliberately. Never test it by hand; call
	// PortfolioInScope (read path) or PortfolioEntitled (capital path), which
	// carry the argument for each.
	Portfolios []string
	// Claims is the full decoded custom claim set, so authorization policy
	// (AUTH-01b) can read attributes beyond roles/tenant without re-parsing.
	// Promoted claims (Roles, Tenant, Portfolios) have a typed field and MUST be
	// read from it — a second reader keyed on the map is how the two
	// representations drifted apart in the first place.
	Claims map[string]any
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

// WithPrincipal stashes the authenticated principal on ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext returns the authenticated principal and whether one was
// present. A false ok means the request was never authenticated — callers must
// treat that as anonymous, never as a zero-value principal.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	return p, ok && p != nil
}

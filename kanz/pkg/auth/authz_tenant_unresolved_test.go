package auth

import (
	"context"
	"strings"
	"testing"
)

// AN UNRESOLVED RESOURCE TENANT IS A REFUSAL, NOT A SKIPPED BOUNDARY (#741).
//
// The cross-tenant gate used to read `if req.Resource.Tenant != ""`, on the
// reading that a resource with no tenant is not tenant-scoped. That reading is
// right for a request naming no resource and wrong for one naming a portfolio:
// "" is exactly what a caller produces when its ownership lookup failed, and the
// copilot produced it on EVERY error from the governed read surface. A deadline
// or a 500 therefore removed the tenant boundary for that invocation, while
// portfolio scope and RBAC passed on their own terms and the read went ahead.
//
// These tests pin the three halves of the repair: the typed resource is refused,
// the untyped one still isn't (the health/capability path the old comment was
// actually describing), and each refusal carries a Code a caller can switch on
// without reading English.

func unresolvedAuthorizer(t *testing.T) *PolicyAuthorizer {
	t.Helper()
	return NewPolicyAuthorizer(testPolicy(t))
}

// A typed resource with no tenant is refused, and the refusal is classed as
// isolation — which is what tells a caller to render it as "no such resource"
// rather than "you lack a grant".
func TestAuthorize_TypedResourceWithNoTenantIsRefusedAsIsolation(t *testing.T) {
	az := unresolvedAuthorizer(t)
	// A wildcard admin, in scope, with the action granted: every gate except
	// isolation says yes. If this allows, the boundary is gone.
	d := az.Authorize(context.Background(), Request{
		Principal: &Principal{Subject: "u", Tenant: "acme", Roles: []string{"risk.admin"}},
		Action:    ActionRiskRead,
		Resource:  Resource{Type: ResourcePortfolio, ID: "pf-1"},
	})
	if d.Allow {
		t.Fatalf("a portfolio with no owning tenant was ALLOWED — the cross-tenant boundary is "+
			"skippable by failing the ownership lookup (#741). reason: %s", d.Reason)
	}
	if !d.Code.IsIsolation() || d.Code != DenyResourceTenantUnresolved {
		t.Fatalf("code = %q, want %q (an isolation class): a caller that cannot tell this from a "+
			"missing grant will render it as one, and hand back a cross-tenant existence oracle",
			d.Code, DenyResourceTenantUnresolved)
	}
	// The reason is for the audit trail; it must still say what happened.
	if !strings.Contains(d.Reason, "no tenant") {
		t.Errorf("reason %q does not say the tenant was missing", d.Reason)
	}
}

// The case the old comment was really about: a request that addresses no
// resource at all is still tenant-agnostic and decided on the grant alone.
func TestAuthorize_UntypedResourceStaysTenantAgnostic(t *testing.T) {
	az := unresolvedAuthorizer(t)
	d := az.Authorize(context.Background(), Request{
		Principal: &Principal{Subject: "u", Tenant: "acme", Roles: []string{"risk.reader"}},
		Action:    ActionRiskRead,
		Resource:  Resource{}, // no resource named
	})
	if !d.Allow {
		t.Fatalf("a request naming NO resource was denied (%s) — the repair was meant to catch a "+
			"typed resource with a missing tenant, not to make every non-resource call fail", d.Reason)
	}
	if d.Code != DenyNone {
		t.Errorf("allow carried code %q, want the zero value", d.Code)
	}
}

// Every refusal path carries a distinct, non-empty code. A caller branching on
// Code cannot be silently routed into the default arm by a new refusal that
// forgot to name itself.
func TestAuthorize_EveryRefusalCarriesADistinctCode(t *testing.T) {
	az := unresolvedAuthorizer(t)
	owned := Resource{Type: ResourcePortfolio, ID: "pf-1", Tenant: "acme"}

	cases := []struct {
		name string
		req  Request
		want DenyCode
	}{
		{"nil principal", Request{Action: ActionRiskRead, Resource: owned}, DenyNoPrincipal},
		{"empty action", Request{Principal: &Principal{Tenant: "acme", Roles: []string{"risk.admin"}}, Resource: owned}, DenyEmptyAction},
		{"principal without tenant", Request{Principal: &Principal{Subject: "u", Roles: []string{"risk.admin"}}, Action: ActionRiskRead, Resource: owned}, DenyPrincipalNoTenant},
		{"resource tenant unresolved", Request{Principal: &Principal{Subject: "u", Tenant: "acme", Roles: []string{"risk.admin"}}, Action: ActionRiskRead, Resource: Resource{Type: ResourcePortfolio, ID: "pf-1"}}, DenyResourceTenantUnresolved},
		{"cross tenant", Request{Principal: &Principal{Subject: "u", Tenant: "acme", Roles: []string{"risk.admin"}}, Action: ActionRiskRead, Resource: Resource{Type: ResourcePortfolio, ID: "pf-1", Tenant: "globex"}}, DenyCrossTenant},
		{"portfolio out of scope", Request{Principal: &Principal{Subject: "u", Tenant: "acme", Roles: []string{"risk.admin"}, Portfolios: []string{"pf-9"}}, Action: ActionRiskRead, Resource: owned}, DenyPortfolioOutOfScope},
		{"no grant", Request{Principal: &Principal{Subject: "u", Tenant: "acme", Roles: []string{"nobody"}}, Action: ActionRiskRead, Resource: owned}, DenyNoGrant},
	}

	seen := map[DenyCode]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := az.Authorize(context.Background(), tc.req)
			if d.Allow {
				t.Fatalf("expected a refusal, got allow")
			}
			if d.Code != tc.want {
				t.Fatalf("code = %q, want %q", d.Code, tc.want)
			}
			if d.Reason == "" {
				t.Errorf("refusal carries no reason — the audit trail loses the why")
			}
		})
		if prev, dup := seen[tc.want]; dup {
			t.Errorf("code %q is shared by %q and %q — a caller cannot tell them apart", tc.want, prev, tc.name)
		}
		seen[tc.want] = tc.name
	}

	// ONLY THE ISOLATION CODES ANSWER TRUE. IsIsolation is what a caller uses to
	// decide whether a refusal must be flattened into "no such resource"; if a
	// grant refusal ever answered true, every out-of-scope portfolio would start
	// reporting as missing and the copilot would lie to its own tenant.
	for code, name := range seen {
		want := code == DenyCrossTenant || code == DenyResourceTenantUnresolved
		if code.IsIsolation() != want {
			t.Errorf("%s: %q.IsIsolation() = %v, want %v", name, code, code.IsIsolation(), want)
		}
	}
}

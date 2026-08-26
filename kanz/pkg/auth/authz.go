package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Action is the operation a principal attempts (e.g. "risk.read"). It is a free
// string so new operations need no code change here — only a policy-bundle
// grant. Well-known actions on the risk read surface are declared below.
type Action string

// Well-known actions + resource type for the risk query surface (API-01b/c).
const (
	ActionRiskRead     Action = "risk.read"
	ActionRiskScenario Action = "risk.scenario"

	ResourcePortfolio = "portfolio"
)

// ClaimPortfolios is the token claim carrying the portfolio-id allow-list for
// ABAC portfolio scoping. It is decoded ONCE, by the authenticator, into
// Principal.Portfolios; read that field, never this key (#225).
//
// WHAT ABSENT MEANS DEPENDS ON THE PATH and is not stated here on purpose — see
// PortfolioInScope and PortfolioEntitled in portfolio.go, which disagree
// deliberately and carry the argument.
const ClaimPortfolios = "portfolios"

// Resource is the thing being acted upon, carrying the attributes the decision
// keys on.
//
// A RESOURCE THAT NAMES A TYPE MUST NAME ITS TENANT. Authorize refuses a typed
// resource whose Tenant is empty (DenyResourceTenantUnresolved) rather than
// treating it as tenant-agnostic — do not pass "" as a stand-in for an ownership
// lookup that failed. A request addressing no resource at all leaves the whole
// struct zero.
type Resource struct {
	Type   string
	ID     string
	Tenant string
}

// Request is an authorization query: Principal wants to perform Action on Resource.
type Request struct {
	Principal *Principal
	Action    Action
	Resource  Resource
}

// DenyCode names WHY authorization refused, as a closed set a caller can switch
// on. It exists because a caller must be able to tell an ISOLATION refusal from
// an ordinary "your role does not carry this" — and must do so without parsing
// Reason, which is an English sentence written for a human reading the audit
// trail and is free to change wording.
//
// The distinction is not cosmetic. An isolation refusal has to be rendered back
// to the caller as "there is nothing here", because a caller that can tell
// "another tenant owns this" apart from "this does not exist" holds a
// cross-tenant existence oracle (#741). A missing-grant refusal is about the
// caller's own token and may be stated plainly.
type DenyCode string

const (
	// DenyNone is the zero value, carried by every allow.
	DenyNone DenyCode = ""
	// DenyNoPrincipal — the request carried no authenticated principal.
	DenyNoPrincipal DenyCode = "no_principal"
	// DenyEmptyAction — the request named no action.
	DenyEmptyAction DenyCode = "empty_action"
	// DenyPrincipalNoTenant — the principal itself carries no tenant.
	DenyPrincipalNoTenant DenyCode = "principal_no_tenant"
	// DenyCrossTenant — the resource belongs to a tenant other than the
	// principal's. An isolation refusal.
	DenyCrossTenant DenyCode = "cross_tenant"
	// DenyResourceTenantUnresolved — a typed resource arrived carrying no
	// tenant, so isolation could not be established at all. An isolation
	// refusal: it is the UNKNOWN case, and a critical unknown fails closed.
	DenyResourceTenantUnresolved DenyCode = "resource_tenant_unresolved"
	// DenyPortfolioOutOfScope — the portfolio is not on the principal's
	// allow-list.
	DenyPortfolioOutOfScope DenyCode = "portfolio_out_of_scope"
	// DenyNoGrant — no role the principal holds grants the action.
	DenyNoGrant DenyCode = "no_grant"
)

// IsIsolation reports whether the refusal was the tenant boundary rather than a
// grant. A caller rendering a refusal to an untrusted reader MUST collapse these
// to the same answer it gives for "no such resource"; see DenyCode.
func (c DenyCode) IsIsolation() bool {
	return c == DenyCrossTenant || c == DenyResourceTenantUnresolved
}

// Decision is the authorization outcome.
//
// REASON IS AUDIT-FACING AND ONLY AUDIT-FACING. It is what AUTH-01d logs into
// the observation stream for every allow AND deny, so the "why" of a decision is
// always reconstructable — which is exactly why it names the resource's owning
// tenant on a cross-tenant deny. That makes it unsafe to return to the caller
// being denied: concatenated into a copilot tool result it published one
// tenant's id into another tenant's model context (#741). Branch on Code and
// render your own fixed text; log Reason.
type Decision struct {
	Allow  bool
	Reason string
	// Code is the machine-readable refusal class, DenyNone on an allow.
	Code DenyCode
}

// Authorizer renders a deny-by-default authorization decision.
type Authorizer interface {
	Authorize(ctx context.Context, req Request) Decision
}

// Policy is the declarative authorization bundle: RBAC role→action grants. The
// ABAC dimensions (tenant isolation, portfolio scope) are structural invariants
// enforced in code, NOT policy data — cross-tenant access is an isolation
// boundary (MT-01), never a tunable grant, so it must not be expressible as a
// policy that an operator could loosen by mistake.
type Policy struct {
	// Roles maps a role name to the actions it grants. The wildcard action "*"
	// grants every action (an admin role).
	Roles map[string][]Action `json:"roles"`
}

// LoadPolicy decodes a JSON policy bundle.
func LoadPolicy(r io.Reader) (*Policy, error) {
	var p Policy
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("auth: decode policy: %w", err)
	}
	if len(p.Roles) == 0 {
		return nil, fmt.Errorf("auth: policy has no roles")
	}
	return &p, nil
}

// LoadPolicyFile loads a JSON policy bundle from a file — the deployment shape,
// where the bundle is a ConfigMap mounted into the service (so policy changes
// ship without a rebuild, mirroring the SEC-01d CSI secret-file seam).
func LoadPolicyFile(path string) (*Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return LoadPolicy(f)
}

// PolicyAuthorizer evaluates RBAC grants from a Policy plus the ABAC tenant +
// portfolio scope. It is the concrete AUTH-01b authorizer; the Authorizer
// interface is the seam a Rego/Cedar engine could slot behind if policy ever
// outgrows this model. Hand-rolled over embedding OPA/Cedar deliberately: the
// decision is structurally narrow (tenant isolation + role→action grants +
// portfolio scope), so a full policy runtime would be dependency bloat (the
// same call the gateway's hand-rolled rate limiter and REST transcoding made).
type PolicyAuthorizer struct {
	policy *Policy
}

// NewPolicyAuthorizer builds an authorizer over the policy bundle.
func NewPolicyAuthorizer(p *Policy) *PolicyAuthorizer {
	return &PolicyAuthorizer{policy: p}
}

var _ Authorizer = (*PolicyAuthorizer)(nil)

// Authorize is deny-by-default: every gate below must pass to allow. The order
// is isolation-first — tenant and portfolio scope (ABAC) are checked before the
// RBAC grant, so a role can never widen access beyond the caller's tenant or
// portfolio allow-list.
func (a *PolicyAuthorizer) Authorize(_ context.Context, req Request) Decision {
	p := req.Principal
	if p == nil {
		return deny(DenyNoPrincipal, "no authenticated principal")
	}
	if req.Action == "" {
		return deny(DenyEmptyAction, "empty action")
	}
	// Tenant isolation (ABAC): the principal must carry a tenant, and a TYPED
	// resource must name the tenant it belongs to.
	if p.Tenant == "" {
		return deny(DenyPrincipalNoTenant, "principal has no tenant")
	}
	// AN EMPTY TENANT ON A TYPED RESOURCE IS UNKNOWN, NOT "TENANT-AGNOSTIC".
	//
	// This gate used to read `if req.Resource.Tenant != ""`, so an unpopulated
	// tenant skipped the boundary entirely — the reading being that a resource
	// with no tenant is not tenant-scoped (a health probe, a bare capability
	// check). That reading is right for a request naming NO resource, and wrong
	// for one naming a portfolio: "" is precisely the value a caller produces
	// when the ownership lookup FAILED, and the copilot produced it on every
	// error from OwnerTenant. A deadline or a 500 on that lookup therefore
	// removed the tenant boundary for that invocation while the rest of the gate
	// still passed, and the tool then read the data (#741).
	//
	// So the discriminator is Type, not Tenant. Resource{} still addresses
	// nothing and stays tenant-agnostic; a typed resource must say who owns it
	// or it is refused. That turns a caller's unresolved lookup into a refusal
	// instead of a bypass — the same rule as "a critical unknown fails closed",
	// applied to isolation.
	if req.Resource.Type != "" && req.Resource.Tenant == "" {
		return deny(DenyResourceTenantUnresolved, fmt.Sprintf(
			"%s %q carries no tenant: isolation cannot be established", req.Resource.Type, req.Resource.ID))
	}
	if req.Resource.Tenant != "" && req.Resource.Tenant != p.Tenant {
		return deny(DenyCrossTenant, fmt.Sprintf("cross-tenant denied: principal tenant %q != resource tenant %q", p.Tenant, req.Resource.Tenant))
	}
	// Portfolio scope (ABAC): if the principal is restricted to an explicit
	// portfolio allow-list, the target portfolio must be on it.
	//
	// AN EMPTY ALLOW-LIST PERMITS HERE AND DENIES ON THE CAPITAL PATH. That is a
	// decision, not a drift — PortfolioInScope and PortfolioEntitled sit beside
	// each other in portfolio.go with the argument for each, and this call site
	// must not be "unified" with the OMS's without reading it (#225).
	if req.Resource.Type == ResourcePortfolio && !PortfolioInScope(p.Portfolios, req.Resource.ID) {
		return deny(DenyPortfolioOutOfScope, fmt.Sprintf("portfolio %q not in principal scope", req.Resource.ID))
	}
	// RBAC: some role the principal holds must grant the action.
	for _, role := range p.Roles {
		for _, act := range a.policy.Roles[role] {
			if act == req.Action || act == "*" {
				return Decision{Allow: true, Reason: fmt.Sprintf("granted by role %q", role)}
			}
		}
	}
	return deny(DenyNoGrant, fmt.Sprintf("no role grants action %q", req.Action))
}

func deny(code DenyCode, reason string) Decision {
	return Decision{Allow: false, Reason: reason, Code: code}
}

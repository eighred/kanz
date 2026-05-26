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

// ClaimPortfolios is the principal claim carrying the portfolio-id allow-list
// for ABAC portfolio scoping. Absent/empty ⇒ the principal may reach every
// portfolio within its own tenant (the tenant boundary still applies).
const ClaimPortfolios = "portfolios"

// Resource is the thing being acted upon, carrying the attributes the decision
// keys on. Tenant-scoped resource types (e.g. portfolio) MUST populate Tenant
// so the cross-tenant guard can run; a tenant-agnostic resource leaves it empty.
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

// Decision is the authorization outcome. Reason is audit-facing — it is what
// AUTH-01d logs into the observation stream for every allow AND deny, so the
// "why" of a decision is always reconstructable.
type Decision struct {
	Allow  bool
	Reason string
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
		return deny("no authenticated principal")
	}
	if req.Action == "" {
		return deny("empty action")
	}
	// Tenant isolation (ABAC): the principal must carry a tenant, and a
	// tenant-scoped resource must belong to it. A resource with no tenant is
	// treated as not tenant-scoped (e.g. health) and skips the boundary.
	if p.Tenant == "" {
		return deny("principal has no tenant")
	}
	if req.Resource.Tenant != "" && req.Resource.Tenant != p.Tenant {
		return deny(fmt.Sprintf("cross-tenant denied: principal tenant %q != resource tenant %q", p.Tenant, req.Resource.Tenant))
	}
	// Portfolio scope (ABAC): if the principal is restricted to an explicit
	// portfolio allow-list, the target portfolio must be on it.
	if req.Resource.Type == ResourcePortfolio {
		if allowed := rolesClaim(p.Claims[ClaimPortfolios]); len(allowed) > 0 && !contains(allowed, req.Resource.ID) {
			return deny(fmt.Sprintf("portfolio %q not in principal scope", req.Resource.ID))
		}
	}
	// RBAC: some role the principal holds must grant the action.
	for _, role := range p.Roles {
		for _, act := range a.policy.Roles[role] {
			if act == req.Action || act == "*" {
				return Decision{Allow: true, Reason: fmt.Sprintf("granted by role %q", role)}
			}
		}
	}
	return deny(fmt.Sprintf("no role grants action %q", req.Action))
}

func deny(reason string) Decision { return Decision{Allow: false, Reason: reason} }

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

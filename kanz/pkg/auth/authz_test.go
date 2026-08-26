package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// AUTH-01e: the authorization contract suite. It pins the deny-by-default
// decision model exhaustively (role×action×scope matrix), guards the SHIPPED
// policy bundle against drift, and proves the layers compose — the audited
// authorizer never alters a verdict, and the forged-issuer guard holds. All
// pure, runs on every `go test`.

func testPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := LoadPolicy(strings.NewReader(`{
		"roles": {
			"risk.reader":  ["risk.read"],
			"risk.analyst": ["risk.read", "risk.scenario"],
			"risk.admin":   ["*"]
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// matrixCase is one row of the authorization matrix. The principal is always in
// tenant "acme"; resTenant/resID model the resource being acted on.
type matrixCase struct {
	name       string
	roles      []string
	portfolios []string // principal's portfolio-scope claim; nil ⇒ unrestricted
	action     Action
	resTenant  string // the resource's owning tenant; "" ⇒ unresolved, and refused
	resID      string
	allow      bool
}

func (tc matrixCase) request() Request {
	// The scope is the TYPED field. It used to be planted as an untyped
	// Claims["portfolios"] here, mirroring an authorizer that re-decoded the map
	// on every call — the second representation of one fact that let the
	// gateway's OIDC bridge drop it in silence (#225). A Claims entry now scopes
	// nothing, deliberately.
	p := &Principal{Subject: "akif", Tenant: "acme", Roles: tc.roles, Portfolios: tc.portfolios}
	return Request{Principal: p, Action: tc.action, Resource: Resource{Type: ResourcePortfolio, ID: tc.resID, Tenant: tc.resTenant}}
}

// A leftover Claims["portfolios"] must not scope anything, or the two
// representations are back and the map is the one nobody maintains.
func TestPolicyAuthorize_ScopeComesFromTheFieldNotTheClaimsBag(t *testing.T) {
	az := NewPolicyAuthorizer(testPolicy(t))
	p := &Principal{Subject: "akif", Tenant: "acme", Roles: []string{"risk.reader"},
		Claims: map[string]any{ClaimPortfolios: []any{"pf-1"}}} // no typed field
	d := az.Authorize(context.Background(), Request{
		Principal: p, Action: ActionRiskRead,
		Resource: Resource{Type: ResourcePortfolio, ID: "pf-9", Tenant: "acme"},
	})
	if !d.Allow {
		t.Fatalf("a Claims-only portfolio list scoped the decision (%s) — there must be exactly "+
			"one reader of this fact, and it is Principal.Portfolios", d.Reason)
	}
}

// authMatrix is the full role×action×scope contract against the shipped policy
// (reader→read, analyst→read+scenario, admin→*).
func authMatrix() []matrixCase {
	return []matrixCase{
		// RBAC role×action within the caller's own tenant, unrestricted scope.
		{"reader reads", []string{"risk.reader"}, nil, ActionRiskRead, "acme", "pf-1", true},
		{"reader cannot scenario", []string{"risk.reader"}, nil, ActionRiskScenario, "acme", "pf-1", false},
		{"reader unknown action", []string{"risk.reader"}, nil, "risk.delete", "acme", "pf-1", false},
		{"analyst reads", []string{"risk.analyst"}, nil, ActionRiskRead, "acme", "pf-1", true},
		{"analyst scenario", []string{"risk.analyst"}, nil, ActionRiskScenario, "acme", "pf-1", true},
		{"analyst unknown action", []string{"risk.analyst"}, nil, "risk.delete", "acme", "pf-1", false},
		{"admin reads", []string{"risk.admin"}, nil, ActionRiskRead, "acme", "pf-1", true},
		{"admin scenario", []string{"risk.admin"}, nil, ActionRiskScenario, "acme", "pf-1", true},
		{"admin wildcard covers unknown action", []string{"risk.admin"}, nil, "risk.delete", "acme", "pf-1", true},
		{"no role", nil, nil, ActionRiskRead, "acme", "pf-1", false},
		{"unknown role", []string{"nope"}, nil, ActionRiskRead, "acme", "pf-1", false},
		{"multi-role uses the granting one", []string{"nope", "risk.reader"}, nil, ActionRiskRead, "acme", "pf-1", true},

		// Tenant isolation (ABAC) beats RBAC — even the wildcard admin.
		{"admin cross-tenant denied", []string{"risk.admin"}, nil, ActionRiskRead, "globex", "pf-1", false},
		{"reader cross-tenant denied", []string{"risk.reader"}, nil, ActionRiskRead, "globex", "pf-1", false},

		// AN UNRESOLVED OWNER IS REFUSED, NOT WAVED THROUGH (#741). These rows
		// carry a typed resource with no tenant — the shape a caller produces
		// when its ownership lookup failed. It used to mean "not tenant-scoped,
		// skip the boundary", which made a dependency timeout a tenant bypass.
		// The wildcard admin row is the one that matters: if RBAC could rescue
		// this, the refusal would be a grant question rather than an isolation
		// one.
		{"admin with unresolved resource tenant denied", []string{"risk.admin"}, nil, ActionRiskRead, "", "pf-1", false},
		{"reader with unresolved resource tenant denied", []string{"risk.reader"}, nil, ActionRiskRead, "", "pf-1", false},

		// Portfolio scope (ABAC) beats RBAC — even the wildcard admin.
		{"scoped reader in scope", []string{"risk.reader"}, []string{"pf-1", "pf-2"}, ActionRiskRead, "acme", "pf-2", true},
		{"scoped reader out of scope", []string{"risk.reader"}, []string{"pf-1", "pf-2"}, ActionRiskRead, "acme", "pf-9", false},
		{"scoped admin out of scope denied", []string{"risk.admin"}, []string{"pf-1"}, ActionRiskRead, "acme", "pf-9", false},
		{"unrestricted reaches any portfolio", []string{"risk.reader"}, nil, ActionRiskRead, "acme", "pf-9", true},
	}
}

func TestPolicyAuthorize_Matrix(t *testing.T) {
	az := NewPolicyAuthorizer(testPolicy(t))
	for _, tc := range authMatrix() {
		t.Run(tc.name, func(t *testing.T) {
			if d := az.Authorize(context.Background(), tc.request()); d.Allow != tc.allow {
				t.Fatalf("allow=%v want %v (reason: %s)", d.Allow, tc.allow, d.Reason)
			}
		})
	}
}

// TestRealPolicyBundle guards the SHIPPED bundle against drift: it loads the
// actual infra/security/policies/risk-authz.json (the source of truth the
// ConfigMap wraps) and asserts the documented grants hold through the real
// authorizer — so editing the file to grant more/less breaks the build.
func TestRealPolicyBundle(t *testing.T) {
	p, err := LoadPolicyFile(filepath.Join("..", "..", "infra", "security", "policies", "risk-authz.json"))
	if err != nil {
		t.Fatalf("load shipped bundle: %v", err)
	}
	az := NewPolicyAuthorizer(p)
	check := func(role string, act Action, want bool) {
		t.Helper()
		d := az.Authorize(context.Background(), Request{
			Principal: &Principal{Subject: "u", Tenant: "acme", Roles: []string{role}},
			Action:    act,
			Resource:  Resource{Type: ResourcePortfolio, ID: "pf-1", Tenant: "acme"},
		})
		if d.Allow != want {
			t.Errorf("%s %s: allow=%v want %v", role, act, d.Allow, want)
		}
	}
	check("risk.reader", ActionRiskRead, true)
	check("risk.reader", ActionRiskScenario, false)
	check("risk.analyst", ActionRiskRead, true)
	check("risk.analyst", ActionRiskScenario, true)
	check("risk.admin", ActionRiskRead, true)
	check("risk.admin", "anything.else", true)
}

func TestPolicyAuthorize_DenyByDefault(t *testing.T) {
	az := NewPolicyAuthorizer(testPolicy(t))
	res := Resource{Type: ResourcePortfolio, ID: "pf-1", Tenant: "acme"}

	cases := []struct {
		name string
		req  Request
	}{
		{"nil principal", Request{Action: ActionRiskRead, Resource: res}},
		{"empty action even for admin", Request{Principal: &Principal{Tenant: "acme", Roles: []string{"risk.admin"}}, Resource: res}},
		{"principal without tenant", Request{Principal: &Principal{Subject: "u", Roles: []string{"risk.admin"}}, Action: ActionRiskRead, Resource: res}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if d := az.Authorize(context.Background(), tc.req); d.Allow {
				t.Fatalf("expected deny, got allow")
			}
		})
	}
}

func TestLoadPolicy_Errors(t *testing.T) {
	if _, err := LoadPolicy(strings.NewReader(`{"roles":{}}`)); err == nil {
		t.Fatal("empty roles must error")
	}
	if _, err := LoadPolicy(strings.NewReader(`{"unknown":1}`)); err == nil {
		t.Fatal("unknown field must error (DisallowUnknownFields)")
	}
	if _, err := LoadPolicy(strings.NewReader(`not json`)); err == nil {
		t.Fatal("malformed JSON must error")
	}
}

// TestAuthLayerComposes proves the audited authorizer is verdict-transparent
// across the whole matrix — wrapping for audit never changes a decision — and
// that each decision produces exactly one recorded DecisionLog whose recorded
// verdict matches. This is the authz↔audit (01b↔01d) contract.
func TestAuthLayerComposes(t *testing.T) {
	raw := NewPolicyAuthorizer(testPolicy(t))
	for _, tc := range authMatrix() {
		t.Run(tc.name, func(t *testing.T) {
			rec := &captureRecorder{}
			audited := NewAuditedAuthorizer(raw, rec, "authz:test", nil)
			req := tc.request()

			want := raw.Authorize(context.Background(), req)
			got := audited.Authorize(context.Background(), req)
			if got.Allow != want.Allow {
				t.Fatalf("audited verdict %v != raw %v", got.Allow, want.Allow)
			}
			if len(rec.entries) != 1 {
				t.Fatalf("recorded %d entries, want 1", len(rec.entries))
			}
			verdict := "deny"
			if want.Allow {
				verdict = "allow"
			}
			if rec.entries[0].GetAttributes()["decision"] != verdict {
				t.Fatalf("recorded decision=%q want %q", rec.entries[0].GetAttributes()["decision"], verdict)
			}
		})
	}
}

// TestForgedIssuerRejection pins the AUTH-01c forged-issuer property as part of
// the auth contract: a principal may issue as itself or a delegated identity,
// and nothing else (unit detail lives in issuer_test.go).
func TestForgedIssuerRejection(t *testing.T) {
	self := &Principal{Subject: "akif", Tenant: "acme"}
	delegated := &Principal{Subject: "akif", Tenant: "acme", Claims: map[string]any{ClaimIssuers: []any{"strategy:momentum-v2"}}}

	if err := VerifyIssuer(self, "user:akif"); err != nil {
		t.Fatalf("self-issue should pass: %v", err)
	}
	if err := VerifyIssuer(self, "user:eve"); !errors.Is(err, ErrForgedIssuer) {
		t.Fatalf("forged issuer: got %v want ErrForgedIssuer", err)
	}
	if err := VerifyIssuer(self, "strategy:momentum-v2"); !errors.Is(err, ErrForgedIssuer) {
		t.Fatalf("ungranted delegation: got %v want ErrForgedIssuer", err)
	}
	if err := VerifyIssuer(delegated, "strategy:momentum-v2"); err != nil {
		t.Fatalf("granted delegation should pass: %v", err)
	}
}

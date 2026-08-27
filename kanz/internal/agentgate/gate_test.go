package agentgate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/auth"
)

// THE GATE STANDS ALONE, AND THIS FILE IS THE PROOF (#743).
//
// #743 names extracting this decision into shared code as the step before an MCP
// server exists, because an MCP server needs exactly it and a second copy is how
// a fix stops spreading. It was kept inside services/copilot until services/mcp
// existed, so the promotion arrived WITH its second consumer rather than ahead
// of one — CLAUDE.md's rule, and the reason moving it was a file move.
//
// EVERY TEST BELOW CONSTRUCTS THE GATE DIRECTLY: no Registry, no governed
// client, no tool definitions, and nothing from either service. That is the
// claim under test. If the gate ever acquires a dependency on a caller's
// machinery, these tests stop compiling — which is the signal that it has become
// somebody's private decision again rather than the estate's.

// fakeOwner is an OwnerResolver over a map. It is the WHOLE collaborator the
// gate needs for ownership — the decision used to hold a governed.Client to call
// one method of it.
type fakeOwner struct {
	byID map[string]string
	err  error
}

func (f fakeOwner) OwnerTenant(_ context.Context, resourceID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	t, ok := f.byID[resourceID]
	if !ok {
		return "", ErrResourceNotVisible
	}
	return t, nil
}

// gateRecorder captures the audit records so a test can assert the attempt was
// recorded even when it was refused.
type gateRecorder struct{ logs []*observationpb.DecisionLog }

func (g *gateRecorder) Record(_ context.Context, e *observationpb.DecisionLog) error {
	g.logs = append(g.logs, e)
	return nil
}

func newGate(t *testing.T, owner OwnerResolver) (*Gate, *gateRecorder) {
	t.Helper()
	rec := &gateRecorder{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	authz := auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(gateTestPolicy()), rec, "agent-tools", quiet)
	return NewGate(authz, owner, quiet), rec
}

func t1(subject string) *auth.Principal {
	return &auth.Principal{Subject: subject, Tenant: "t1", Roles: []string{"analyst"}}
}

// The entitled caller passes. Without this every refusal test below is satisfied
// by a gate that refuses everything.
func TestGate_EntitledCallerIsAllowed(t *testing.T) {
	g, _ := newGate(t, fakeOwner{byID: map[string]string{"PF-1": "t1"}})

	v := g.Authorize(context.Background(), t1("alice"), auth.ActionRiskRead, auth.ResourcePortfolio, "PF-1")
	if !v.Allowed {
		t.Fatalf("entitled caller refused: %q", v.Message)
	}
}

// A cross-tenant probe and a resource that does not exist must be answered in
// IDENTICAL words. A caller able to tell them apart can enumerate another
// tenant's resources by id.
func TestGate_CrossTenantAndUnknownAreIndistinguishable(t *testing.T) {
	g, _ := newGate(t, fakeOwner{byID: map[string]string{"PF-1": "t1"}})
	intruder := &auth.Principal{Subject: "mallory", Tenant: "t2", Roles: []string{"analyst"}}

	existing := g.Authorize(context.Background(), intruder, auth.ActionRiskRead, auth.ResourcePortfolio, "PF-1")
	missing := g.Authorize(context.Background(), intruder, auth.ActionRiskRead, auth.ResourcePortfolio, "PF-NOPE")

	if existing.Allowed || missing.Allowed {
		t.Fatalf("a cross-tenant or unknown resource was allowed: %+v %+v", existing, missing)
	}
	if strings.TrimSuffix(existing.Message, "PF-1") != strings.TrimSuffix(missing.Message, "PF-NOPE") {
		t.Fatalf("an existing resource answers %q and a missing one answers %q — the difference is a "+
			"cross-tenant existence oracle", existing.Message, missing.Message)
	}
	for _, leak := range []string{"t1", "tenant", "cross"} {
		if strings.Contains(existing.Message, leak) {
			t.Errorf("refusal %q carries %q — the authorizer's reason must not reach the caller", existing.Message, leak)
		}
	}
}

// A resolver that cannot answer FAILS CLOSED, and says so honestly rather than
// reporting an outage as an empty resource.
func TestGate_AnUnresolvableOwnerRefuses(t *testing.T) {
	g, rec := newGate(t, fakeOwner{err: errors.New("deadline exceeded")})

	v := g.Authorize(context.Background(), t1("alice"), auth.ActionRiskRead, auth.ResourcePortfolio, "PF-1")
	if v.Allowed {
		t.Fatal("an unresolvable owner was allowed — availability decided isolation")
	}
	if v.Message != OwnerUnavailable(auth.ResourcePortfolio) {
		t.Errorf("message = %q, want the unavailable wording", v.Message)
	}
	// The attempt is still audited: a probe refused before the authorizer runs
	// is a probe nobody can see afterwards.
	if len(rec.logs) == 0 {
		t.Fatal("the refused attempt was not recorded")
	}
	if got := rec.logs[len(rec.logs)-1].GetAttributes()["deny.code"]; got != string(auth.DenyResourceTenantUnresolved) {
		t.Errorf("deny.code = %q, want %q", got, auth.DenyResourceTenantUnresolved)
	}
}

// A refusal about the CALLER's own token is stated plainly. Flattening those
// into "no such resource" would tell an analyst their own portfolio had vanished.
func TestGate_AGrantRefusalIsStatedNotFlattened(t *testing.T) {
	g, _ := newGate(t, fakeOwner{byID: map[string]string{"PF-1": "t1"}})
	noRole := &auth.Principal{Subject: "carol", Tenant: "t1", Roles: []string{"nobody"}}

	v := g.Authorize(context.Background(), noRole, auth.ActionRiskRead, auth.ResourcePortfolio, "PF-1")
	if v.Allowed || v.Message != NotAuthorized(auth.ResourcePortfolio) {
		t.Fatalf("message = %q, want %q", v.Message, NotAuthorized(auth.ResourcePortfolio))
	}
}

// THE GATE IS NOT PORTFOLIO-SHAPED, and that is the property that makes #743's
// eventual extraction a file move rather than a redesign. The resource type is a
// parameter, so a second tool surface reasoning over a different resource gets
// the same decision without a second copy of it.
//
// Asserted rather than assumed: the decision used to hardcode
// auth.ResourcePortfolio, which is invisible until somebody tries to reuse it.
func TestGate_ServesAResourceTypeThatIsNotAPortfolio(t *testing.T) {
	g, rec := newGate(t, fakeOwner{byID: map[string]string{"DS-1": "t2"}})

	// Same decision, a different resource type, and a caller from another tenant:
	// the isolation boundary must hold on a resource this package never names.
	v := g.Authorize(context.Background(), t1("alice"), auth.ActionRiskRead, "dataset", "DS-1")
	if v.Allowed {
		t.Fatal("a tenant-t1 caller reached a tenant-t2 dataset — the boundary is portfolio-shaped")
	}
	last := rec.logs[len(rec.logs)-1]
	if got := last.GetAttributes()["resource.type"]; got != "dataset" {
		t.Errorf("audited resource.type = %q, want dataset — the type reaches the record, not just the check", got)
	}
	if got := last.GetAttributes()["deny.code"]; got != string(auth.DenyCrossTenant) {
		t.Errorf("deny.code = %q, want %q", got, auth.DenyCrossTenant)
	}
}

// gateTestPolicy grants the analyst role the read actions. It is this package's
// own fixture: the gate borrows nothing from the services that call it, and a
// test reaching into one of them would quietly undo that.
func gateTestPolicy() *auth.Policy {
	return &auth.Policy{Roles: map[string][]auth.Action{
		"analyst": {auth.ActionRiskRead, auth.ActionRiskScenario},
	}}
}

// The mapping itself, over the whole deny-code set. Only the isolation classes
// collapse into the not-found answer; a grant refusal that started reading as
// "no such resource" would tell an analyst their own portfolio had vanished.
//
// It moved here with the gate: it is a property of the decision, not of any
// caller, and leaving it behind would have made it a test copilot happened to own.
func TestRefusalFor_OnlyIsolationCollapsesIntoNotFound(t *testing.T) {
	const rt = auth.ResourcePortfolio
	for _, code := range []auth.DenyCode{auth.DenyCrossTenant, auth.DenyResourceTenantUnresolved} {
		if got := refusalFor(code, rt, "PF-1"); got != NotVisible(rt, "PF-1") {
			t.Errorf("refusalFor(%q) = %q, want the not-found wording", code, got)
		}
	}
	for _, code := range []auth.DenyCode{auth.DenyNoGrant, auth.DenyPortfolioOutOfScope,
		auth.DenyNoPrincipal, auth.DenyPrincipalNoTenant, auth.DenyEmptyAction} {
		if got := refusalFor(code, rt, "PF-1"); got != NotAuthorized(rt) {
			t.Errorf("refusalFor(%q) = %q, want %q", code, got, NotAuthorized(rt))
		}
	}
}

package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	observationpb "github.com/kanz-eng/kanz-schemas-go/observation/v1"
)

type captureRecorder struct {
	entries []*observationpb.DecisionLog
	err     error
}

func (c *captureRecorder) Record(_ context.Context, e *observationpb.DecisionLog) error {
	c.entries = append(c.entries, e)
	return c.err
}

// staticAuthorizer returns a fixed decision, for testing the audit decorator
// in isolation from the policy engine.
type staticAuthorizer struct{ d Decision }

func (s staticAuthorizer) Authorize(context.Context, Request) Decision { return s.d }

func TestBuildDecisionLog(t *testing.T) {
	req := Request{
		Principal: &Principal{Subject: "akif", Tenant: "acme"},
		Action:    ActionRiskScenario,
		Resource:  Resource{Type: ResourcePortfolio, ID: "pf-1", Tenant: "acme"},
	}
	d := Decision{Allow: false, Reason: "no role grants action \"risk.scenario\""}
	log := BuildDecisionLog("", req, d)

	if log.GetDecider() != DefaultAuthzDecider {
		t.Fatalf("decider=%q", log.GetDecider())
	}
	attrs := log.GetAttributes()
	if attrs["decision"] != "deny" || attrs["action"] != "risk.scenario" ||
		attrs["principal.subject"] != "akif" || attrs["principal.tenant"] != "acme" ||
		attrs["resource.type"] != "portfolio" || attrs["resource.id"] != "pf-1" ||
		attrs["resource.tenant"] != "acme" || attrs["reason"] == "" {
		t.Fatalf("attributes = %v", attrs)
	}
	if !strings.HasPrefix(log.GetSummary(), "DENY risk.scenario on portfolio:pf-1 for akif:") {
		t.Fatalf("summary = %q", log.GetSummary())
	}
}

func TestBuildDecisionLog_AnonymousAllow(t *testing.T) {
	log := BuildDecisionLog("authz:gw", Request{Action: ActionRiskRead}, Decision{Allow: true, Reason: "granted by role \"risk.reader\""})
	if log.GetDecider() != "authz:gw" {
		t.Fatalf("decider=%q", log.GetDecider())
	}
	if log.GetAttributes()["decision"] != "allow" {
		t.Fatalf("decision=%q", log.GetAttributes()["decision"])
	}
	if !strings.HasPrefix(log.GetSummary(), "ALLOW risk.read on (none) for anonymous:") {
		t.Fatalf("summary = %q", log.GetSummary())
	}
}

func TestAuditedAuthorizer_RecordsBoth(t *testing.T) {
	rec := &captureRecorder{}
	az := NewAuditedAuthorizer(staticAuthorizer{Decision{Allow: true, Reason: "ok"}}, rec, "authz:test", nil)
	req := Request{Principal: &Principal{Subject: "u", Tenant: "acme"}, Action: ActionRiskRead}

	if d := az.Authorize(context.Background(), req); !d.Allow {
		t.Fatal("decision should pass through unchanged")
	}
	// Flip to deny and authorize again.
	az.inner = staticAuthorizer{Decision{Allow: false, Reason: "nope"}}
	if d := az.Authorize(context.Background(), req); d.Allow {
		t.Fatal("decision should pass through unchanged (deny)")
	}
	if len(rec.entries) != 2 {
		t.Fatalf("recorded %d entries, want 2 (allow + deny)", len(rec.entries))
	}
	if rec.entries[0].GetAttributes()["decision"] != "allow" || rec.entries[1].GetAttributes()["decision"] != "deny" {
		t.Fatalf("decisions = %q,%q", rec.entries[0].GetAttributes()["decision"], rec.entries[1].GetAttributes()["decision"])
	}
}

func TestAuditedAuthorizer_RecorderErrorDoesNotFailDecision(t *testing.T) {
	rec := &captureRecorder{err: errors.New("sink down")}
	az := NewAuditedAuthorizer(staticAuthorizer{Decision{Allow: true, Reason: "ok"}}, rec, "", nil)
	if d := az.Authorize(context.Background(), Request{Principal: &Principal{Subject: "u", Tenant: "acme"}, Action: ActionRiskRead}); !d.Allow {
		t.Fatal("recorder failure must not change the decision")
	}
	if len(rec.entries) != 1 {
		t.Fatalf("recorded %d, want 1", len(rec.entries))
	}
}

func TestAuditedAuthorizer_NilRecorder(t *testing.T) {
	az := NewAuditedAuthorizer(staticAuthorizer{Decision{Allow: false, Reason: "x"}}, nil, "", nil)
	if d := az.Authorize(context.Background(), Request{Action: ActionRiskRead}); d.Allow {
		t.Fatal("nil recorder must not affect the decision")
	}
}

func TestSlogRecorder(t *testing.T) {
	// Smoke test: a nil-logger recorder defaults to slog.Default() and never errors.
	if err := NewSlogRecorder(nil).Record(context.Background(), BuildDecisionLog("", Request{Action: ActionRiskRead}, Decision{Allow: true})); err != nil {
		t.Fatal(err)
	}
}

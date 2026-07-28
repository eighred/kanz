package tools

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	observationpb "github.com/kanz-eng/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
)

// capRecorder captures the authz DecisionLogs so a test can assert a decision
// was logged to the observation stream (AUDIT-01).
type capRecorder struct{ logs []*observationpb.DecisionLog }

func (c *capRecorder) Record(_ context.Context, e *observationpb.DecisionLog) error {
	c.logs = append(c.logs, e)
	return nil
}

func testPolicy() *auth.Policy {
	return &auth.Policy{Roles: map[string][]auth.Action{
		"analyst": {auth.ActionRiskRead, auth.ActionRiskScenario},
	}}
}

// harness wires the audited authorizer + stub governed client used across tests.
func harness(t *testing.T) (*Registry, *capRecorder) {
	t.Helper()
	rec := &capRecorder{}
	authz := auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(testPolicy()), rec, "copilot", slog.New(slog.NewTextHandler(discard{}, nil)))
	client := governed.NewStubClient()
	client.Put(governed.Reading{
		PortfolioID:   "PF-T1",
		Tenant:        "t1",
		Kind:          "measures",
		Values:        map[string]float64{"VaR99": 1250000, "Delta": 0.42},
		SourceEventID: "evt-123",
		AsOf:          time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
	})
	return NewRegistry(authz, client, retrieval.IdentityCatalog{}), rec
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func t1Analyst() *auth.Principal {
	return &auth.Principal{Subject: "alice", Tenant: "t1", Roles: []string{"analyst"}}
}

func TestInvoke_AuthorizedReturnsDataAndCitation(t *testing.T) {
	reg, _ := harness(t)
	out := reg.Invoke(context.Background(), t1Analyst(), llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})
	if out.IsError {
		t.Fatalf("expected success, got error: %s", out.Content)
	}
	// Citation correctness: the citation traces to the exact source event + portfolio.
	if len(out.Citations) != 1 || out.Citations[0].SourceEventID != "evt-123" || out.Citations[0].PortfolioID != "PF-T1" {
		t.Fatalf("citation = %+v, want evt-123/PF-T1", out.Citations)
	}
	if !strings.Contains(out.Content, "VaR99 = 1.25e+06") && !strings.Contains(out.Content, "VaR99 = 1250000") {
		t.Errorf("content missing VaR99 value: %s", out.Content)
	}
	// Values feed the grounding check.
	if len(out.Values) != 2 {
		t.Errorf("expected 2 values, got %v", out.Values)
	}
}

func TestInvoke_CrossTenantDeniedAndLogged(t *testing.T) {
	reg, rec := harness(t)
	// A t2 principal probes a t1-owned portfolio.
	intruder := &auth.Principal{Subject: "mallory", Tenant: "t2", Roles: []string{"analyst"}}
	out := reg.Invoke(context.Background(), intruder, llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})

	if !out.IsError {
		t.Fatalf("cross-tenant probe must be denied, got: %s", out.Content)
	}
	if !strings.Contains(out.Content, "not authorized") {
		t.Errorf("expected authorization denial, got: %s", out.Content)
	}
	if len(out.Citations) != 0 || len(out.Values) != 0 {
		t.Errorf("denied probe must leak no data: %+v / %v", out.Citations, out.Values)
	}
	// The deny was logged to the observation stream.
	if len(rec.logs) == 0 {
		t.Fatal("expected the denial to be recorded")
	}
	last := rec.logs[len(rec.logs)-1]
	if last.GetAttributes()["decision"] != "deny" {
		t.Errorf("recorded decision = %v, want deny", last.GetAttributes()["decision"])
	}
}

func TestInvoke_PortfolioScopeDenied(t *testing.T) {
	reg, _ := harness(t)
	// Same tenant, but the principal is scoped to a different portfolio allow-list.
	scoped := &auth.Principal{Subject: "bob", Tenant: "t1", Roles: []string{"analyst"},
		Claims: map[string]any{auth.ClaimPortfolios: []any{"PF-OTHER"}}}
	out := reg.Invoke(context.Background(), scoped, llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})
	if !out.IsError || !strings.Contains(out.Content, "not authorized") {
		t.Fatalf("out-of-scope portfolio must be denied, got: %+v", out)
	}
}

func TestInvoke_UnknownToolAndPortfolio(t *testing.T) {
	reg, _ := harness(t)
	if out := reg.Invoke(context.Background(), t1Analyst(), llm.ToolCall{Name: "nope"}); !out.IsError {
		t.Error("unknown tool should error")
	}
	// Authorized principal, missing portfolio_id.
	if out := reg.Invoke(context.Background(), t1Analyst(), llm.ToolCall{Name: "get_exposure", Input: map[string]any{}}); !out.IsError {
		t.Error("missing portfolio_id should error")
	}
}

func TestDefs(t *testing.T) {
	reg, _ := harness(t)
	defs := reg.Defs()
	if len(defs) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(defs))
	}
	if defs[0].Name != "get_risk_measures" {
		t.Errorf("first tool = %q", defs[0].Name)
	}
}

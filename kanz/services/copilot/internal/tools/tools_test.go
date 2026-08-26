package tools

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

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
	reg, rec, _ := harnessWith(t, nil)
	return reg, rec
}

// harnessWith builds the same registry over a governed client whose OwnerTenant
// fails with ownerErr (nil ⇒ the plain stub). It also returns the client, so a
// test can prove the READ path was never reached rather than only that the
// content looked like a refusal.
func harnessWith(t *testing.T, ownerErr error) (*Registry, *capRecorder, *countingClient) {
	t.Helper()
	rec := &capRecorder{}
	quiet := slog.New(slog.NewTextHandler(discard{}, nil))
	authz := auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(testPolicy()), rec, "copilot", quiet)
	stub := governed.NewStubClient()
	stub.Put(governed.Reading{
		PortfolioID:   "PF-T1",
		Tenant:        "t1",
		Kind:          "measures",
		Values:        map[string]float64{"VaR99": 1250000, "Delta": 0.42},
		SourceEventID: "evt-123",
		AsOf:          time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
	})
	client := &countingClient{Client: stub, ownerErr: ownerErr}
	return NewRegistry(authz, client, retrieval.IdentityCatalog{}, quiet), rec, client
}

// countingClient injects an OwnerTenant failure and counts governed READS. The
// count is the load-bearing assertion for #741: a refusal that still read the
// data would be a leak wearing a refusal's clothes.
type countingClient struct {
	governed.Client
	ownerErr error
	reads    int
}

func (c *countingClient) OwnerTenant(ctx context.Context, portfolioID string) (string, error) {
	if c.ownerErr != nil {
		// The shape a timeout or a 500 takes: no tenant, and an error.
		return "", c.ownerErr
	}
	return c.Client.OwnerTenant(ctx, portfolioID)
}

func (c *countingClient) Measures(ctx context.Context, portfolioID string, names []string) (governed.Reading, error) {
	c.reads++
	return c.Client.Measures(ctx, portfolioID, names)
}

func (c *countingClient) Exposure(ctx context.Context, portfolioID string) (governed.Reading, error) {
	c.reads++
	return c.Client.Exposure(ctx, portfolioID)
}

func (c *countingClient) EvaluateScenario(ctx context.Context, portfolioID, scenario string) (governed.Reading, error) {
	c.reads++
	return c.Client.EvaluateScenario(ctx, portfolioID, scenario)
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
	// The probe is answered in the SAME words as a portfolio that does not
	// exist. It used to be answered with the authorizer's own sentence, which
	// names the owning tenant — so a t2 caller learned both that PF-T1 exists
	// and that t1 owns it, from inside the refusal (#741).
	if out.Content != msgNoGovernedData+"PF-T1" {
		t.Errorf("cross-tenant refusal = %q, want the not-found wording %q", out.Content, msgNoGovernedData+"PF-T1")
	}
	for _, leak := range []string{"t1", "cross-tenant", "tenant"} {
		if strings.Contains(out.Content, leak) {
			t.Errorf("refusal content %q carries %q — the authorizer's reason must not reach the model", out.Content, leak)
		}
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
	// The detail the MODEL must not see is exactly the detail the AUDIT must:
	// sanitizing the caller-facing string is only safe if the real reason lands
	// somewhere an investigator can read it.
	if code := last.GetAttributes()["deny.code"]; code != string(auth.DenyCrossTenant) {
		t.Errorf("recorded deny.code = %q, want %q", code, auth.DenyCrossTenant)
	}
	if reason := last.GetAttributes()["reason"]; !strings.Contains(reason, "t1") {
		t.Errorf("audit reason %q does not name the resource tenant — the investigation loses what the refusal was about", reason)
	}
}

func TestInvoke_PortfolioScopeDenied(t *testing.T) {
	reg, _ := harness(t)
	// Same tenant, but the principal is scoped to a different portfolio allow-list.
	// The scope is the TYPED field, not a Claims entry: #225 collapsed the two
	// shapes of this one fact into Principal.Portfolios, decoded once by the
	// authenticator. A Claims["portfolios"] here would now be ignored — which is
	// the point, and is why this test would fail loudly rather than silently
	// widen if someone re-introduced the map read.
	scoped := &auth.Principal{Subject: "bob", Tenant: "t1", Roles: []string{"analyst"},
		Portfolios: []string{"PF-OTHER"}}
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

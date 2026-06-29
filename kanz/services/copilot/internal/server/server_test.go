package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/services/copilot/internal/agent"
	"github.com/kanz-eng/kanz/services/copilot/internal/governed"
	"github.com/kanz-eng/kanz/services/copilot/internal/llm"
	"github.com/kanz-eng/kanz/services/copilot/internal/retrieval"
	"github.com/kanz-eng/kanz/services/copilot/internal/tools"
)

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func newServer(t *testing.T) *Server {
	t.Helper()
	authz := auth.NewAuditedAuthorizer(
		auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"analyst": {auth.ActionRiskRead}}}),
		auth.NewSlogRecorder(slog.New(slog.NewTextHandler(discard{}, nil))), "copilot",
		slog.New(slog.NewTextHandler(discard{}, nil)))
	client := governed.NewStubClient()
	client.Put(governed.Reading{
		PortfolioID: "PF-T1", Tenant: "t1", Kind: "measures",
		Values: map[string]float64{"VaR99": 1250000}, SourceEventID: "evt-1",
		AsOf: time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
	})
	model := llm.NewStubModel(
		llm.Response{ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}}}},
		llm.Response{Text: "VaR99 is 1250000.", StopReason: llm.StopEndTurn},
	)
	reg := tools.NewRegistry(authz, client, retrieval.IdentityCatalog{})
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, agent.New(model, reg))
}

func TestAsk_Unauthenticated401(t *testing.T) {
	s := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"what is my VaR?"}`))
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ask: want 401 got %d", rec.Code)
	}
}

func TestAsk_Authorized(t *testing.T) {
	s := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"what is PF-T1 VaR?"}`))
	ctx := auth.WithPrincipal(req.Context(), &auth.Principal{Subject: "alice", Tenant: "t1", Roles: []string{"analyst"}})
	s.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized ask: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if g, _ := out["grounded"].(bool); !g {
		t.Errorf("expected grounded answer, got %v", out)
	}
	cites, _ := out["citations"].([]any)
	if len(cites) != 1 {
		t.Errorf("expected 1 citation, got %v", out["citations"])
	}
}

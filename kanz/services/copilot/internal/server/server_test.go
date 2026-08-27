package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/agent"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
	"github.com/eighred/kanz/services/copilot/internal/tools"
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
		Measures: governed.Measured(map[string]float64{"VaR99": 1250000}), SourceEventID: "evt-1",
		AsOf: time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
	})
	model := llm.NewStubModel(
		llm.Response{ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}}}},
		llm.Response{Text: "VaR99 is 1250000.", StopReason: llm.StopEndTurn},
	)
	reg := tools.NewRegistry(authz, client, retrieval.IdentityCatalog{}, nil)
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

// TestAsk_Authorized drives the server the way the mesh does — with the identity
// headers the api-gateway injects, and nothing on the context.
//
// It used to call auth.WithPrincipal on the request context directly, which is a
// shape no request has ever had in this process: nothing in cmd/copilot populated
// the context, so /v1/ask answered 401 to every real caller while this test was
// green (#268). Going through SetPrincipalHeaders is what makes it a proof — the
// injector's own encoding has to survive the reader for the ask to be authorized.
func TestAsk_Authorized(t *testing.T) {
	s := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"what is PF-T1 VaR?"}`))
	auth.SetPrincipalHeaders(req.Header, "alice", "t1", []string{"analyst"})
	s.ServeHTTP(rec, req)
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

// The kubelet and Prometheus reach this service directly, not through the
// gateway, and carry no principal. If auth.RequirePrincipal refuses them the pod
// never becomes ready and the service is scraped as down — a 401 on /readyz is a
// deployment outage, not a security posture.
func TestProbesAndMetricsDoNotRequireAPrincipal(t *testing.T) {
	s := newServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s without a principal: want 200 got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

// A tenant with no subject is not a partially-identified caller, it is an
// unidentified one. Admitting it would build a Principal that PolicyAuthorizer
// denies everything to but that governance.CheckAccess would still serve every
// non-PII dataset — the divergence #268 is about.
func TestAsk_PartialIdentityHeadersRefused(t *testing.T) {
	for name, set := range map[string]func(h http.Header){
		"tenant only":  func(h http.Header) { auth.SetPrincipalHeaders(h, "", "t1", []string{"analyst"}) },
		"subject only": func(h http.Header) { auth.SetPrincipalHeaders(h, "alice", "", []string{"analyst"}) },
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"q"}`))
			set(req.Header)
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: want 401 got %d (%s)", name, rec.Code, rec.Body.String())
			}
		})
	}
}

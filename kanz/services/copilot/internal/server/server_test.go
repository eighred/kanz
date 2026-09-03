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

func newServerWithModel(t *testing.T, model llm.Model) *Server {
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
	reg := tools.NewRegistry(authz, client, retrieval.IdentityCatalog{}, nil)
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, agent.New(model, reg))
}

// newServer is the default fixture: one tool call, then a grounded answer.
func newServer(t *testing.T) *Server {
	t.Helper()
	return newServerWithModel(t, llm.NewStubModel(
		llm.Response{ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}}}},
		llm.Response{Text: "VaR99 is 1250000.", StopReason: llm.StopEndTurn},
	))
}

// ask drives one authorized /v1/ask and returns the decoded body.
func ask(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"what is PF-T1 VaR?"}`))
	auth.SetPrincipalHeaders(req.Header, "alice", "t1", []string{"analyst"})
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ask: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return out
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

// AN UNGROUNDED ANSWER NAMES THE FIGURE IT COULD NOT SUPPORT (#981).
//
// The grounding gate MARKS, it does not withhold: a numeric claim backed by no
// tool result still goes out as prose, so the flag is the entire protection. A
// flag that says only "something here is invented" leaves a caller two options,
// discard the whole answer or ignore the flag, and ignoring it is the cheaper
// habit. This asserts the response names WHICH token failed the check.
func TestAsk_UngroundedAnswerNamesTheInventedFigure(t *testing.T) {
	s := newServerWithModel(t, llm.NewStubModel(
		llm.Response{ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "get_risk_measures",
			Input: map[string]any{"portfolio_id": "PF-T1"}}}},
		// 1250000 is the cited VaR; 987654 was returned by no tool.
		llm.Response{Text: "VaR99 is 1250000, and tail risk is 987654.", StopReason: llm.StopEndTurn},
	))
	out := ask(t, s)

	if g, _ := out["grounded"].(bool); g {
		t.Fatalf("an answer asserting a figure no tool returned was reported grounded: %v", out)
	}
	got, ok := out["ungrounded"].([]any)
	if !ok {
		t.Fatalf("the response carries no `ungrounded` list, so `grounded: false` names nothing and a "+
			"client cannot show the operator which number was invented (#981): %v", out)
	}
	found := false
	for _, v := range got {
		if str, _ := v.(string); str == "987654" {
			found = true
		}
	}
	if !found {
		t.Errorf("ungrounded = %v, want it to name 987654 — the figure that appeared in the answer "+
			"and in no tool result", got)
	}
}

// A GROUNDED ANSWER STILL CARRIES THE FIELD, AS AN EMPTY ARRAY.
//
// Without this the test above passes on a surface that emits `ungrounded` only
// when it is non-empty, which forces every client to tell "none" from "absent" —
// the same distinction `citations` already refuses to make.
func TestAsk_AGroundedAnswerCarriesAnEmptyUngroundedList(t *testing.T) {
	out := ask(t, newServer(t))
	if g, _ := out["grounded"].(bool); !g {
		t.Fatalf("expected a grounded answer, got %v", out)
	}
	got, ok := out["ungrounded"].([]any)
	if !ok || len(got) != 0 {
		t.Errorf("ungrounded = %#v, want an empty array on a grounded answer", out["ungrounded"])
	}
}

// BUDGET EXHAUSTION REACHES THE CALLER AS ITS OWN FIELD (#971 via #981).
//
// This is the case the issue did not name and that the response shape made
// worst: an exhausted loop returns Grounded TRUE (vacuously — it asserted no
// numbers) and Refused FALSE, so a caller reading those two fields sees an
// ordinary trustworthy answer whose prose happens to say it failed. #971
// separated "the model declined" from "the model never converged" because they
// have different owners; carrying neither field here re-merged them one hop on.
func TestAsk_AnExhaustedBudgetIsNotReportedAsAnOrdinaryAnswer(t *testing.T) {
	// A model that only ever asks for another tool call never converges.
	calls := make([]llm.Response, 0, 12)
	for i := 0; i < 12; i++ {
		calls = append(calls, llm.Response{ToolCalls: []llm.ToolCall{{ID: "tc", Name: "get_risk_measures",
			Input: map[string]any{"portfolio_id": "PF-T1"}}}})
	}
	out := ask(t, newServerWithModel(t, llm.NewStubModel(calls...)))

	if be, _ := out["budget_exhausted"].(bool); !be {
		t.Fatalf("a loop that never converged did not report budget_exhausted, so the caller sees "+
			"grounded=%v refused=%v and nothing else — indistinguishable from a real answer (#981): %v",
			out["grounded"], out["refused"], out)
	}
	if r, _ := out["refused"].(bool); r {
		t.Errorf("budget exhaustion was reported as a refusal; #971 made these distinct facts and the "+
			"wire must keep them distinct: %v", out)
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

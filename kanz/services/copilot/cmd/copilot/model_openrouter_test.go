//go:build openrouter

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/copilot/internal/config"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

// EVERY TEST HERE IS FULLY MOCKED. No key, no network, no live API — CI must
// never depend on a third party answering, and a mapping test that needs an
// upstream to be up is not a mapping test.
//
// COPILOT_OPENROUTER_BASE_URL exists for exactly this: each test points the
// adapter at an httptest server and asserts BOTH directions — what went out on
// the wire, and what came back through the seam.

// orServer captures the request the adapter sent and replies with a fixture.
type orServer struct {
	*httptest.Server
	gotBody  []byte
	gotAuth  string
	gotPath  string
	response string
	status   int
}

func newORServer(t *testing.T, response string) *orServer {
	t.Helper()
	s := &orServer{response: response, status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.gotBody, _ = io.ReadAll(r.Body)
		s.gotAuth = r.Header.Get("Authorization")
		s.gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.response))
	}))
	t.Cleanup(s.Close)
	return s
}

func orModel(t *testing.T, srv *orServer) (llm.Model, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := newOpenRouterModel(config.Config{
		ModelID:           "anthropic/claude-opus-4-8",
		OpenRouterAPIKey:  "test-key",
		OpenRouterBaseURL: srv.URL,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), reg)
	if err != nil {
		t.Fatalf("newOpenRouterModel: %v", err)
	}
	return m, reg
}

// decode the captured request so assertions read against structure, not strings.
func (s *orServer) request(t *testing.T) orRequest {
	t.Helper()
	var req orRequest
	if err := json.Unmarshal(s.gotBody, &req); err != nil {
		t.Fatalf("the adapter sent unparseable JSON: %v\n%s", err, s.gotBody)
	}
	return req
}

func counterValue(t *testing.T, reg *prometheus.Registry, finish, native, mapped string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_copilot_openrouter_stop_reason_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if got["finish_reason"] == finish && got["native_finish_reason"] == native && got["mapped"] == mapped {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// --- request mapping --------------------------------------------------------

// A TOOL RESULT BECOMES ITS OWN MESSAGE. The seam carries results on a USER turn
// (Anthropic's shape); chat-completions has no such thing — each result is a
// separate message with role "tool" and a tool_call_id. Losing that expansion is
// how a multi-tool turn silently drops results.
func TestToolResultsExpandIntoSeparateToolMessages(t *testing.T) {
	srv := newORServer(t, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`)
	m, _ := orModel(t, srv)

	_, err := m.Complete(context.Background(), llm.Request{
		System: "you are a risk copilot",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Text: "what is my exposure?"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "exposure", Input: map[string]any{"portfolio": "PF1"}},
				{ID: "call_2", Name: "measures", Input: map[string]any{"portfolio": "PF1"}},
			}},
			{Role: llm.RoleUser, ToolResults: []llm.ToolResult{
				{ToolCallID: "call_1", Content: "12.5m"},
				{ToolCallID: "call_2", Content: "boom", IsError: true},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	req := srv.request(t)
	// system + user + assistant + TWO tool messages = 5
	if len(req.Messages) != 5 {
		t.Fatalf("sent %d messages, want 5 (system, user, assistant, and one per tool result): %+v",
			len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system — position is the only thing that marks it",
			req.Messages[0].Role)
	}
	if got := req.Messages[2].ToolCalls; len(got) != 2 {
		t.Fatalf("assistant carried %d tool calls, want 2", len(got))
	}
	// Arguments must be a JSON STRING on this API, not an object.
	if args := req.Messages[2].ToolCalls[0].Function.Arguments; !strings.Contains(args, `"portfolio"`) {
		t.Errorf("tool call arguments = %q, want the input serialised as a JSON string", args)
	}
	for i, want := range []string{"call_1", "call_2"} {
		msg := req.Messages[3+i]
		if msg.Role != "tool" {
			t.Errorf("message %d role = %q, want tool", 3+i, msg.Role)
		}
		if msg.ToolCallID != want {
			t.Errorf("message %d tool_call_id = %q, want %q — an unpaired result is dropped by the model",
				3+i, msg.ToolCallID, want)
		}
	}
	// The seam's IsError has no field on this API, so it must reach the model in
	// the content or it learns the failed call succeeded.
	if !strings.HasPrefix(req.Messages[4].Content, "ERROR:") {
		t.Errorf("failed tool result content = %q, want it marked as an error", req.Messages[4].Content)
	}
}

// THE WHOLE JSON SCHEMA SURVIVES. The Anthropic adapter passes only
// "properties" because its typed param carries the rest; copying that here would
// silently drop every constraint a governed tool declares.
func TestToolSchemasArePassedWholeNotJustProperties(t *testing.T) {
	srv := newORServer(t, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	m, _ := orModel(t, srv)

	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"portfolio": map[string]any{"type": "string"}},
		"required":   []any{"portfolio"},
	}
	if _, err := m.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}},
		Tools:    []llm.ToolDef{{Name: "exposure", Description: "portfolio exposure", InputSchema: schema}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	req := srv.request(t)
	if len(req.Tools) != 1 {
		t.Fatalf("sent %d tools, want 1", len(req.Tools))
	}
	params := req.Tools[0].Function.Parameters
	for _, key := range []string{"type", "properties", "required"} {
		if _, ok := params[key]; !ok {
			t.Errorf("tool parameters lost %q — a governed tool's constraints would not reach the model: %+v",
				key, params)
		}
	}
}

func TestTheRequestCarriesTheKeyAndHitsChatCompletions(t *testing.T) {
	srv := newORServer(t, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	m, _ := orModel(t, srv)
	if _, err := m.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if srv.gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want a bearer token", srv.gotAuth)
	}
	if srv.gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", srv.gotPath)
	}
}

// --- response mapping -------------------------------------------------------

func TestToolCallsComeBackThroughTheSeam(t *testing.T) {
	srv := newORServer(t, `{"choices":[{"message":{"content":"","tool_calls":[
		{"id":"call_9","type":"function","function":{"name":"exposure","arguments":"{\"portfolio\":\"PF1\"}"}}
	]},"finish_reason":"tool_calls"}]}`)
	m, _ := orModel(t, srv)

	res, err := m.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "exposure?"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.StopReason != llm.StopToolUse {
		t.Errorf("StopReason = %q, want tool_use — the agent would stop instead of running the tool",
			res.StopReason)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.ID != "call_9" || tc.Name != "exposure" {
		t.Errorf("tool call = %+v, want id call_9 / name exposure", tc)
	}
	// Arguments arrive as a JSON string and must become the seam's map.
	if got, _ := tc.Input["portfolio"].(string); got != "PF1" {
		t.Errorf("tool input = %+v, want portfolio=PF1 decoded from the arguments string", tc.Input)
	}
}

// A tool call the adapter cannot decode must ERROR, not run.
//
// The alternative is invoking a GOVERNED tool — one that reads a real portfolio
// — with invented or empty arguments.
func TestUnparseableToolArgumentsAreRefusedNotGuessed(t *testing.T) {
	srv := newORServer(t, `{"choices":[{"message":{"tool_calls":[
		{"id":"c1","type":"function","function":{"name":"exposure","arguments":"{not json"}}
	]},"finish_reason":"tool_calls"}]}`)
	m, _ := orModel(t, srv)

	if _, err := m.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "x"}},
	}); err == nil {
		t.Fatal("a tool call with unparseable arguments was accepted — a governed tool would run on " +
			"invented input")
	}
}

// --- StopReason mapping -----------------------------------------------------

// REFUSAL IS STRUCTURAL, FROM EITHER FIELD.
func TestRefusalIsDetectedFromBothStructuralSignals(t *testing.T) {
	cases := []struct {
		name, fixture  string
		wantStop       llm.StopReason
		finish, native string
	}{
		{
			name:     "provider content filter",
			fixture:  `{"choices":[{"message":{"content":"blocked"},"finish_reason":"content_filter"}]}`,
			wantStop: llm.StopRefusal, finish: "content_filter", native: "none",
		},
		{
			// A Claude routed via OpenRouter reports its own refusal here.
			name:     "upstream native refusal passthrough",
			fixture:  `{"choices":[{"message":{"content":"I can't help with that"},"finish_reason":"stop","native_finish_reason":"refusal"}]}`,
			wantStop: llm.StopRefusal, finish: "stop", native: "refusal",
		},
		{
			name:     "ordinary completion",
			fixture:  `{"choices":[{"message":{"content":"12.5m"},"finish_reason":"stop"}]}`,
			wantStop: llm.StopEndTurn, finish: "stop", native: "none",
		},
		{
			// Truncation is NOT a refusal. Mapping it to one would halt the agent
			// loop on a long answer.
			name:     "truncated by length",
			fixture:  `{"choices":[{"message":{"content":"..."},"finish_reason":"length"}]}`,
			wantStop: llm.StopEndTurn, finish: "length", native: "none",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newORServer(t, tc.fixture)
			m, reg := orModel(t, srv)

			res, err := m.Complete(context.Background(), llm.Request{
				Messages: []llm.Message{{Role: llm.RoleUser, Text: "q"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if res.StopReason != tc.wantStop {
				t.Errorf("StopReason = %q, want %q", res.StopReason, tc.wantStop)
			}
			// THE OBSERVABILITY IS THE REQUIREMENT, not a nicety: it is what makes
			// "this provider never reports refusals" checkable instead of assumed.
			if got := counterValue(t, reg, tc.finish, tc.native, string(tc.wantStop)); got != 1 {
				t.Errorf("stop_reason counter for (%s,%s,%s) = %v, want 1",
					tc.finish, tc.native, tc.wantStop, got)
			}
		})
	}
}

// MODEL PROSE IS NOT EVIDENCE OF REFUSAL, and this pins that decision.
//
// agent.go:92 halts the tool loop on StopRefusal and reports Answer{Refused}.
// So a phantom refusal truncates a real analysis and presents it as a decline —
// worse than the miss it would prevent, which shows the model's own words as
// ordinary text. A refusal-shaped SENTENCE with finish_reason "stop" must map to
// StopEndTurn.
func TestRefusalIsNotInferredFromTheAnswerText(t *testing.T) {
	srv := newORServer(t, `{"choices":[{"message":{"content":"I cannot help with that. I'm sorry, but I am unable to comply."},"finish_reason":"stop"}]}`)
	m, reg := orModel(t, srv)

	res, err := m.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "q"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.StopReason != llm.StopEndTurn {
		t.Errorf("StopReason = %q for refusal-shaped PROSE with finish_reason stop, want end_turn — "+
			"pattern-matching text would truncate real analyses that merely discuss refusals",
			res.StopReason)
	}
	// It is still counted, which is how the residual gap stays visible.
	if got := counterValue(t, reg, "stop", "none", string(llm.StopEndTurn)); got != 1 {
		t.Errorf("the completion was not counted (got %v) — the gap this leaves has to be measurable", got)
	}
}

// An unbounded native_finish_reason would let an upstream provider blow up the
// metric's cardinality.
func TestUnknownFinishReasonsAreBucketed(t *testing.T) {
	srv := newORServer(t, `{"choices":[{"message":{"content":"x"},"finish_reason":"stop","native_finish_reason":"MAX_TOKENS_7f3a"}]}`)
	m, reg := orModel(t, srv)
	if _, err := m.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "q"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := counterValue(t, reg, "stop", "other", string(llm.StopEndTurn)); got != 1 {
		t.Errorf("an unknown native reason was not bucketed as \"other\" (got %v) — one label per "+
			"upstream string is how a metric takes down a Prometheus", got)
	}
}

// --- failure surfaces -------------------------------------------------------

// A 200 carrying an error object is a real OpenRouter shape. Treating it as an
// empty answer surfaces as the copilot saying nothing, with nothing saying why.
func TestAnErrorObjectInA200IsSurfaced(t *testing.T) {
	srv := newORServer(t, `{"error":{"message":"upstream is down","code":502}}`)
	m, _ := orModel(t, srv)

	_, err := m.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "q"}},
	})
	if err == nil {
		t.Fatal("an error object inside a 200 was treated as a successful completion")
	}
	if !strings.Contains(err.Error(), "upstream is down") {
		t.Errorf("error = %q, want it to carry the upstream message", err)
	}
}

func TestANonOKStatusIsSurfacedWithItsBody(t *testing.T) {
	srv := newORServer(t, `{"error":{"message":"no credit"}}`)
	srv.status = http.StatusPaymentRequired
	m, _ := orModel(t, srv)

	_, err := m.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "q"}},
	})
	if err == nil {
		t.Fatal("a 402 was treated as success")
	}
	if !strings.Contains(err.Error(), "402") || !strings.Contains(err.Error(), "no credit") {
		t.Errorf("error = %q, want the status AND the body — one without the other is not diagnosable", err)
	}
}

// The adapter must refuse to BUILD without a key, so the service does not come
// up healthy and fail only when somebody asks a question.
func TestTheAdapterRefusesToBuildWithoutAKey(t *testing.T) {
	_, err := newOpenRouterModel(config.Config{ModelID: "x"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), prometheus.NewRegistry())
	if err == nil {
		t.Fatal("built an OpenRouter model with no API key — the service would pass readiness and " +
			"fail at the first question")
	}
	if !strings.Contains(err.Error(), "COPILOT_OPENROUTER_API_KEY_FILE") {
		t.Errorf("error = %q, want it to name the mount to add", err)
	}
}

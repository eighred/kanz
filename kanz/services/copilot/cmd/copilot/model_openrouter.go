//go:build openrouter

// The OpenRouter adapter (#179). Behind the `openrouter` build tag for the same
// reason the Anthropic one is behind `anthropic`: the default build links no LLM
// SDK, which keeps `go build/vet` and the whole test suite free of a vendor
// dependency. A production image builds both and the deployment chooses with
// COPILOT_PROVIDER.
//
// WHY OPENROUTER IS ONE ADAPTER AND NOT ONE PER VENDOR. It fronts many models —
// Claude, GPT, DeepSeek, Llama — behind a single OpenAI-compatible endpoint, so
// the vendor becomes COPILOT_MODEL_ID rather than a compile-time decision.
//
// NO SDK. The API is chat-completions JSON over HTTP and this adapter uses
// net/http directly. Pulling an OpenAI SDK to send one POST would add a second
// vendor dependency to the binary for no capability the seam can use — the same
// argument that keeps anthropic-sdk-go behind a tag, applied harder to a client
// we would use one endpoint of.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/copilot/internal/config"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

// ProviderOpenRouter is this adapter's COPILOT_PROVIDER value.
const ProviderOpenRouter = "openrouter"

func init() { registerProvider(ProviderOpenRouter, newOpenRouterModel) }

// openRouterTimeout bounds one completion. Generous because a tool-use turn over
// a large context is slow, and a copilot that gives up mid-analysis is worse
// than one that takes a while.
const openRouterTimeout = 5 * time.Minute

// openRouterModel adapts OpenRouter's OpenAI-compatible chat-completions API to
// the llm.Model seam.
type openRouterModel struct {
	http    *http.Client
	baseURL string
	apiKey  string
	modelID string
	logger  *slog.Logger

	// stops counts responses by the finish_reason that produced them.
	//
	// IT IS THE OBSERVABILITY THE DESIGN REQUIRED, doing one specific job:
	// making "this provider never reports refusals" a fact somebody can check
	// rather than an assumption nobody tested. See mapStopReason.
	stops *prometheus.CounterVec
}

func newOpenRouterModel(cfg config.Config, logger *slog.Logger, reg prometheus.Registerer) (llm.Model, error) {
	if cfg.OpenRouterAPIKey == "" {
		// Refused at build, not at the first question: a copilot that comes up
		// healthy and fails only when somebody asks something has passed its
		// readiness probe under false pretences.
		return nil, errors.New("COPILOT_PROVIDER=openrouter but no API key: mount " +
			"COPILOT_OPENROUTER_API_KEY_FILE (SEC-01d), or choose a provider this deployment has " +
			"credentials for")
	}
	base := strings.TrimRight(cfg.OpenRouterBaseURL, "/")
	if base == "" {
		base = config.DefaultOpenRouterBaseURL
	}

	stops := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_copilot_openrouter_stop_reason_total",
		Help: "Completions by the finish_reason OpenRouter returned and the llm.StopReason it mapped to.",
	}, []string{"finish_reason", "native_finish_reason", "mapped"})
	if reg != nil {
		if err := reg.Register(stops); err != nil {
			// Already-registered is not fatal — but it must not be silent, and the
			// existing collector must be adopted or the counts split in two.
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				return nil, fmt.Errorf("openrouter: register metrics: %w", err)
			}
			if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
				stops = existing
			}
		}
	}

	logger.Info("copilot llm: openrouter client", "model", cfg.ModelID, "base_url", base)
	return &openRouterModel{
		http:    &http.Client{Timeout: openRouterTimeout},
		baseURL: base,
		apiKey:  cfg.OpenRouterAPIKey,
		modelID: cfg.ModelID,
		logger:  logger,
		stops:   stops,
	}, nil
}

// --- wire types -------------------------------------------------------------
//
// Hand-written rather than imported: these are the only shapes the seam needs,
// and they are stable parts of an API many providers implement.

type orRequest struct {
	Model    string      `json:"model"`
	Messages []orMessage `json:"messages"`
	Tools    []orTool    `json:"tools,omitempty"`
}

type orMessage struct {
	Role      string       `json:"role"`
	Content   string       `json:"content,omitempty"`
	ToolCalls []orToolCall `json:"tool_calls,omitempty"`
	// ToolCallID pairs a "tool" message back to the call that asked for it.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type orToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments is a JSON STRING, not an object — an OpenAI-ism the seam does
		// not share (llm.ToolCall.Input is a map). Both directions convert it.
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type orTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

type orResponse struct {
	Choices []struct {
		Message      orMessage `json:"message"`
		FinishReason string    `json:"finish_reason"`
		// NativeFinishReason is OpenRouter's passthrough of the UPSTREAM
		// provider's own reason. It is why refusal detection can be structural
		// rather than guessed — see mapStopReason.
		NativeFinishReason string `json:"native_finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// --- the seam ---------------------------------------------------------------

// Complete runs one turn: map the seam Request onto chat-completions, POST it,
// and map the choice back.
func (m *openRouterModel) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	body, err := json.Marshal(orRequest{
		Model:    m.modelID,
		Messages: toORMessages(req),
		Tools:    toORTools(req.Tools),
	})
	if err != nil {
		return llm.Response{}, fmt.Errorf("openrouter: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+m.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := m.http.Do(httpReq)
	if err != nil {
		return llm.Response{}, fmt.Errorf("openrouter: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return llm.Response{}, fmt.Errorf("openrouter: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return llm.Response{}, fmt.Errorf("openrouter: status %d: %s", resp.StatusCode, truncateBody(string(raw), 300))
	}

	var out orResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return llm.Response{}, fmt.Errorf("openrouter: decode response: %w", err)
	}
	// A 200 carrying an error object is a real OpenRouter shape — an upstream
	// failure it forwarded. Treating it as an empty answer would surface as the
	// copilot saying nothing, with nothing anywhere saying why.
	if out.Error != nil {
		return llm.Response{}, fmt.Errorf("openrouter: upstream error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return llm.Response{}, errors.New("openrouter: response carried no choices")
	}
	choice := out.Choices[0]

	res := llm.Response{Text: choice.Message.Content}
	for _, tc := range choice.Message.ToolCalls {
		var input map[string]any
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				// A tool call whose arguments will not parse cannot be run, and
				// guessing would run a GOVERNED tool on invented input.
				return llm.Response{}, fmt.Errorf("openrouter: tool call %q has unparseable arguments: %w",
					tc.Function.Name, err)
			}
		}
		res.ToolCalls = append(res.ToolCalls, llm.ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: input})
	}
	res.StopReason = m.mapStopReason(choice.FinishReason, choice.NativeFinishReason)
	return res, nil
}

// mapStopReason translates a finish_reason into the seam's StopReason.
//
// REFUSAL IS DETECTED STRUCTURALLY, AND ONLY STRUCTURALLY. Two fields carry it
// honestly:
//
//	finish_reason "content_filter"    the provider's own filter stopped it
//	native_finish_reason "refusal"    the UPSTREAM model's own reason, passed
//	                                  through — a Claude routed via OpenRouter
//	                                  reports its refusal here verbatim
//
// WHAT THIS DELIBERATELY DOES NOT DO IS PATTERN-MATCH THE TEXT, and the reason
// is agent.go:92. On StopRefusal the agent HALTS THE TOOL LOOP and returns
// Answer{Refused: true}, which the terminal renders as "the copilot refused this
// request". A false positive therefore does not mislabel one line of output — it
// truncates a real analysis part-way and presents it as a refusal.
//
// That asymmetry decides it. A MISSED refusal shows the model's own "I can't
// help with that" as ordinary text: visible, mild, self-explaining. A PHANTOM
// refusal silently drops the tool calls the answer needed. Matching model prose
// for apology phrases generates the second kind, in the language of whatever
// model COPILOT_MODEL_ID happens to name.
//
// The residual gap is real and is not argued away: a model that declines in
// CONTENT while reporting finish_reason "stop" is structurally indistinguishable
// from one that answered. So it is MEASURED. Every completion increments
// kanz_copilot_openrouter_stop_reason_total by (finish_reason,
// native_finish_reason, mapped), which makes "this provider never reports
// refusals" a number to look at rather than an assumption nobody tested. If that
// number says the gap matters for a model in use, closing it becomes a decision
// with evidence behind it instead of a heuristic added on a hunch.
func (m *openRouterModel) mapStopReason(finish, native string) llm.StopReason {
	mapped := llm.StopEndTurn
	switch {
	case strings.EqualFold(finish, "tool_calls"):
		mapped = llm.StopToolUse
	case strings.EqualFold(finish, "content_filter"), strings.EqualFold(native, "refusal"):
		mapped = llm.StopRefusal
	}

	if m.stops != nil {
		m.stops.WithLabelValues(orLabel(finish), orLabel(native), string(mapped)).Inc()
	}
	if mapped == llm.StopRefusal {
		// Logged as well as counted: a refusal is the one outcome an operator may
		// have to explain to somebody, and a counter increment cannot be traced
		// back to a request.
		m.logger.Warn("openrouter: model declined the request",
			"finish_reason", finish, "native_finish_reason", native, "model", m.modelID)
	}
	return mapped
}

// orLabel keeps the metric's cardinality bounded. finish_reason is a small
// closed set, but native_finish_reason is whatever an upstream provider sends,
// and an unbounded label is how a metric takes down a Prometheus.
//
// "OTHER" MUST MEAN UNANTICIPATED, OR IT MEANS NOTHING (#179 verification).
// Driving the built binary against a stand-in upstream showed every ordinary
// turn landing in that bucket: OpenRouter passes an Anthropic model's OWN reason
// through native_finish_reason, and those are end_turn / tool_use / max_tokens —
// none of which were listed, so all three counted as "other".
//
//	kanz_copilot_openrouter_stop_reason_total{finish_reason="tool_calls",
//	  native_finish_reason="other",mapped="tool_use"} 1     <- was "tool_use"
//
// That is the bucket whose entire job is to make a reason nobody expected
// visible, saturated by the two most routine values on the most likely model
// family. A genuine surprise arriving there would have been indistinguishable
// from normal traffic — which defeats the measurement this metric exists to
// provide (see mapStopReason: the refusal gap is closed by evidence, not by a
// heuristic, and evidence read from a saturated bucket is not evidence).
//
// So the closed set carries BOTH vocabularies: OpenAI's finish_reason values and
// the upstream-native stop reasons that reach us verbatim. Case is folded first,
// which also covers the providers that shout theirs (Gemini's STOP/MAX_TOKENS).
func orLabel(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return "none"
	// OpenAI-shaped finish_reason — what OpenRouter normalises to.
	case "stop":
		return "stop"
	case "tool_calls":
		return "tool_calls"
	case "length":
		return "length"
	case "content_filter":
		return "content_filter"
	case "error":
		return "error"
	// Upstream-native stop reasons, passed through unchanged. "refusal" is in
	// both vocabularies and is the one mapStopReason branches on.
	case "refusal":
		return "refusal"
	case "end_turn":
		return "end_turn"
	case "tool_use":
		return "tool_use"
	case "max_tokens":
		return "max_tokens"
	case "stop_sequence":
		return "stop_sequence"
	case "pause_turn":
		return "pause_turn"
	default:
		return "other"
	}
}

// --- request mapping --------------------------------------------------------

// toORMessages maps the seam's turns onto chat-completions messages.
//
// THE SHAPES DO NOT LINE UP ONE-TO-ONE, which is the substance of this adapter.
// The seam carries tool RESULTS on a user turn (Anthropic's shape: a user
// message containing tool_result blocks). Chat-completions has no such thing —
// each result is its OWN message, role "tool", carrying a tool_call_id. So one
// seam message becomes several, and losing that expansion is how a multi-tool
// turn silently drops results.
func toORMessages(req llm.Request) []orMessage {
	out := make([]orMessage, 0, len(req.Messages)+1)
	// The system prompt is a MESSAGE here, not a top-level field as at Anthropic.
	// It leads, because position is the only thing that marks it.
	if req.System != "" {
		out = append(out, orMessage{Role: "system", Content: req.System})
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case llm.RoleAssistant:
			m := orMessage{Role: "assistant", Content: msg.Text}
			for _, tc := range msg.ToolCalls {
				var call orToolCall
				call.ID = tc.ID
				call.Type = "function"
				call.Function.Name = tc.Name
				// Arguments is a JSON string on this API.
				if b, err := json.Marshal(tc.Input); err == nil {
					call.Function.Arguments = string(b)
				} else {
					call.Function.Arguments = "{}"
				}
				m.ToolCalls = append(m.ToolCalls, call)
			}
			out = append(out, m)

		default: // RoleUser
			if len(msg.ToolResults) > 0 {
				for _, tr := range msg.ToolResults {
					content := tr.Content
					if tr.IsError {
						// The seam has an IsError flag; this API does not. Marking it
						// in the content is the only way the model learns the tool
						// failed — passing the error text as an ordinary result
						// teaches it the call succeeded.
						content = "ERROR: " + content
					}
					out = append(out, orMessage{Role: "tool", ToolCallID: tr.ToolCallID, Content: content})
				}
				continue
			}
			out = append(out, orMessage{Role: "user", Content: msg.Text})
		}
	}
	return out
}

// toORTools maps the seam's tool definitions onto function tools.
func toORTools(defs []llm.ToolDef) []orTool {
	if len(defs) == 0 {
		return nil
	}
	out := make([]orTool, 0, len(defs))
	for _, d := range defs {
		var t orTool
		t.Type = "function"
		t.Function.Name = d.Name
		t.Function.Description = d.Description
		// InputSchema is already a JSON Schema object, which is what `parameters`
		// wants — passed WHOLE rather than picking "properties" out of it, so
		// required/type/$defs survive. The Anthropic adapter takes only properties
		// because its InputSchemaParam has typed fields for the rest; copying that
		// here would silently drop every constraint a governed tool declares.
		t.Function.Parameters = d.InputSchema
		out = append(out, t)
	}
	return out
}

func truncateBody(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

//go:build anthropic

// The production Claude adapter (PARITY-04a). Behind the `anthropic` build tag
// so only the production image (`go build -tags anthropic`) pulls
// anthropic-sdk-go — the default module build stays LLM-SDK-free (see
// model_stub.go). It implements the version-agnostic llm.Model seam against
// whatever model COPILOT_MODEL_ID names (default claude-fable-5): one
// Complete == one Claude turn, so the agent (internal/agent) drives the manual
// tool-use loop over this adapter exactly as it drives the StubModel.
//
// Per-version quirks are the adapter's concern, not the agent's: adaptive
// thinking is always on, and for the Fable/Mythos family — whose safety
// classifiers can return stop_reason "refusal" — the request carries the
// server-side `fallbacks` parameter so a decline is transparently re-served by
// claude-opus-4-8 inside the same call. Streaming is used unconditionally so
// large (up to 128K) outputs cannot hit the SDK's non-streaming HTTP timeout.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/copilot/internal/config"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

const (
	// anthropicMaxTokens leaves room for large grounded answers; streaming keeps
	// the request under the SDK's non-streaming timeout regardless.
	anthropicMaxTokens = 16000
	// refusalFallbackModel re-serves a Fable/Mythos refusal in the same call.
	refusalFallbackModel = "claude-opus-4-8"
)

// anthropicModel adapts the anthropic-sdk-go client to the llm.Model seam.
type anthropicModel struct {
	client   anthropic.Client
	modelID  string
	fallback bool // Fable/Mythos family: attach the server-side refusal fallback
}

// ProviderAnthropic is this adapter's COPILOT_PROVIDER value.
const ProviderAnthropic = "anthropic"

func init() { registerProvider(ProviderAnthropic, newAnthropicModel) }

// newAnthropicModel builds the real Claude client. anthropic.NewClient()
// resolves credentials the standard way (ANTHROPIC_API_KEY / OAuth profile /
// WIF).
//
// It no longer defines newModel: with a second provider linked, two files
// defining that function would not compile, and which one won would have been
// decided by build tags rather than by the deployment (#179).
func newAnthropicModel(cfg config.Config, logger *slog.Logger, _ prometheus.Registerer) (llm.Model, error) {
	fb := isRefusalFallbackModel(cfg.ModelID)
	logger.Info("copilot llm: anthropic client", "model", cfg.ModelID, "refusal_fallback", fb)
	// SEC-01d: the key arrives as a Vault/CSI file, not a plaintext env var. Falling
	// through to anthropic.NewClient() (env / OAuth profile / WIF) keeps local dev
	// working without a mount.
	var opts []option.RequestOption
	if cfg.AnthropicAPIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.AnthropicAPIKey))
	}
	return &anthropicModel{
		client:   anthropic.NewClient(opts...),
		modelID:  cfg.ModelID,
		fallback: fb,
	}, nil
}

// isRefusalFallbackModel reports whether the configured model needs the
// server-side refusal fallback (the Fable 5 / Mythos 5 family, whose safety
// classifiers can decline a request with stop_reason "refusal").
func isRefusalFallbackModel(modelID string) bool {
	return strings.HasPrefix(modelID, "claude-fable") || strings.HasPrefix(modelID, "claude-mythos")
}

// Complete runs one Claude turn: it maps the seam Request onto Anthropic message
// params, streams the response to completion, and maps the result back.
func (m *anthropicModel) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(m.modelID),
		MaxTokens: anthropicMaxTokens,
		Thinking:  anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{}},
		Messages:  toBetaMessages(req.Messages),
		Tools:     toBetaTools(req.Tools),
	}
	if req.System != "" {
		params.System = []anthropic.BetaTextBlockParam{{Text: req.System}}
	}
	if m.fallback {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_06_01}
		// Fallbacks became a UNION in anthropic-sdk-go v1.61.0 — it was a plain
		// []BetaFallbackParam through v1.60.0. The union's other variant is
		// OfDefault (the SDK's own default chain), and only one field may be
		// non-zero, so naming OfBetaFallbackArray explicitly is what keeps this
		// pinned to OUR fallback model rather than whatever the SDK would pick.
		// That distinction is the reason this field is set at all.
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{
			OfBetaFallbackArray: []anthropic.BetaFallbackParam{{Model: refusalFallbackModel}},
		}
	}

	stream := m.client.Beta.Messages.NewStreaming(ctx, params)
	acc := anthropic.BetaMessage{}
	for stream.Next() {
		// AN EVENT THAT WILL NOT ACCUMULATE LEAVES acc PARTIAL. Discarding this
		// error serves whatever assembled — a truncated answer, or a tool_use
		// block that never completed — to a portfolio manager AS ANALYSIS, with
		// nothing anywhere saying it was cut short. stream.Err() below does not
		// cover it: that reports a transport failure, not a message the SDK could
		// not put back together.
		//
		// Nothing had ever flagged it because CI's golangci-lint ran with no
		// build tags, so neither adapter behind one was linted at all (#179
		// verification; the lint step now passes the shipped tags).
		if err := acc.Accumulate(stream.Current()); err != nil {
			return llm.Response{}, fmt.Errorf("anthropic: incomplete streamed message: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return llm.Response{}, err
	}
	return fromBetaMessage(acc), nil
}

// toBetaMessages reconstructs the Anthropic message history from the seam's
// turns. Assistant turns carry their tool_use blocks (the load-bearing part for
// tool_result pairing); the model's own thinking blocks are not retained by the
// seam and so are not replayed (a tolerable multi-turn nuance — see the
// carried-forward note).
func toBetaMessages(msgs []llm.Message) []anthropic.BetaMessageParam {
	out := make([]anthropic.BetaMessageParam, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.Role {
		case llm.RoleAssistant:
			var blocks []anthropic.BetaContentBlockParamUnion
			if msg.Text != "" {
				blocks = append(blocks, anthropic.BetaContentBlockParamUnion{
					OfText: &anthropic.BetaTextBlockParam{Text: msg.Text},
				})
			}
			for _, tc := range msg.ToolCalls {
				blocks = append(blocks, anthropic.BetaContentBlockParamUnion{
					OfToolUse: &anthropic.BetaToolUseBlockParam{
						ID:    tc.ID,
						Name:  tc.Name,
						Input: tc.Input,
					},
				})
			}
			out = append(out, anthropic.BetaMessageParam{
				Role:    anthropic.BetaMessageParamRoleAssistant,
				Content: blocks,
			})
		default: // RoleUser
			if len(msg.ToolResults) > 0 {
				blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(msg.ToolResults))
				for _, tr := range msg.ToolResults {
					blocks = append(blocks, anthropic.NewBetaToolResultBlock(tr.ToolCallID, tr.Content, tr.IsError))
				}
				out = append(out, anthropic.NewBetaUserMessage(blocks...))
			} else {
				out = append(out, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(msg.Text)))
			}
		}
	}
	return out
}

// toBetaTools maps the seam's tool definitions onto Claude tool-use functions.
func toBetaTools(defs []llm.ToolDef) []anthropic.BetaToolUnionParam {
	if len(defs) == 0 {
		return nil
	}
	out := make([]anthropic.BetaToolUnionParam, 0, len(defs))
	for _, d := range defs {
		tool := anthropic.BetaToolParam{
			Name:        d.Name,
			Description: anthropic.String(d.Description),
		}
		if props, ok := d.InputSchema["properties"].(map[string]any); ok {
			tool.InputSchema = anthropic.BetaToolInputSchemaParam{Properties: props}
		}
		out = append(out, anthropic.BetaToolUnionParam{OfTool: &tool})
	}
	return out
}

// fromBetaMessage maps an accumulated Claude message back onto the seam
// Response — text, tool_use requests, and the stop_reason the agent branches on
// (a whole-chain refusal after the server-side fallback maps to StopRefusal).
func fromBetaMessage(msg anthropic.BetaMessage) llm.Response {
	var out llm.Response
	var text strings.Builder
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text.WriteString(b.Text)
		case anthropic.BetaToolUseBlock:
			// BetaToolUseBlock.Input is `any`, NOT []byte — the raw JSON comes off the
			// block's JSON view. This is what rotted: the adapter unmarshalled Input
			// directly, which stopped compiling when the SDK widened the field, and
			// nothing caught it because the `anthropic` build tag was never compiled by
			// CI. The only copilot binary that built was the stub.
			var input map[string]any
			if raw := b.JSON.Input.Raw(); raw != "" {
				_ = json.Unmarshal([]byte(raw), &input)
			}
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{ID: b.ID, Name: b.Name, Input: input})
		}
	}
	out.Text = text.String()
	switch msg.StopReason {
	case anthropic.BetaStopReasonToolUse:
		out.StopReason = llm.StopToolUse
	case anthropic.BetaStopReasonRefusal:
		out.StopReason = llm.StopRefusal
	default:
		out.StopReason = llm.StopEndTurn
	}
	return out
}

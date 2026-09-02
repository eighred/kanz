// Package llm is the copilot's Claude client seam (COPILOT-01a). The copilot is
// a tool-use agent driven by Claude (Opus 4.8): it asks the model a question,
// the model calls governed tools, and the model composes a grounded answer.
//
// # The Claude client is a seam (DEBT-02) — version-agnostic, the real client is composition-root
//
// Following the platform-wide stance every external side-effect takes (the OMS
// execution.Venue, the settlement SettlementVenue, the datamaster VendorFeed —
// a dependency-free default in-tree, the real adapter wired at the composition
// root), the Model interface here is the seam. It is deliberately NOT tied to one
// Claude version: the model id is CONFIGURATION (COPILOT_MODEL_ID), so switching
// versions — or even providers — is a config/composition-root change, never a
// code change. The agent and tools never name a version.
//
// The PRODUCTION implementation is the Anthropic client built on
// `github.com/anthropics/anthropic-sdk-go`, wired in `cmd/copilot` so the core
// service module pulls no LLM SDK dependency (the CLAUDE.md no-bloat rule + the
// EVT-15a build-without-external-regen stance). Its shape, parameterized by the
// configured model id (defaults to the MAX / most-capable model, DefaultModelID):
//
//	import "github.com/anthropics/anthropic-sdk-go"
//	client := anthropic.NewClient() // ANTHROPIC_API_KEY
//	// per Complete call — manual tool-use loop, adaptive thinking (the 4.6+/Fable family):
//	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
//	    Model:     anthropic.Model(cfg.ModelID), // e.g. "claude-fable-5" / "claude-opus-4-8"
//	    MaxTokens: 8192,
//	    System:    []anthropic.TextBlockParam{{Text: req.System}},
//	    Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
//	    Tools:     toolUnions(req.Tools),
//	    Messages:  toMessageParams(req.Messages),
//	})
//	// map resp.Content (TextBlock / ToolUseBlock) + resp.StopReason back into Response.
//	// (Per-version request quirks — e.g. Fable 5 omits an explicit thinking-disabled
//	//  and may return stop_reason "refusal" — are the adapter's concern, not the agent's.)
//
// The agent (internal/agent) drives the loop against THIS interface, so the
// scriptable StubModel exercises the whole tool-use + grounding + guard path in
// tests with no network and no key.
package llm

import "context"

// DefaultModelID is the copilot's default Claude model when COPILOT_MODEL_ID is
// unset — the MAX / most-capable model (Claude Fable 5). It is only a DEFAULT:
// the version is config-selected behind the Model seam, so a deployment switches
// versions (or pins an older one) without touching this package.
const DefaultModelID = "claude-fable-5"

// Role is a conversation turn's author.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ToolDef is a Claude tool-use function definition (name + description + JSON
// input schema). Mirrors anthropic.ToolParam.
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolCall is the model's request to invoke a tool (an assistant turn's
// tool_use block).
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolResult is the caller's reply to a ToolCall (a user turn's tool_result
// block). IsError marks a failed/denied tool so the model can adapt.
type ToolResult struct {
	ToolCallID string
	Content    string
	IsError    bool
}

// Message is one conversation turn. An assistant turn may carry ToolCalls; the
// user turn that follows carries the matching ToolResults.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// Request is one Complete call: the system prompt, the conversation so far, and
// the tools the model may call.
type Request struct {
	System   string
	Messages []Message
	Tools    []ToolDef
}

// StopReason mirrors the Anthropic stop_reason values the agent branches on.
type StopReason string

const (
	StopEndTurn StopReason = "end_turn"
	StopToolUse StopReason = "tool_use"
	StopRefusal StopReason = "refusal"
)

// Response is the model's reply for one turn.
type Response struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason StopReason

	// Model is the model identity that produced THIS turn — the provider's own
	// name and version, as it reported them (#971).
	//
	// PER TURN AND NOT PER DEPLOYMENT. Recording the configured model name would
	// be a claim about CONFIGURATION; this seam is provider-agnostic (#179) and an
	// adapter may route differently per call, so "which model actually answered"
	// is a question only the response can settle.
	//
	// AN EMPTY VALUE IS NOT AN ERROR HERE, and it is not defaulted either. A
	// provider that reports no identity leaves this empty, and the answer record
	// carries the empty entry rather than omitting the turn — an omission would
	// silently shorten the model list and make it disagree with the turn count,
	// which is the one thing that would make the record unreadable.
	Model string
}

// Model is the Claude seam (see the package doc for the production Opus 4.8
// adapter). Complete runs ONE turn — the agent loops, feeding tool results back,
// until StopEndTurn.
type Model interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

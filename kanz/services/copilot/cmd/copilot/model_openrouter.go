//go:build openrouter

// The OpenRouter adapter (#179). Behind the `openrouter` build tag for the same
// reason the Anthropic one is behind `anthropic`: the default build links no LLM
// SDK, which is what keeps `go build/vet` and the whole test suite free of a
// vendor dependency. A production image builds both and the deployment chooses
// with COPILOT_PROVIDER.
//
// WHY OPENROUTER IS ONE ADAPTER AND NOT ONE PER VENDOR. It fronts many models —
// Claude, GPT, DeepSeek, Llama — behind a single OpenAI-compatible endpoint, so
// the vendor becomes COPILOT_MODEL_ID rather than a compile-time decision. That
// is the whole reason it is worth having: the seam already made the model id
// configuration, and this makes the vendor follow it.
//
// THE MAPPING IS NOT WRITTEN YET, AND THAT IS DELIBERATE FOR THIS CHANGE.
// llm.ToolDef / ToolCall / StopReason are shaped on Anthropic's content blocks
// (see toBetaTools and fromBetaMessage in model_anthropic.go). Mapping OpenAI's
// tool-calling shape onto them faithfully is the work, and it decides whether
// internal/agent runs unchanged — which it must, since not changing the agent is
// the point of the seam.
//
// Two things that mapping has to answer, recorded here so they are not
// rediscovered:
//
//   - OPENAI HAS NO REFUSAL STOP REASON. Anthropic reports stop_reason
//     "refusal", and the agent BRANCHES on llm.StopRefusal. Collapsing a
//     provider's declines into StopEndTurn would make a refusal
//     indistinguishable from an answer on a surface whose whole job is grounded
//     analysis. So this adapter must detect refusal itself — and, because a
//     heuristic that silently stops matching is the same defect wearing a
//     different hat, that detection has to be logged and counted rather than
//     assumed.
//   - THE SERVER-SIDE FALLBACK HAS NO EQUIVALENT. model_anthropic.go attaches
//     `fallbacks` so a Fable/Mythos decline is re-served by claude-opus-4-8
//     inside the same call, which means the Anthropic path ABSORBS refusals this
//     one will surface. That is a behaviour difference between providers, not a
//     bug, and it belongs in front of whoever compares them.
package main

import (
	"errors"
	"log/slog"

	"github.com/eighred/kanz/services/copilot/internal/config"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

// ProviderOpenRouter is this adapter's COPILOT_PROVIDER value.
const ProviderOpenRouter = "openrouter"

func init() { registerProvider(ProviderOpenRouter, newOpenRouterModel) }

// newOpenRouterModel refuses to start until the tool-use mapping exists.
//
// REFUSING AT STARTUP, NOT AT THE FIRST QUESTION. A model that registers and
// then errors inside Complete would let the service come up healthy, pass its
// readiness probe, and fail only when a portfolio manager asked something. An
// adapter that cannot answer must not be a running copilot.
//
// This is the seam and the wiring proven — the tag compiles, the provider
// registers, COPILOT_PROVIDER resolves it — with the mapping still to come.
func newOpenRouterModel(_ config.Config, _ *slog.Logger) (llm.Model, error) {
	return nil, errors.New("the OpenRouter adapter is not implemented yet: the tool-use and " +
		"StopReason mapping lands separately (#179). This binary links the provider but cannot " +
		"answer with it — use COPILOT_PROVIDER=anthropic, or build without -tags openrouter")
}

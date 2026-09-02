// Package agent is the copilot orchestrator (COPILOT-01): it drives the Claude
// tool-use loop, authorizing every tool call against the calling Principal,
// grounding the answer in cited governed data, and screening both the tool
// results (prompt-injection) and the final output (hallucinated measures).
//
// Identity is load-bearing: Ask requires an authenticated Principal and threads
// it into every tool authorization, so the copilot can never surface data the
// caller could not read directly — and a cross-tenant probe is denied and logged
// by the AUTH-01 AuditedAuthorizer the tool registry holds.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
	"github.com/eighred/kanz/services/copilot/internal/tools"
)

// ErrUnauthenticated is returned when Ask is called without a Principal —
// the copilot is never anonymous (AUTH-01).
var ErrUnauthenticated = errors.New("copilot: no authenticated principal")

// DefaultMaxTurns bounds the tool-use loop so a misbehaving model cannot spin
// forever.
const DefaultMaxTurns = 8

// systemPrompt constrains the model to grounded, tool-sourced answers. Phrased
// per the Opus-4.8 prompting guidance: prescriptive about WHEN to use tools,
// explicit that ungrounded numbers are not allowed.
const systemPrompt = `You are Kanz Copilot, an analytics assistant over a governed risk platform.
Answer ONLY from the data returned by the provided tools. Never invent or estimate a
number that a tool did not return. For every portfolio question, call the appropriate
tool, then answer from its result and cite the source it reported. If a tool denies
access or returns no data, say so plainly — do not guess. Be concise and lead with the
answer.`

// Answer is a grounded copilot response.
type Answer struct {
	Text      string
	Citations []retrieval.Citation
	// Grounded is false when the output review found a numeric claim not backed
	// by any cited tool value (a possible hallucination).
	Grounded   bool
	Ungrounded []string
	// Refused is true when the model declined (stop_reason refusal).
	Refused bool
	// BudgetExhausted is true when the tool-use loop hit maxTurns without the
	// model reaching a final answer.
	//
	// DISTINCT FROM Refused (#971). A model that declined and one that never
	// converged are different facts with different owners — the first is the model
	// working, the second is the earliest sign of one that has stopped — and they
	// previously returned the same shape with a different sentence.
	BudgetExhausted bool
	// InjectionFlagged is true when a tool result tripped the prompt-injection
	// guard during this answer.
	InjectionFlagged bool
}

// Agent is the copilot. It is stateless across calls; all per-request identity
// flows through ctx + the Principal passed to Ask.
type Agent struct {
	model    llm.Model
	tools    *tools.Registry
	maxTurns int

	// recorder keeps what the model was shown (#971). Nil records nothing, which
	// the composition root reports at startup rather than leaving the estate to
	// discover when an answer cannot be explained.
	recorder     AnswerRecorder
	onRecordLost func(error)
	onAnswer     func(Observation)
	now          func() time.Time
	log          *slog.Logger
}

// New builds an agent over a Claude model and the governed tool registry.
func New(model llm.Model, registry *tools.Registry, opts ...Option) *Agent {
	a := &Agent{model: model, tools: registry, maxTurns: DefaultMaxTurns}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Ask runs the tool-use loop for one question under the Principal and returns a
// grounded answer. Every tool call is authorized against p; the model only ever
// sees data p is permitted to read.
func (a *Agent) Ask(ctx context.Context, p *auth.Principal, question string) (Answer, error) {
	if p == nil {
		return Answer{}, ErrUnauthenticated
	}

	req := llm.Request{
		System:   systemPrompt,
		Tools:    a.tools.Defs(),
		Messages: []llm.Message{{Role: llm.RoleUser, Text: question}},
	}

	var citations []retrieval.Citation
	var citedValues []float64
	injectionFlagged := false
	trace := loopTrace{answerID: newAnswerID(a.clock())}

	for turn := 0; turn < a.maxTurns; turn++ {
		resp, err := a.model.Complete(ctx, req)
		trace.turns++
		trace.models = append(trace.models, resp.Model)
		if err != nil {
			// THE FAILURE IS RECORDED TOO. A loop that could not complete is a fact
			// about the agent, and recording only the successes would leave exactly
			// the failures unexplained (#971).
			ans := Answer{Citations: citations, InjectionFlagged: injectionFlagged}
			a.record(ctx, p, question, ans, trace, true)
			return Answer{}, err
		}
		if resp.StopReason == llm.StopRefusal {
			ans := Answer{Text: resp.Text, Refused: true, Grounded: true, InjectionFlagged: injectionFlagged}
			a.record(ctx, p, question, ans, trace, false)
			return ans, nil
		}

		if len(resp.ToolCalls) == 0 {
			// Final answer — review grounding against everything the tools returned.
			grounding := ReviewOutput(resp.Text, citedValues)
			ans := Answer{
				Text:             resp.Text,
				Citations:        citations,
				Grounded:         grounding.Grounded,
				Ungrounded:       grounding.Ungrounded,
				InjectionFlagged: injectionFlagged,
			}
			a.record(ctx, p, question, ans, trace, false)
			return ans, nil
		}

		// Record the assistant tool-call turn, then invoke each tool.
		req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
		var results []llm.ToolResult
		for _, call := range resp.ToolCalls {
			trace.toolCalls++
			out := a.tools.Invoke(ctx, p, call)
			if out.IsError {
				trace.toolErrors++
			}
			// COVERAGE IS ACCUMULATED FOR EVERY CALL, error results included. A tool
			// that refused returns 0/0, so counting unconditionally needs no second
			// rule about which results are eligible — and the alternative, skipping
			// errors, would make a loop whose reads were all denied look like a loop
			// that was shown a complete book.
			trace.measured += out.Measured
			trace.unavailable += out.Unavailable
			content, flagged := ScanToolResult(out.Content)
			if flagged {
				injectionFlagged = true
			}
			if !out.IsError {
				citations = append(citations, out.Citations...)
				citedValues = append(citedValues, out.Values...)
			}
			results = append(results, llm.ToolResult{ToolCallID: call.ID, Content: content, IsError: out.IsError})
		}
		req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser, ToolResults: results})
	}

	ans := Answer{
		Text:            "I could not complete the request within the tool-use budget.",
		Citations:       citations,
		Grounded:        true,
		BudgetExhausted: true,
		//InjectionFlagged carries forward so an exhausted loop that was fed a
		// poisoned result is still visible as such.
		InjectionFlagged: injectionFlagged,
	}
	a.record(ctx, p, question, ans, trace, false)
	return ans, nil
}

// CitationsString renders an answer's citations for display/logging.
func CitationsString(ans Answer) string {
	if len(ans.Citations) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ans.Citations))
	for _, c := range ans.Citations {
		parts = append(parts, c.String())
	}
	return strings.Join(parts, " ")
}

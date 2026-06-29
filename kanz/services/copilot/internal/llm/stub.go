package llm

import "context"

// StubModel is the dependency-free default Model — the SimVenue analog for the
// Claude client. It replays a scripted sequence of Responses, one per Complete
// call, so tests drive the agent's tool-use loop deterministically (turn 1: call
// a tool; turn 2: the grounded final answer). Past the script it returns an
// empty end-turn. This is what lets the whole copilot path — tool authorization,
// citations, the prompt-injection guard, the output review — run with no network
// and no API key.
type StubModel struct {
	Script []Response
	calls  int
}

// NewStubModel returns a StubModel that replays script.
func NewStubModel(script ...Response) *StubModel { return &StubModel{Script: script} }

// Complete returns the next scripted response.
func (m *StubModel) Complete(_ context.Context, _ Request) (Response, error) {
	if m.calls >= len(m.Script) {
		return Response{StopReason: StopEndTurn}, nil
	}
	r := m.Script[m.calls]
	m.calls++
	if r.StopReason == "" {
		if len(r.ToolCalls) > 0 {
			r.StopReason = StopToolUse
		} else {
			r.StopReason = StopEndTurn
		}
	}
	return r, nil
}

// Calls reports how many times Complete was invoked — tests assert the loop ran
// the expected number of turns.
func (m *StubModel) Calls() int { return m.calls }

package main

import (
	"errors"
	"log/slog"

	"github.com/eighred/kanz/services/copilot/internal/config"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

// ProviderStub is the fabricating model's COPILOT_PROVIDER value.
const ProviderStub = "stub"

// NO BUILD TAG ANY MORE (#179). This file used to be `//go:build !anthropic` and
// define newModel, which made "the stub" mean "whatever you get when you forget
// a flag". It is now a provider like any other: always linked, because it costs
// nothing and the test suite needs it, and reachable only by ASKING for it.
//
// That is a strictly stronger guarantee than the build tag gave. A default build
// asked for COPILOT_PROVIDER=anthropic now fails saying anthropic is not linked,
// where before it silently fell through to this.
func init() { registerProvider(ProviderStub, newStubModel) }

// newStubModel returns the dependency-free StubModel.
//
// THE STUB DOES NOT ANSWER QUESTIONS — IT FABRICATES ANSWERS. Its replies reach
// a portfolio manager as ANALYSIS and read exactly like real ones; there is no
// dashboard on which this looks wrong.
//
// So it stays an explicit, affirmative opt-in. This is the same rule that
// removed the OKX endpoint default (#147) and the SimVenue/SimFeed fallbacks:
// fabrication must never be reachable by omission — and now not by a forgotten
// build flag either, since selecting it takes naming it twice.
func newStubModel(cfg config.Config, logger *slog.Logger) (llm.Model, error) {
	if !cfg.AllowStub {
		return nil, errors.New("COPILOT_PROVIDER=stub, but the stub does not answer questions — it " +
			"FABRICATES them. Set COPILOT_ALLOW_STUB=true if fabricated analysis is genuinely what " +
			"you want, or choose a real provider")
	}
	logger.Warn("SERVING FABRICATED ANALYSIS — the copilot is running the StubModel. Every answer is invented, not derived from the portfolio")
	return llm.NewStubModel(), nil
}

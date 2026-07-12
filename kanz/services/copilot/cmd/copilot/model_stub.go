//go:build !anthropic

package main

import (
	"log/slog"
	"os"

	"github.com/kanz-eng/kanz/services/copilot/internal/config"
	"github.com/kanz-eng/kanz/services/copilot/internal/llm"
)

// newModel returns the dependency-free StubModel in the default build. The real
// anthropic-sdk-go client is compiled in ONLY under the `anthropic` build tag
// (model_anthropic.go), so the default module build — and every `go build/vet`
// + test run — pulls no LLM SDK dependency (the CLAUDE.md no-bloat rule + the
// EVT-15a build-without-external-regen stance). Production images build the
// binary with `-tags anthropic`.
func newModel(cfg config.Config, logger *slog.Logger) llm.Model {
	// THE STUB DOES NOT ANSWER QUESTIONS — IT FABRICATES ANSWERS.
	//
	// This binary was built without -tags anthropic, so no Claude client is linked
	// and every reply comes from llm.StubModel. Those replies reach a portfolio
	// manager as ANALYSIS, and they read exactly like real ones — there is no
	// dashboard on which this looks wrong.
	//
	// It used to log this at Info and serve. That is the same defect as the OMS
	// silently routing to SimVenue and market-ingest silently publishing SimFeed
	// prices: fabrication reachable by forgetting a build flag. It is now an
	// explicit opt-in and a hard failure otherwise.
	if !cfg.AllowStub {
		logger.Error("copilot cannot start: built WITHOUT -tags anthropic, so the only model available is the STUB — it does not answer questions, it FABRICATES them. " +
			"Build the production image (-tags anthropic), or set COPILOT_ALLOW_STUB=true if fabricated analysis is genuinely what you want")
		os.Exit(2)
	}
	logger.Warn("SERVING FABRICATED ANALYSIS — the copilot is running the StubModel. Every answer is invented, not derived from the portfolio")
	return llm.NewStubModel()
}

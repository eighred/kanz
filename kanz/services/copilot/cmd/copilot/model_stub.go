//go:build !anthropic

package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/services/copilot/internal/config"
	"github.com/kanz-eng/kanz/services/copilot/internal/llm"
)

// newModel returns the dependency-free StubModel in the default build. The real
// anthropic-sdk-go client is compiled in ONLY under the `anthropic` build tag
// (model_anthropic.go), so the default module build — and every `go build/vet`
// + test run — pulls no LLM SDK dependency (the CLAUDE.md no-bloat rule + the
// EVT-15a build-without-external-regen stance). Production images build the
// binary with `-tags anthropic`.
func newModel(_ config.Config, logger *slog.Logger) llm.Model {
	logger.Info("copilot llm: stub model (built without -tags anthropic)")
	return llm.NewStubModel()
}

package main

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/eighred/kanz/services/copilot/internal/config"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

// THE PROVIDER IS A RUNTIME CHOICE, LINKED AT BUILD TIME (#179).
//
// This used to be one function with two definitions — model_anthropic.go under
// `//go:build anthropic`, model_stub.go under `//go:build !anthropic` — so which
// model the copilot spoke to was decided by whoever built the image. An operator
// could pick an image; they could not pick a provider.
//
// Two adapters cannot both define newModel once both are linked, so selection
// moves here. The split is deliberate and is the hybrid the design settled on:
//
//	COMPILED IN   by build tag. The default build links no LLM SDK at all, which
//	              is what keeps `go build/vet` and the whole test suite free of a
//	              vendor SDK. anthropic-sdk-go is already a module dependency
//	              (go.mod), so the tag never kept it out of the graph — but it
//	              does keep it out of the binary, measured at +14.8MB (23.8MB ->
//	              38.5MB) for one SDK. A second would compound it.
//	SELECTED      at runtime by COPILOT_PROVIDER, among what is linked.
//
// So a production image builds `-tags anthropic,openrouter` and the deployment
// chooses; a developer builds neither and gets a binary that refuses to serve
// rather than one that quietly invents answers.
//
// FAIL CLOSED, ALWAYS. An unset or unrecognised provider is an error naming what
// this binary actually has, never a default. The precedent is exact: the OKX
// endpoint has no default because "nobody configured this" and "somebody chose
// production" must not be the same state (#147), and model_stub.go already exits
// rather than serving fabricated analysis by omission. A provider reached by
// omission is that same defect.

// providerBuilder constructs one provider's llm.Model.
//
// It returns an error rather than exiting so the failure is testable and so the
// composition root reports it with everything else — a library that calls
// os.Exit cannot be exercised by a test that asserts the refusal.
type providerBuilder func(cfg config.Config, logger *slog.Logger) (llm.Model, error)

// providerBuilders holds what THIS BINARY was linked with. Populated by each
// adapter's init(), so the map's contents are a fact about the build rather than
// a list somebody has to keep in step with the build tags.
var providerBuilders = map[string]providerBuilder{}

// registerProvider is called from an adapter's init(). A duplicate name is a
// programming error in the composition root and panics: it means two adapters
// claim one provider id, and picking either silently would make which model
// answers depend on file order.
func registerProvider(name string, build providerBuilder) {
	if _, dup := providerBuilders[name]; dup {
		panic("copilot: duplicate llm provider registered: " + name)
	}
	providerBuilders[name] = build
}

// linkedProviders is what this binary can be asked for, sorted for a stable
// error message.
func linkedProviders() []string {
	out := make([]string, 0, len(providerBuilders))
	for name := range providerBuilders {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// newModel resolves COPILOT_PROVIDER against what this binary was linked with.
func newModel(cfg config.Config, logger *slog.Logger) (llm.Model, error) {
	linked := linkedProviders()

	if cfg.Provider == "" {
		return nil, fmt.Errorf("COPILOT_PROVIDER is required and has no default: this binary is "+
			"linked with %v, and which model answers a portfolio manager's question is not a "+
			"decision to make by omission. Set it explicitly", linked)
	}
	build, ok := providerBuilders[cfg.Provider]
	if !ok {
		// Naming what IS linked matters more than naming what is not: the usual
		// cause is an image built without the tag for the provider the deployment
		// asks for, and "openrouter is not linked, this binary has [anthropic
		// stub]" says that, where "unknown provider" does not.
		return nil, fmt.Errorf("COPILOT_PROVIDER=%q is not linked into this binary, which has %v. "+
			"Either set one of those, or build the image with the matching tag "+
			"(go build -tags %s)", cfg.Provider, linked, cfg.Provider)
	}
	return build(cfg, logger)
}

package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/copilot/internal/config"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// withProviders swaps the registry for one test and restores it, so a test
// cannot leave a fake provider behind for the next one.
func withProviders(t *testing.T, m map[string]providerBuilder) {
	t.Helper()
	saved := providerBuilders
	providerBuilders = m
	t.Cleanup(func() { providerBuilders = saved })
}

// AN UNSET PROVIDER IS A REFUSAL, NOT A DEFAULT.
//
// The precedent is exact: the OKX endpoint has no default because "nobody
// configured this" and "somebody chose production" must not be the same state
// (#147). Which model answers a portfolio manager's question is the same class
// of decision.
func TestAnUnsetProviderRefusesToStart(t *testing.T) {
	withProviders(t, map[string]providerBuilder{
		"anthropic": func(config.Config, *slog.Logger, prometheus.Registerer) (llm.Model, error) {
			return llm.NewStubModel(), nil
		},
	})

	_, err := newModel(config.Config{}, testLogger(), nil)
	if err == nil {
		t.Fatal("an unset COPILOT_PROVIDER started the copilot — the provider would be chosen by omission")
	}
	// ASSERT THE SPECIFIC WORDING, not merely that something failed.
	//
	// Mutation testing caught this: disabling the empty check still errored,
	// because "" falls through to the map lookup and comes back as
	// `COPILOT_PROVIDER="" is not linked into this binary`. That is fail-closed,
	// so nothing unsafe — but it is the wrong message. "is required and has no
	// default" tells an operator to set it; "\"\" is not linked" invites them to
	// go looking for a build tag that does not exist.
	//
	// A test that only asserts "an error happened" cannot tell those apart, and
	// the message is most of the value of failing closed.
	if !strings.Contains(err.Error(), "is required and has no default") {
		t.Errorf("error %q does not say the variable is REQUIRED — an operator would read it as a "+
			"missing build tag rather than a missing setting", err)
	}
	// The message must say what this binary HAS, or the operator is guessing.
	if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("error %q does not list the linked providers", err)
	}
}

// A PROVIDER THAT IS NOT LINKED NAMES WHAT IS.
//
// The usual cause is an image built without the tag the deployment asks for, and
// "openrouter is not linked, this binary has [anthropic stub]" says that where
// "unknown provider" does not.
func TestAnUnlinkedProviderNamesWhatIsLinked(t *testing.T) {
	withProviders(t, map[string]providerBuilder{
		"anthropic": func(config.Config, *slog.Logger, prometheus.Registerer) (llm.Model, error) {
			return llm.NewStubModel(), nil
		},
		"stub": func(config.Config, *slog.Logger, prometheus.Registerer) (llm.Model, error) {
			return llm.NewStubModel(), nil
		},
	})

	_, err := newModel(config.Config{Provider: "openrouter"}, testLogger(), nil)
	if err == nil {
		t.Fatal("an unlinked provider was accepted")
	}
	for _, want := range []string{"openrouter", "anthropic", "stub", "-tags"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q — it must name what was asked for, what exists, and how to fix it",
				err, want)
		}
	}
}

// The linked provider is actually used, and the builder's error is surfaced
// rather than swallowed.
func TestALinkedProviderIsBuiltAndItsErrorSurfaces(t *testing.T) {
	built := false
	withProviders(t, map[string]providerBuilder{
		"anthropic": func(config.Config, *slog.Logger, prometheus.Registerer) (llm.Model, error) {
			built = true
			return llm.NewStubModel(), nil
		},
	})
	if _, err := newModel(config.Config{Provider: "anthropic"}, testLogger(), nil); err != nil {
		t.Fatalf("a linked provider failed to build: %v", err)
	}
	if !built {
		t.Error("the registered builder was never called")
	}

	withProviders(t, map[string]providerBuilder{
		"broken": func(config.Config, *slog.Logger, prometheus.Registerer) (llm.Model, error) {
			return nil, errNotToday
		},
	})
	if _, err := newModel(config.Config{Provider: "broken"}, testLogger(), nil); err == nil {
		t.Error("a builder's error was swallowed — the copilot would start with a nil model")
	}
}

var errNotToday = errStr("not today")

type errStr string

func (e errStr) Error() string { return string(e) }

// THE STUB IS REACHABLE ONLY BY ASKING FOR IT TWICE.
//
// It used to be what you got when you forgot `-tags anthropic`. Now selecting it
// takes naming it in COPILOT_PROVIDER *and* setting COPILOT_ALLOW_STUB — because
// its answers reach a portfolio manager as analysis and read exactly like real
// ones.
func TestTheStubRequiresAnExplicitOptInEvenWhenSelected(t *testing.T) {
	_, err := newStubModel(config.Config{Provider: ProviderStub}, testLogger(), nil)
	if err == nil {
		t.Fatal("the stub was served without COPILOT_ALLOW_STUB — fabricated analysis reachable by " +
			"selecting a provider is the same defect as reaching it by forgetting a build flag")
	}
	if !strings.Contains(err.Error(), "FABRICATE") {
		t.Errorf("error %q does not say what the stub actually does", err)
	}

	m, err := newStubModel(config.Config{Provider: ProviderStub, AllowStub: true}, testLogger(), nil)
	if err != nil {
		t.Fatalf("the stub refused an explicit opt-in: %v", err)
	}
	if m == nil {
		t.Error("the stub opt-in returned a nil model")
	}
}

// The stub is ALWAYS linked — it costs nothing and the test suite needs it — so
// a default build has something to be asked for rather than nothing at all.
func TestTheStubIsLinkedInEveryBuild(t *testing.T) {
	if _, ok := providerBuilders[ProviderStub]; !ok {
		t.Errorf("the stub provider is not registered; this binary links %v", linkedProviders())
	}
}

// Two adapters claiming one id would make which model answers depend on file
// order. That is a composition-root bug, so it panics rather than picking.
func TestRegisteringADuplicateProviderPanics(t *testing.T) {
	withProviders(t, map[string]providerBuilder{})
	registerProvider("dup", func(config.Config, *slog.Logger, prometheus.Registerer) (llm.Model, error) { return nil, nil })

	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate provider did not panic — which model answers would " +
				"depend on which file init() ran first")
		}
	}()
	registerProvider("dup", func(config.Config, *slog.Logger, prometheus.Registerer) (llm.Model, error) { return nil, nil })
}

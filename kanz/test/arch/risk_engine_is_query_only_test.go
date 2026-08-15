package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE RISK ENGINE'S API TAKES QUERIES, NEVER COMMANDS (#513).
//
// internal/risk/api/v1's own doc states the rule: all state changes are
// event-driven, so the interface is query + scenario + health only. State arrives
// on the bus (RISK-04 ingests domain FACTs) and risk output leaves on the bus
// (RISK-10 publishes it). A caller that wants to change something publishes; it
// does not call.
//
// # Why this is a guard now
//
// The rule was recorded in a design document that was deleted on 2026-07-29 — so
// the comment asserting it was pointing at nothing, which is what #513 is about.
// A rule whose only evidence is a citation to a file nobody can
// open is a rule that gets broken by someone who reasonably concluded it had been
// withdrawn.
//
// # What a violation would actually cost
//
// A mutating method here is not a style problem. It is a SECOND WAY FOR STATE TO
// CHANGE, beside the bus, and the two do not reconcile: the bus path is ordered,
// replayable and audited, and a direct call is none of those. Every consumer that
// rebuilds risk state by replaying the stream would silently disagree with the
// engine that took a call — and disagree only for the portfolios somebody used
// the shortcut on, which is the hardest possible version of the bug to find.
//
// # Names, not types
//
// This matches on method NAMES rather than trying to decide what mutates. That is
// a blunt instrument and it is the right one here: the rule is about the SHAPE of
// the surface, a reviewer applies exactly this test by eye, and a mutating method
// that names itself innocently is a deliberate act rather than an oversight —
// which no static check catches anyway.

// mutatingVerbs are the prefixes a state-changing method would plausibly carry.
var mutatingVerbs = []string{
	"Apply", "Mutate", "Set", "Update", "Delete", "Remove", "Command",
	"Write", "Put", "Post", "Create", "Submit", "Execute", "Ingest", "Record",
}

func TestRiskEngineAPIDeclaresNoMutatingMethod(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "risk", "api", "v1", "engine.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var methods []string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Engine" {
			return true
		}
		iface, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		found = true
		for _, m := range iface.Methods.List {
			for _, name := range m.Names {
				methods = append(methods, name.Name)
			}
		}
		return false
	})

	// NON-VACUITY. A renamed interface or a moved file would leave this passing
	// having examined nothing — the failure mode every guard in this directory has
	// to rule out about itself.
	if !found {
		t.Fatalf("no `Engine` interface in %s — it was renamed or moved, and this guard is "+
			"checking nothing", path)
	}
	if len(methods) < 2 {
		t.Fatalf("found %d methods on Engine (%v) — the interface parse is broken, not the "+
			"estate", len(methods), methods)
	}

	var bad []string
	for _, m := range methods {
		for _, verb := range mutatingVerbs {
			if strings.HasPrefix(m, verb) {
				bad = append(bad, m)
				break
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("api/v1.Engine declares mutating method(s) %v.\n"+
			"This surface is query + scenario + health only: all state changes are "+
			"event-driven. A mutating method here is a SECOND way for state to change, beside "+
			"the bus, and the two do not reconcile — the bus path is ordered, replayable and "+
			"audited, and a direct call is none of those. Consumers that rebuild risk state by "+
			"replaying the stream would disagree with the engine, and only for the portfolios "+
			"somebody used the shortcut on (#513).",
			bad)
	}
	t.Logf("api/v1.Engine: %d query methods, none mutating — %v", len(methods), methods)
}

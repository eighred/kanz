package main

// THE MARGIN CONTROL'S COMPOSITION ROOT (#408, control 3).
//
// Two properties, neither of which has a unit under it, and both of which fail
// SILENTLY when they are wrong.
//
//  1. The pre-trade gate must be GIVEN the margin source. Without it,
//     Candidate.Margin is nil and every portfolio whose mandate declares margin
//     trading is refused permanently — a fail-closed direction, so no alarm
//     fires, no test reddens, and the symptom is a desk that cannot trade with a
//     refusal reason that reads exactly like a degraded venue feed.
//
//  2. The venue-margin fold must be BROADCAST, not a work queue. A durable
//     consumer group load-balances (pkg/bus/consumer.go), so with the shipped
//     replicas: 2 each pod would hold a DIFFERENT margin state: one refuses the
//     order, the other admits it. On a control whose entire purpose is to fail
//     closed, half the pods failing closed is not a weaker version of the
//     control — it is the control not existing, plus a coin toss.
//
// This is the same class of property, and the same shape of guard, as
// TestPriceSpineIsBroadcastNotAWorkQueue directly beside it. It reads the
// composition root's own AST rather than its text, so it cannot be satisfied by
// the paragraph above mentioning the names it looks for.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// namesVenueMarginSubject reports whether the node mentions venuemargin.Subject.
func namesVenueMarginSubject(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Subject" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "venuemargin" {
				found = true
			}
		}
		return !found
	})
	return found
}

// marginWiring reads main.go and reports how the venue-margin state is
// subscribed and whether the pre-trade gate is given a margin source.
func marginWiring(t *testing.T) (broadcast, workQueue, sourceWired bool) {
	t.Helper()
	// THE WHOLE COMPOSITION ROOT, NOT main.go ALONE. #643 moved the gate's
	// construction into pretrade.go, and a guard pinned to one file reported the
	// margin source missing for a source that had merely moved — a false alarm on
	// a control whose real failure is silent, which is the worst direction for a
	// guard to be wrong in.
	files := compositionRootFiles(t)
	inspect := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// comp.WithMarginSource(...) — the gate option that hands the rule its
		// input. Matched on the selector so a differently named local alias of the
		// compliance package still counts.
		if sel.Sel.Name == "WithMarginSource" {
			sourceWired = true
			return true
		}
		if !namesVenueMarginSubject(call) {
			return true
		}
		switch {
		case strings.HasPrefix(sel.Sel.Name, "SubscribeBroadcast"):
			broadcast = true
		case sel.Sel.Name == "Subscribe":
			workQueue = true
		}
		return true
	}
	for _, f := range files {
		ast.Inspect(f, inspect)
	}
	return broadcast, workQueue, sourceWired
}

func TestVenueMarginIsBroadcastAndReachesThePreTradeGate(t *testing.T) {
	broadcast, workQueue, sourceWired := marginWiring(t)

	if !sourceWired {
		t.Error("the pre-trade gate is built with NO margin source (comp.WithMarginSource is not " +
			"called): Candidate.Margin is nil, so every portfolio whose mandate declares " +
			"RULE_TYPE_VENUE_MARGIN is refused forever, with a reason indistinguishable from a " +
			"venue feed that has gone quiet. #408 control 3 would be present in the code and " +
			"absent from the running process")
	}
	if workQueue {
		t.Error("venuemargin.Subject is wired through the WORK-QUEUE path (consumer.Subscribe). " +
			"A consumer group load-balances, so each OMS replica would fold only some of the " +
			"observations and hold a DIFFERENT margin state — the same order refused by one pod " +
			"and admitted by the other")
	}
	if !broadcast {
		t.Error("venuemargin.Subject is not subscribed via SubscribeBroadcast — the exchange's " +
			"margin state is replicated STATE, not work, and every replica's pre-trade gate " +
			"needs all of it")
	}
}

// compositionRootFiles parses every non-test Go file of this package — the whole
// composition root, which since #643 is more than main.go.
//
// A DIRECTORY WALK RATHER THAN parser.ParseDir: that function is deprecated as
// of Go 1.25 and golangci-lint, this repository's fourth gate, refuses it.
func compositionRootFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the composition root: %v", err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("no non-test Go files in the composition root — this guard is parsing nothing")
	}
	return out
}

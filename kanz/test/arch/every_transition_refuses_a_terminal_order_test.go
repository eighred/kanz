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

// EVERY ORDER TRANSITION REFUSES A TERMINAL ORDER (#840).
//
// # The asymmetry this was written for
//
// aggregate.go decides which order transitions are legal. Four of its six
// enforced that themselves — ApplyFill, Cancel, Expire and Amend each open with
// IsTerminal(st). Route and Reject did not: both cloned the state and wrote
// ROUTED / REJECTED unconditionally.
//
// Neither was reachable. resume() checks IsTerminal(fresh) before calling
// work(), Reconcile() refuses a terminal order outright, and the per-order claim
// plus Store.Save's version predicate stop two writers interleaving. So the
// invariant was held by a caller-side check in one function plus a lock and a CAS
// in two other files — while a reader of aggregate.go saw four transitions
// refusing a terminal state and reasonably concluded the aggregate enforced it.
//
// That is the shape worth failing a build over: not a bug that was firing, but a
// rule that was true of most of a file and assumed of all of it. A new caller of
// work(), or a reshuffle of resume()'s guards, made an order the ledger calls
// ROUTED after it was CANCELLED — which resume() and the sweep treat as live, so
// a withdrawn order is re-driven to an exchange.
//
// # The transition set is DERIVED, not listed
//
// A hand-written list of transitions in this file would be a second copy of the
// thing that broke, and would say nothing about the seventh transition somebody
// adds next year. So the set comes from the signatures: an exported function in
// aggregate.go that TAKES an order state and RETURNS one is, by definition, a
// transition, and must consult IsTerminal.
//
// That rule picks out exactly the six and excludes the two neighbours for the
// right reasons rather than by name:
//
//   - Accept takes a *SubmitOrder, not a state — it creates the first state, and
//     there is no prior status to test.
//   - IsTerminal returns a bool, not a state — it is the predicate itself.
//
// # It checks that the predicate is CONSULTED, not that it is obeyed
//
// A transition that calls IsTerminal and ignores the answer passes here; that
// half is behaviour, and services/oms/internal/order/aggregate_terminal_test.go
// covers it for every terminal status. What no behavioural test can cover is the
// transition that does not exist yet, and this fails the build at exactly the
// moment one is added without a guard.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. Every paragraph
// above names IsTerminal; the check below runs over parsed identifiers inside a
// named function's BODY, with comments not attached, so this text cannot satisfy
// it.
const aggregateFile = "services/oms/internal/order/aggregate.go"

// orderStateType is the type whose presence in both the parameters and the
// results makes a function a transition.
const orderStateType = "OrderState"

// terminalGuardCall is what a transition must consult.
const terminalGuardCall = "IsTerminal"

// terminalGuardExempt names a transition permitted not to consult IsTerminal,
// and the issue that retires the entry.
//
// EMPTY, AND THAT IS THE POINT. An entry here is a decision that some transition
// may act on an order that is already finished — a fill folded into a cancelled
// order, a rejection written over a completed trade. That has to be argued in
// writing before it is true in code, and there is no argument for it: the four
// transitions that always guarded are the precedent, not the exception.
var terminalGuardExempt = map[string]string{}

func TestEveryOrderTransitionRefusesATerminalOrder(t *testing.T) {
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(aggregateFile))
	fset := token.NewFileSet()
	// Mode 0: comments are not attached, so this cannot match its own prose.
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", aggregateFile, err)
	}

	var checked, guarded []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil || !fn.Name.IsExported() {
			continue
		}
		if !takesOrderState(fn) || !returnsOrderState(fn) {
			continue
		}
		name := fn.Name.Name
		if _, ok := terminalGuardExempt[name]; ok {
			continue
		}
		checked = append(checked, name)
		if callsIdent(fn.Body, terminalGuardCall) {
			guarded = append(guarded, name)
		}
	}

	sort.Strings(checked)
	sort.Strings(guarded)

	// A GUARD THAT FOUND NO TRANSITIONS IS NOT A PASSING GUARD. If the aggregate
	// moves, or the state type is renamed, the loop above goes quiet and this
	// would report success while asserting nothing at all.
	if len(checked) < 4 {
		t.Fatalf("found only %d transitions in %s (%s) — a transition is an exported func taking "+
			"and returning an *orderpb.%s. If the aggregate moved or the type was renamed, point "+
			"this guard at the new one; it is asserting almost nothing as it stands",
			len(checked), aggregateFile, strings.Join(checked, ", "), orderStateType)
	}

	for _, name := range checked {
		if !namesTransition(guarded, name) {
			t.Errorf("%s takes an order state and returns a new one, so it is a transition, and "+
				"it never consults %s.\n"+
				"An order that is CANCELLED, EXPIRED or FILLED has finished; a transition that "+
				"acts on one anyway writes a live-looking status over a terminal record, and "+
				"resume() and the sweep then treat that order as working — a withdrawn order "+
				"re-driven to an exchange, or a completed trade overwritten. The other "+
				"transitions in this file open with `if %s(st)`; do the same, or add %s to "+
				"terminalGuardExempt with the argument for why this one may act on a finished "+
				"order", name, terminalGuardCall, terminalGuardCall, name)
		}
	}

	// DEAD-ENTRY ARM: an exemption naming a transition that no longer exists, or
	// one that has since grown its guard, is permission nobody needs and reads as
	// a decision somebody made.
	for name, reason := range terminalGuardExempt {
		if !namesTransition(checked, name) && !namesTransition(guarded, name) {
			t.Errorf("exemption for %q (%s) matches no transition in %s — delete it",
				name, reason, aggregateFile)
		}
	}
}

func takesOrderState(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, p := range fn.Type.Params.List {
		if isPointerTo(p.Type, orderStateType) {
			return true
		}
	}
	return false
}

func returnsOrderState(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, r := range fn.Type.Results.List {
		if isPointerTo(r.Type, orderStateType) {
			return true
		}
	}
	return false
}

// isPointerTo reports whether expr is *pkg.Name or *Name.
func isPointerTo(expr ast.Expr, name string) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch t := star.X.(type) {
	case *ast.SelectorExpr:
		return t.Sel.Name == name
	case *ast.Ident:
		return t.Name == name
	}
	return false
}

// callsIdent reports whether body calls a plain function named name.
func callsIdent(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func namesTransition(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

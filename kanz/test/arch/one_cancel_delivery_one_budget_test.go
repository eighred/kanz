package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A COMMAND DELIVERY'S CONTEXT IS THREADED, NEVER RE-ROOTED (#801).
//
// # What this is protecting
//
// The OMS cancel path carries two things in its context and nothing else knows
// they are there: the DELIVERY BUDGET, one deadline derived from the broker's
// AckWait that bounds a whole command rather than one lock acquisition, and the
// CLAIM SCOPE, the record of which per-order locks this delivery already holds.
//
// Both are invisible at a call site. A context that is re-rooted — or an
// awaitClaim whose returned context is discarded — compiles, passes every unit
// test, and silently restores the two defects the budget removed:
//
//   - the fan-out is bounded per acquisition again, so a cancel of a scheduled
//     parent with N children costs N × claimWait plus N venue round-trips and
//     runs past AckWait. Past AckWait the broker redelivers the cancel onto a
//     sibling pod, which re-dispatches closeAtVenue for every child — a call the
//     OMS's own comment says is "not idempotent from the exchange's point of
//     view". Duplicate withdrawals at a live exchange, on the EXIT path.
//
//   - the claim scope is empty, so a delivery can take a lock it already holds.
//     The per-order lock is a 1-capacity channel and is not re-entrant, so that
//     is the cancel subject's dispatch goroutine blocking on itself until the
//     budget expires.
//
// # Why a guard rather than only the tests
//
// services/oms/internal/order/claimscope_test.go proves the behaviour for the
// call sites that exist TODAY. What it cannot notice is a NEW one — a handler
// added later that starts its own context, or a caller that takes awaitClaim's
// release and throws its context away. Neither shows up as a failure anywhere:
// the budget simply stops applying to that path.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. Everything below
// is matched over PARSED EXPRESSIONS with comments not attached, so the
// paragraphs you are reading — which name context.Background and awaitClaim
// repeatedly — cannot satisfy or defeat it.

// orderPkgDir is the OMS order package: the aggregate, the handlers, the
// per-order lock table and the claim scope.
const orderPkgDir = "services/oms/internal/order"

// rerootingCalls are the ways to build a context that is not derived from the
// delivery's. Each drops the deadline and the claim scope together.
var rerootingCalls = map[string]bool{
	"Background": true,
	"TODO":       true,
}

// TestTheCancelDeliveryContextIsNeverReRooted is the default-deny arm: no
// non-test file in the order package may start a context of its own.
//
// THERE ARE NO EXEMPTIONS, AND THAT IS A MEASURED FACT rather than an
// aspiration: at the time this guard was written the package contained zero
// context.Background and zero context.TODO calls outside tests. A path that
// genuinely needs its own root — a background sweeper owning its own lifetime —
// belongs in the composition root, which is where every other one in this
// service already lives.
func TestTheCancelDeliveryContextIsNeverReRooted(t *testing.T) {
	for _, path := range orderPackageSources(t) {
		fset := token.NewFileSet()
		// Mode 0: comments are not attached, so this cannot match its own prose.
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !rerootingCalls[sel.Sel.Name] {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "context" {
				return true
			}
			t.Errorf("%s:%d calls context.%s — a context that is not derived from the "+
				"delivery's carries neither the delivery budget nor the claim scope, so every "+
				"per-order lock taken below it is bounded per acquisition again and a cancel of "+
				"a scheduled parent runs past the broker's AckWait. Thread the context the "+
				"handler was given",
				filepath.Base(path), fset.Position(sel.Pos()).Line, sel.Sel.Name)
			return true
		})
	}
}

// TestEveryAwaitClaimKeepsTheContextItReturns is the other half, and it is the
// one a reasonable change is most likely to trip.
//
// awaitClaim returns (context.Context, func(), error). The context is the ONLY
// record that this delivery now holds that order's lock. Writing
//
//	_, release, err := s.awaitClaim(ctx, id)
//
// is legal Go, reads like ordinary "I do not need that value", and empties the
// claim scope for everything the handler does while holding the lock — including
// the nested child cancel, which is the exact path the scope exists for.
//
// So the first result must be bound to ctx: the same name the handler already
// uses, shadowing it, so that everything below the claim necessarily uses the
// scoped one.
func TestEveryAwaitClaimKeepsTheContextItReturns(t *testing.T) {
	var checked int
	for _, path := range orderPackageSources(t) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Rhs) != 1 {
				return true
			}
			call, ok := assign.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "awaitClaim" {
				return true
			}
			checked++
			line := fset.Position(sel.Pos()).Line
			if len(assign.Lhs) != 3 {
				t.Errorf("%s:%d assigns %d values from awaitClaim, want 3 — its first result is "+
					"the context carrying this delivery's claim scope",
					filepath.Base(path), line, len(assign.Lhs))
				return true
			}
			got, ok := assign.Lhs[0].(*ast.Ident)
			if !ok || got.Name != "ctx" {
				name := "a non-identifier"
				if ok {
					name = got.Name
				}
				t.Errorf("%s:%d binds awaitClaim's context to %q, want ctx — anything else "+
					"leaves the unscoped context in scope below the claim, so the nested child "+
					"cancel is indistinguishable from a fresh delivery: no budget, no "+
					"re-entrancy check, no depth bound",
					filepath.Base(path), line, name)
			}
			return true
		})
	}

	// A GUARD THAT FOUND NOTHING TO CHECK IS NOT A PASSING GUARD. If awaitClaim
	// is renamed or its call sites move out of this package, the loop above goes
	// quiet and this test would report success while asserting nothing at all.
	if checked == 0 {
		t.Fatal("no awaitClaim call site was found in " + orderPkgDir + " — this guard is " +
			"asserting nothing. If the function was renamed, rename it here too")
	}
}

// orderPackageSources lists the non-test .go files of the order package, sorted
// so failures are reported in a stable order.
func orderPackageSources(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(moduleRoot(t), filepath.FromSlash(orderPkgDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", orderPkgDir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	if len(out) == 0 {
		t.Fatalf("no source files under %s — this guard reads a directory that has moved",
			orderPkgDir)
	}
	sort.Strings(out)
	return out
}

package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// EVERY DELIVERY THE BUS DISPATCHES IS BOUNDED BY ITS OWN AckWait (#836).
//
// # What ran unbounded
//
// Nothing in Subscribe put a deadline on the context it handed a handler. The
// only thing keeping a handler inside its consumer's AckWait was the handler's
// own arithmetic — written per handler, where it was written at all, and in most
// services not at all.
//
// Past AckWait the broker redelivers a message whose first copy is STILL
// RUNNING. On a work subject that is a second concurrent dispatch of the same
// command: on this platform, a second trade. pkg/bus/tuning.go states the rule
// the whole file is sized by —
//
//	MaxAckPending × worst-case per-message handling  <  AckWait
//
// — and until #836 nothing enforced the "worst-case per-message handling" half.
// It was an assertion about code the bus does not own.
//
// #801 is what that cost: the OMS cancel of a scheduled parent took one
// per-order claim and one venue round-trip per child, all on one delivery,
// against a 60s AckWait. It was fixed with a budget local to that one handler,
// which is how the estate ends up with twenty copies of one number and nineteen
// of them wrong — the `secret()` pattern AGENTS.md names.
//
// # What this checks
//
// Every jetstream Consume callback in pkg/bus — the exact points where a message
// becomes a handler invocation — must arm the budget before dispatching. There
// are two (the queue-group path and the ephemeral broadcast path) and the set is
// DERIVED from the AST rather than listed here, so a third subscription kind
// added later is covered without anyone remembering to open this file.
//
// It checks that the budget is ARMED, not that the number is right; the number
// is pkg/bus's own unit tests plus the real-broker integration test
// (pkg/bus/handler_budget_integration_test.go), which drives a 2s AckWait and
// asserts the handler is cancelled at 1.5s — before the server re-offers the
// message, with no two copies ever in flight.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. Every paragraph
// above names the budget; the check below runs over parsed call expressions
// inside a function literal's BODY, with comments not attached, so this text
// cannot satisfy it.
const (
	// natsFile holds every JetStream subscription this estate makes.
	natsFile = "pkg/bus/nats.go"
	// consumeCall is the jetstream method that turns messages into handler calls.
	consumeCall = "Consume"
	// budgetCall is what must be armed inside each of those callbacks.
	budgetCall = "withHandlerBudget"
)

// deliveryBudgetExempt names a Consume callback permitted to dispatch unbounded,
// and the issue that retires the entry.
//
// EMPTY, AND THAT IS THE POINT. An entry here is a decision that some class of
// message may be handled by a goroutine still running when the broker has already
// given up on it and handed the same message to somebody else. There is no
// subscription kind for which that is safe: the tick class loses nothing by being
// cut off, and the work and control classes lose correctness by not being.
var deliveryBudgetExempt = map[int]string{}

func TestEveryBusDeliveryIsBoundedByItsAckWait(t *testing.T) {
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(natsFile))
	fset := token.NewFileSet()
	// Mode 0: comments are not attached, so this cannot match its own prose.
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", natsFile, err)
	}

	found := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != consumeCall || len(call.Args) == 0 {
			return true
		}
		cb, ok := call.Args[0].(*ast.FuncLit)
		if !ok || cb.Body == nil {
			return true
		}
		line := fset.Position(call.Pos()).Line
		found++
		if _, ok := deliveryBudgetExempt[line]; ok {
			return true
		}
		if !callsPlainFunc(cb.Body, budgetCall) {
			t.Errorf("%s:%d dispatches a message to a handler without calling %s.\n"+
				"A handler that runs past its consumer's AckWait is not slow — the broker "+
				"redelivers the message while that copy is still working, which is a second "+
				"concurrent dispatch of one event, and on a work subject that is a second "+
				"trade. Arm the budget from this consumer's own AckWait before dispatching, "+
				"or add line %d to deliveryBudgetExempt with the argument for why this class "+
				"of message may be handled past the point the broker gave up on it",
				natsFile, line, budgetCall, line)
		}
		return true
	})

	// A GUARD THAT FOUND NO DISPATCH POINTS IS NOT A PASSING GUARD. If the
	// subscription machinery moves out of this file, or the jetstream API is
	// renamed, the walk above goes quiet and this would report success while
	// asserting nothing at all.
	if found < 2 {
		t.Fatalf("found %d %s callbacks in %s, want at least the queue-group and broadcast "+
			"paths — if the subscription machinery moved, point this guard at it rather than "+
			"leaving it green over an empty walk", found, consumeCall, natsFile)
	}

	// DEAD-ENTRY ARM: an exemption for a line that is no longer a dispatch point,
	// or one that has since armed the budget, is permission nobody needs and reads
	// as a decision somebody made.
	for line, reason := range deliveryBudgetExempt {
		if line > 0 && found == 0 {
			t.Errorf("exemption for %s:%d (%s) matches no dispatch point — delete it",
				natsFile, line, reason)
		}
	}
}

// callsFunc reports whether body calls a plain function named name.
func callsPlainFunc(body *ast.BlockStmt, name string) bool {
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

// THE BUDGET IS DERIVED FROM AckWait, NOT RESTATED (#836).
//
// The OMS needs the same number for its schedule ticker, which is not a bus
// delivery and therefore never gets a broker-supplied deadline. That is one
// formula with two entry points, which is fine — two formulas would not be, and
// two is what this stops: claimscope.go must reach the fraction through pkg/bus
// rather than spelling out an AckWait and a margin of its own.
func TestTheOMSBudgetReadsTheBusFraction(t *testing.T) {
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(claimScopeFile))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", claimScopeFile, err)
	}
	src := string(raw)
	if !strings.Contains(src, "bus.HandlerBudgetNumerator") ||
		!strings.Contains(src, "bus.HandlerBudgetDenominator") {
		t.Errorf("%s no longer derives its budget from pkg/bus's fraction. A second copy of "+
			"the AckWait arithmetic is exactly what #836 removed: raising an AckWait would "+
			"then leave a stale budget behind in another package, and the two would disagree "+
			"about when a delivery must stop", claimScopeFile)
	}
}

const claimScopeFile = "services/oms/internal/order/claimscope.go"

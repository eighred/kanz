package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// AN ADAPTER THAT WRAPS AN ORDER VIEW MUST EXPORT THE VIEW'S FAILURES (#1047).
//
// # What this is protecting
//
// orderview.Seam's read failure used to be indistinguishable from "this is not
// our order", so a Postgres blip inside a venue adapter deleted real executions
// from the order.order.filled stream — no FACT, so the position book never
// booked the position and the ledger never journalled the cash. The seam now
// returns the error and the ingesters refuse the report, but a refusal nothing
// counts is still an outage nobody can see: the only trace left is an ERROR log
// in one pod, and a log line is not a threshold.
//
// So a composition root may not build a BARE seam. It builds an observed one,
// and it registers the counters that observation produces. Each of those is a
// separate way to arrive at the same silence:
//
//   - a bare orderview.NewSeam: the root writes its own onErr, both roots wrote
//     the same wrong sentence ("reconciliation is degraded", which names the
//     watchdog and not the dropped fills), and neither counted anything.
//   - built and not registered: the collector exists in the process and is
//     scraped by nothing.
//   - registered and never incremented: a series pinned at zero forever, which
//     an `== 0` alert reads as health. This estate has shipped that twice
//     (#963, #973), caught both times only by running the binary — which is why
//     the increment lives inside NewObservedSeam where a test can reach it,
//     rather than in a root where nothing can.
//
// # Why a guard and not only the behavioural tests
//
// The tests in internal/venueadapter/orderview pin what the seam and the
// counters DO. What they cannot see is a THIRD venue adapter: composition roots
// are this estate's known blind spot, because cmd/*/main.go wiring escapes every
// unit test. There are two roots today and they were identical in the defect.
//
// # Why it reads the AST with comments detached
//
// Guards in this tree have passed while asserting nothing because a regex over
// raw source matched their own explanatory prose. This paragraph names NewSeam,
// NewObservedSeam, NewObservability and MustRegister, and satisfies nothing
// below.
const orderViewObsScope = "services"

const (
	bareSeamCtor     = "NewSeam"
	observedSeamCtor = "NewObservedSeam"
	obsCtor          = "NewObservability"
	obsCollectors    = "Collectors"
	registerFn       = "MustRegister"
)

// bareSeamExempt names a file under services/ that may call orderview.NewSeam
// directly, and why.
//
// DEFAULT-DENY, and it is EMPTY. A bare seam means a root supplying its own
// failure handler, which is the thing this guard exists to stop: an unobserved
// order view is an adapter that can go blind and drop executions with nothing
// but a log line to say so.
var bareSeamExempt = map[string]string{}

func TestEveryOrderViewSeamExportsItsFailures(t *testing.T) {
	root := moduleRoot(t)

	roots := 0
	for _, gf := range goFilesUnder(t, filepath.Join(root, filepath.FromSlash(orderViewObsScope))) {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		rel := orderViewObsScope + "/" + gf.rel
		fset := token.NewFileSet()
		// Mode 0: comments are not attached, so no prose can be mistaken for a call.
		f, err := parser.ParseFile(fset, rel, gf.body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		// 1. NO BARE SEAM.
		if findPkgCall(f, "orderview", bareSeamCtor) != nil {
			if reason, ok := bareSeamExempt[rel]; ok {
				t.Logf("%s: exempt from the observed-seam rule — %s", rel, reason)
			} else {
				t.Errorf("%s calls orderview.%s directly — that leaves the order view's failures to "+
					"a handler this root writes and no test can reach, which is how both venue "+
					"adapters ended up logging \"reconciliation is degraded\" for an outage that was "+
					"dropping fills. Use orderview.%s", rel, bareSeamCtor, observedSeamCtor)
			}
		}

		seamCall := findPkgCall(f, "orderview", observedSeamCtor)
		if seamCall == nil {
			continue
		}
		roots++

		// 2. THE COUNTERS IT IS GIVEN COME FROM THE SHARED, SEEDED CONSTRUCTOR.
		//    A zero-value Observability satisfies the compiler and exports nothing.
		obsIdent := assignedFromPkgCall(f, "orderview", obsCtor)
		if obsIdent == "" {
			t.Errorf("%s builds an observed seam but never calls orderview.%s — the counters it is "+
				"handed are whatever the caller invented, and a zero-value Observability compiles "+
				"and exports no series at all", rel, obsCtor)
			continue
		}
		if !callPasses(seamCall, obsIdent) {
			t.Errorf("%s calls orderview.%s but does not pass %q to it — the seeded counters and "+
				"the seam that increments them are two different objects",
				rel, observedSeamCtor, obsIdent)
		}

		// 3. THEY ARE REGISTERED. A collector nothing scrapes does not exist.
		if !reachesCall(f, registerFn, func(arg ast.Expr) bool {
			return callsMethodOn(arg, obsIdent, obsCollectors)
		}) {
			t.Errorf("%s builds orderview.%s as %q but never passes %s.%s() to %s — the counters "+
				"exist in the process and are exported to nobody",
				rel, obsCtor, obsIdent, obsIdent, obsCollectors, registerFn)
		}
	}

	// NON-VACUITY: both venue adapters, or the scan has lost the files it is
	// about and every arm above is checking nothing.
	if roots < 2 {
		t.Fatalf("found %d composition root(s) calling orderview.%s under %s/ — there are two "+
			"venue adapters, so a scan seeing fewer than that is asserting nothing",
			roots, observedSeamCtor, orderViewObsScope)
	}
}

// findPkgCall returns the first call to pkg.name in f.
func findPkgCall(f *ast.File, pkg, name string) *ast.CallExpr {
	var found *ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isPkgSelector(call.Fun, pkg, name) {
			found = call
		}
		return true
	})
	return found
}

// assignedFromPkgCall returns the identifier a pkg.name call result was bound to.
func assignedFromPkgCall(f *ast.File, pkg, name string) string {
	out := ""
	ast.Inspect(f, func(n ast.Node) bool {
		if out != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || !isPkgSelector(call.Fun, pkg, name) {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			out = id.Name
		}
		return true
	})
	return out
}

// callPasses reports whether ident is one of call's arguments.
func callPasses(call *ast.CallExpr, ident string) bool {
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == ident {
			return true
		}
	}
	return false
}

// reachesCall reports whether some call to a method named fn has an argument
// satisfying want.
func reachesCall(f *ast.File, fn string, want func(ast.Expr) bool) bool {
	hit := false
	ast.Inspect(f, func(n ast.Node) bool {
		if hit {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != fn {
			return true
		}
		for _, a := range call.Args {
			if want(a) {
				hit = true
			}
		}
		return true
	})
	return hit
}

// callsMethodOn reports whether e is `recv.method()`. A `...` spread wraps the
// call rather than the other way round, so the CallExpr is still e.
func callsMethodOn(e ast.Expr, recv, method string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	return isPkgSelector(call.Fun, recv, method)
}

func isPkgSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

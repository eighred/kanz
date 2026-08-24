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

// AN ADMISSION AGAINST AN UNVOUCHED BALANCE IS OBSERVED BY THE RUNNING OMS (#671).
//
// # Why this is an arch guard and not a unit test
//
// The gate's own cases prove that WithUnaccountedObserver fires. They cannot
// prove the OMS PASSES one. #614 established exactly how invisible that gap is:
// removing the completeness posture from accounting's composition root left BOTH
// service suites and all of internal/ green, with only an arch guard failing.
//
// This estate has shipped two crashes through the same blind spot — cmd/*/main.go
// wiring escapes every unit test, because no unit test constructs it. So the
// claim "an order admitted against a balance nobody vouched for is counted" is
// only true if the composition root wires the observer, and that is checked here,
// off the AST, on every run.
//
// # What it does NOT claim
//
// It checks that an observer is PASSED, not what the callback does with it. A
// deployment could wire an empty function. That is the weaker property, and it is
// worth having anyway: the wiring is what a reviewer forgets, and a metric that
// was never registered cannot be noticed missing on a dashboard nobody built yet.
func TestOMSCompositionRootObservesUnaccountedAdmissions(t *testing.T) {
	const (
		constructor = "NewPreTradeGate"
		observer    = "WithUnaccountedObserver"
	)
	// THE PACKAGE, NOT main.go. #643 moved the gate's construction into
	// pretrade.go — a named builder, so the seams it wires can be asserted by a
	// real test rather than only read as text — and a guard pinned to one FILE
	// reported "the gate is not constructed" for a gate that had simply moved
	// twelve inches. The property is about the composition ROOT, which is a
	// package.
	dir := filepath.Join(moduleRoot(t), "services", "oms", "cmd", "oms")
	files := parsePackageFiles(t, dir)

	var constructed, observed bool
	inspect := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != constructor {
			return true
		}
		constructed = true
		// The option is an ARGUMENT to the constructor, so a WithUnaccountedObserver
		// built and then dropped on the floor does not satisfy this.
		for _, arg := range call.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			isel, ok := inner.Fun.(*ast.SelectorExpr)
			if ok && isel.Sel != nil && isel.Sel.Name == observer {
				observed = true
			}
		}
		return true
	}
	for _, f := range files {
		ast.Inspect(f, inspect)
	}

	// NON-VACUITY. A renamed or relocated constructor would satisfy the assertion
	// below by never reaching it — the failure mode every "does X call Y" guard
	// has, and the reason this one says so out loud.
	if !constructed {
		t.Fatalf("nothing in services/oms/cmd/oms calls comp.%s — this guard found nothing to "+
			"check. If the gate moved, move this assertion with it rather than deleting it.",
			constructor)
	}
	if !observed {
		t.Fatalf("comp.%s is constructed in the OMS composition root without comp.%s.\n\n"+
			"Every order ADMITTED against a cash balance whose producer did not vouch for it is then "+
			"silent again: no counter, no log, nothing separating a pass against a whole balance from "+
			"a pass against one missing every dividend and coupon this platform has never ingested "+
			"(#588). A REFUSAL still names them — attributeCash puts balance_omits on the violation "+
			"(#614) — so the dangerous direction is the quiet one: foldCorpAct pays quantity x "+
			"per-unit with the SIGN of the holding, so an unfolded entitlement OVERSTATES a SHORT "+
			"book's cash and buying power can admit an order the fund cannot pay for.\n\n"+
			"Nothing else in the toolchain notices. Both service suites and all of internal/ stay "+
			"green with this wiring removed (#614 proved that on the producing side).",
			constructor, observer)
	}
}

// parsePackageFiles parses every non-test Go file in one directory.
//
// A DIRECTORY WALK RATHER THAN parser.ParseDir, which is deprecated as of Go
// 1.25 and which golangci-lint refuses — and which this guard does not need
// anyway: it asks a question about the files in one composition root, not about
// build-tag-resolved package membership.
func parsePackageFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, f)
	}
	// NON-VACUITY, one level down from the caller's own: an empty directory would
	// let every "does the root call X" assertion pass by never reaching it.
	if len(out) == 0 {
		t.Fatalf("no non-test Go files under %s — this guard is parsing nothing", dir)
	}
	return out
}

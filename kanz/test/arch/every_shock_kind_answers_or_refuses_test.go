package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY SHOCK KIND MUST MOVE THE BOOK OR SAY IT COULD NOT (#1035).
//
// # The sibling guard, and the gap between them
//
// every_shock_kind_reaches_the_wire_test.go proves a shock kind is ASKABLE: an
// api/v1 implementor, a query.v1 oneof member, a decode case, an apply case. It
// passed with VolShock fully wired end to end, and the apply case it checked was
// this:
//
//	case v1.VolShock:
//	    // A vol shock has no linear MarketValue effect — it only reprices
//	    // options under full revaluation. No-op here so it is a recognized
//	    // (not dropped) shock on the linear path.
//
// Recognized, and inert. #1004 had just put VolShock on the wire so a desk could
// finally request a vol stress; the reachable engine path is the linear
// scenario.Evaluate; the one path that prices a vol bump (EvaluateReval) is
// blocked on a calibrated vol surface nothing on this estate carries. So a +15
// vol-point stress on a short-vega book cloned the portfolio, mutated nothing,
// and answered 200 with contributed=0, excluded_count=0 — a CLEAN COVERAGE
// RECORD, which is affirmative evidence that a stress ran.
//
// That is worse than #640, where a scenario returned the unshocked book and the
// coverage said so once the record existed. Here the number was not merely
// unresolvable, it was confidently unchanged, and nothing on the response
// distinguished "vol stress applied, this book is convexity-neutral" from "vol
// stress is not implemented on this path". A fund that believes it has stressed
// +15 vol points and has not will size positions on that belief.
//
// # What this guard requires, and why each leg is derived
//
// Three properties, all read out of the source rather than listed here:
//
//  1. EVERY ARM DOES SOMETHING. For each v1.ScenarioShock implementor, the
//     matching case in scenario.applyShock must reference either the portfolio
//     parameter or the coverage parameter — mutate the book, or record that it
//     could not. An empty arm, or one that touches neither, is a shock the
//     caller can ask for whose absence the response cannot express. The two
//     parameter NAMES are taken from applyShock's own signature (by type,
//     *domain.Portfolio and *shockCoverage) so a rename does not silently make
//     this vacuous.
//
//  2. EVERY EXCLUSION REASON IS ACTUALLY RECORDED. Each Skip* constant declared
//     in scenario.go must appear as an argument to a Coverage Exclude /
//     ExcludeWhole call in that file. A reason constant nobody passes to the
//     coverage is a vocabulary entry that reads like a control and is not one —
//     the same shape as the arm above, one layer in. The constant set is read
//     from the const block, never listed, because "somebody enumerated a set by
//     hand and missed a member" is the defect class this whole file is about.
//
//  3. NO CONDITION ON THE REFUSAL PATH CARRIES A BOOLEAN CONSTANT. `if false &&
//     cov.ExcludedCount > 0` switches the refusal off while every symbol legs 1
//     and 2 read is still present, and both would still pass (#771, #957). The
//     path is two places — all of scenario.go, which decides WHETHER a shock
//     landed, and engine.EvaluateScenario, which decides whether to answer —
//     and a literal in either is what makes the recorded exclusion inert.
//     Nothing on either legitimately branches on a literal.
//
// # Why the AST and not a grep
//
// A guard that greps raw source matches its own prose and the nearby field
// names, and this file's comments name every symbol it checks. Everything below
// parses with comments discarded, so a shock type named only in a comment or an
// error string cannot satisfy any leg.
//
// # Known weakness, stated rather than discovered later
//
// Leg 1 proves an arm REACHES the portfolio or the coverage, not that what it
// does there is right: `applyFoo(p)` with an empty body would pass. That is the
// same limit the sibling guard has one level out, and it is why the unit tests
// in internal/risk/scenario assert on the shocked values and on the exclusion
// reason rather than on the call.
func TestEveryShockKindAnswersOrRefuses(t *testing.T) {
	root := moduleRoot(t)
	scenarioGo := filepath.Join(root, "internal", "risk", "scenario", "scenario.go")

	implementors := scenarioShockImplementors(t, root)
	// NON-VACUITY (1/5): a scanner that finds no implementors passes every
	// assertion below while proving nothing. Four exist today.
	if len(implementors) < 4 {
		t.Fatalf("found %d v1.ScenarioShock implementors in internal/risk/api/v1 (%v), want at "+
			"least 4 — the extractor is broken or the types moved, and this guard is comparing "+
			"against nothing", len(implementors), implementors)
	}

	fset := token.NewFileSet()
	// Mode 0: comments are not attached to the tree, so no leg below can be
	// satisfied by prose.
	file, err := parser.ParseFile(fset, scenarioGo, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", scenarioGo, err)
	}

	// ===== Leg 1: every arm moves the book or records on the coverage =====

	fn := funcDeclNamed(file, "applyShock")
	// NON-VACUITY (2/5): a renamed or moved applyShock must fail loudly rather
	// than report every shock kind as fine.
	if fn == nil {
		t.Fatalf("no func applyShock in %s — the shock dispatch moved or was renamed, and this "+
			"guard reads nothing", scenarioGo)
	}
	book := paramNameOfType(fn, "*domain.Portfolio")
	cov := paramNameOfType(fn, "*shockCoverage")
	// NON-VACUITY (3/5): both parameter names are derived from the signature. If
	// either type is gone the "did the arm touch it" question below is
	// unanswerable, and an empty name would match no identifier and fail every
	// arm for the wrong reason.
	if book == "" || cov == "" {
		t.Fatalf("applyShock has no *domain.Portfolio and/or no *shockCoverage parameter "+
			"(portfolio=%q coverage=%q) — its signature changed and this guard can no longer "+
			"tell an arm that acts from one that does not", book, cov)
	}

	arms := shockArmIdents(fn)
	// NON-VACUITY (4/5): the type switch must have been found.
	if len(arms) == 0 {
		t.Fatalf("applyShock contains no type switch over v1 shock types — the dispatch was " +
			"restructured, and this guard is not reading the dispatch it names")
	}

	for _, name := range implementors {
		if reason, ok := inertShockExemptions[name]; ok {
			t.Logf("exempt: %s — %s", name, reason)
			continue
		}
		idents, ok := arms[name]
		if !ok {
			// The sibling guard owns the "there is no case at all" message; this
			// one restates it only so a missing arm does not read as a passing arm.
			t.Errorf("scenario.applyShock has no case for v1.%s — see "+
				"every_shock_kind_reaches_the_wire_test.go, which owns that requirement", name)
			continue
		}
		if !idents[book] && !idents[cov] {
			t.Errorf("scenario.applyShock's v1.%s arm touches neither %s (the portfolio) nor %s "+
				"(the coverage).\n"+
				"An arm that does neither is a shock a caller can ASK FOR and the response cannot "+
				"distinguish from one that applied and moved nothing: the request succeeds, the "+
				"projection comes back, the coverage is clean, and the book is the book. That is "+
				"how a +15 vol-point stress was answered 200 with contributed=0, excluded_count=0 "+
				"for as long as VolShock was on the wire (#1035, and #640 before it).\n"+
				"Either apply the shock to %s, or record why it could not on %s — a whole-"+
				"evaluation ExcludeWhole for a missing seam, a per-instrument Exclude for a "+
				"holding. The engine refuses on a non-empty coverage "+
				"(v1.ErrScenarioUnresolvable), so recording it is what turns a silent no-op into "+
				"an answer the caller can act on.", name, book, cov, book, cov)
		}
	}

	// DEAD-ENTRY ARM: an exemption naming a type that no longer implements
	// v1.ScenarioShock outlives its repair and silently widens the next one.
	declared := map[string]bool{}
	for _, name := range implementors {
		declared[name] = true
	}
	for name, reason := range inertShockExemptions {
		if !declared[name] {
			t.Errorf("exemption for %s (%s) names no v1.ScenarioShock implementor — remove it; "+
				"a stale exemption is a hole waiting for a type of the same name", name, reason)
		}
	}

	// ===== Leg 2: every Skip* reason constant is actually recorded =====

	reasons := skipConstantNames(file)
	// NON-VACUITY (5/5): four reasons exist today. Zero would make the loop below
	// pass over nothing.
	if len(reasons) < 4 {
		t.Fatalf("found %d Skip* exclusion-reason constants in %s (%v), want at least 4 — the "+
			"vocabulary moved and this guard is checking an empty set",
			len(reasons), scenarioGo, reasons)
	}
	recorded := coverageExcludeArgs(file)
	for _, name := range reasons {
		if !recorded[name] {
			t.Errorf("%s is declared as an exclusion reason and is never passed to Exclude or "+
				"ExcludeWhole in %s.\n"+
				"A reason constant nobody records is a control that exists only in the "+
				"vocabulary: the case it names still returns a clean coverage, the engine still "+
				"answers, and the reason shows up in no refusal an operator will ever read.",
				name, scenarioGo)
		}
	}

	// ===== Leg 3: no condition on the refusal path branches on a literal =====

	engineGo := filepath.Join(root, "internal", "risk", "engine", "engine.go")
	engineFile, err := parser.ParseFile(fset, engineGo, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", engineGo, err)
	}
	refuse := funcDeclNamed(engineFile, "EvaluateScenario")
	// NON-VACUITY: the function that turns a non-empty coverage into a refusal
	// must be found, or leg 3 checks half of what it names.
	if refuse == nil {
		t.Fatalf("no func EvaluateScenario in %s — the refusal decision moved, and this guard "+
			"is reading only half the path it names", engineGo)
	}

	for _, scope := range []struct {
		what string
		node ast.Node
	}{
		{filepath.Base(scenarioGo), file},
		{"engine.EvaluateScenario", refuse},
	} {
		for _, where := range boolConstantConditions(fset, scope.node) {
			t.Errorf("%s: a condition in %s carries a boolean literal.\n"+
				"`if false && cov.ExcludedCount > 0` disables the refusal while every symbol legs "+
				"1 and 2 read is still present, so both keep passing, the exclusion is still "+
				"recorded, and the caller is still answered with the unshocked book (#771, #957). "+
				"Nothing on the shock-refusal path branches on a literal.", where, scope.what)
		}
	}
}

// inertShockExemptions maps an api/v1 shock type name to the issue that will
// retire its exemption. EMPTY, and it lands empty: the one arm that did nothing
// was v1.VolShock, and #1035 gave it an exclusion instead of an exemption. The
// first entry anyone needs to add is a conversation about why a shock a caller
// can request must answer with a record indistinguishable from "no effect", not
// a formality.
var inertShockExemptions = map[string]string{}

// funcDeclNamed returns the top-level function declaration with the given name,
// or nil.
func funcDeclNamed(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
			return fn
		}
	}
	return nil
}

// paramNameOfType returns the name of fn's first parameter whose type renders as
// want (e.g. "*domain.Portfolio"). Reading the TYPE rather than assuming the
// name is what keeps this guard honest across a rename: a parameter called
// something else still has to be the portfolio to satisfy the arm check.
func paramNameOfType(fn *ast.FuncDecl, want string) string {
	if fn.Type.Params == nil {
		return ""
	}
	for _, field := range fn.Type.Params.List {
		if types.ExprString(field.Type) != want {
			continue
		}
		for _, n := range field.Names {
			if n.Name != "_" {
				return n.Name
			}
		}
	}
	return ""
}

// shockArmIdents maps each v1 shock type named by a case in fn's type switch to
// the set of identifiers its arm uses. An arm with an empty body yields an empty
// set, which is the case the guard is looking for.
func shockArmIdents(fn *ast.FuncDecl) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.TypeSwitchStmt)
		if !ok || sw.Body == nil {
			return true
		}
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			idents := map[string]bool{}
			for _, s := range clause.Body {
				ast.Inspect(s, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok {
						idents[id.Name] = true
					}
					return true
				})
			}
			for _, expr := range clause.List {
				if name := apiV1TypeName(expr); name != "" {
					out[name] = idents
				}
			}
		}
		return true
	})
	return out
}

// skipConstantNames returns the exported Skip* constant names declared in the
// file — the closed exclusion-reason vocabulary, read from the declaration
// rather than copied into this guard.
func skipConstantNames(file *ast.File) []string {
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, n := range vs.Names {
				if strings.HasPrefix(n.Name, "Skip") {
					out = append(out, n.Name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// coverageExcludeArgs returns the identifier names passed to any Exclude or
// ExcludeWhole call in the file — the coverage's own record of which reasons it
// can emit.
func coverageExcludeArgs(file *ast.File) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Exclude" && sel.Sel.Name != "ExcludeWhole") {
			return true
		}
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// boolConstantConditions returns a position string for every boolean literal
// appearing in a branch condition or as an operand of &&, || or !.
//
// It reads the CONDITION positions specifically rather than banning the
// identifiers outright, because `return true` from an ast.Inspect visitor and a
// bool-returning helper are both ordinary and neither disables anything.
func boolConstantConditions(fset *token.FileSet, root ast.Node) []string {
	// A set, because the same condition is visited twice — once as IfStmt.Cond
	// and again as the && / || BinaryExpr inside it — and one literal is one
	// finding.
	seen := map[string]bool{}
	note := func(expr ast.Expr) {
		if expr == nil {
			return
		}
		ast.Inspect(expr, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || (id.Name != "true" && id.Name != "false") {
				return true
			}
			seen[fset.Position(id.Pos()).String()] = true
			return true
		})
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			note(node.Cond)
		case *ast.ForStmt:
			note(node.Cond)
		case *ast.SwitchStmt:
			note(node.Tag)
		case *ast.BinaryExpr:
			if node.Op == token.LAND || node.Op == token.LOR {
				note(node.X)
				note(node.Y)
			}
		case *ast.UnaryExpr:
			if node.Op == token.NOT {
				note(node.X)
			}
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for pos := range seen {
		out = append(out, pos)
	}
	sort.Strings(out)
	return out
}

package arch

import (
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THREE HAND-MAINTAINED LISTS IN ONE PACKAGE, PAIRWISE UNCHECKED (#471).
//
// internal/risk/benchmarks holds the evidence that an analytic was graded against
// a known-good result, and it holds it in three separate lists that nothing keeps
// in step:
//
//	the Analytic* constants   the names
//	Reports()'s `sets`        name -> case set, the thing that produces evidence
//	Inventory()               the names the posture gauge iterates
//
// Each is edited by hand, and an omission from any one of them is silent in a
// different way.
//
// # What this does NOT do, stated first so the name cannot overstate it
//
// It does not check that every implemented analytic is inventoried, which is the
// gap #471 actually names and measure_catalogue_test.go points at. THAT GAP IS
// NOT CLOSEABLE THE WAY THE MEASURE ONE WAS, and Inventory()'s own doc reached
// the right conclusion before this guard existed: "a package walk would count
// helpers and constructors, and a naming convention would be a second contract to
// keep."
//
// Measures were derivable because every one is a MeasureName constant in one
// tree. Analytics are not: the twelve are implemented across EIGHT packages in
// FOUR top-level trees — internal/risk/pricing, .../volsurface, .../compute/var,
// internal/risk/xva, internal/regulatory, .../frtb, .../stress, and
// internal/collateral — with no shared interface, no registry, no marker and no
// naming convention. BlackScholesPrice, SIMM, Charge, DRC, Expand and Historical
// have nothing in common that a helper in the same file lacks.
//
// A marker comment or a side-effect registration would make it total and would be
// exactly the second contract that doc rejects: a marker somebody forgets is the
// same failure as an inventory line somebody forgets, with two places to forget
// instead of one.
//
// # What it does do, and both of these are live holes today
//
// A CASE SET THAT REACHES NO GATE. An exported `func F() []validation.Case` that
// is written but never added to Reports()'s `sets` never reaches gate.Record, so
// the analytic reads `absent` on kanz_risk_analytics_validated while a complete,
// passing benchmark sits in the tree. The evidence exists and the estate is told
// there is none.
//
// EVIDENCE FOR AN ANALYTIC NOBODY COUNTS. An analytic in `sets` but not in
// Inventory() is the sharper one: gate.Record succeeds, signed evidence is held
// in memory, and the gauge still emits NOTHING — analyticsPosture.Collect ranges
// over the inventory and nothing else, and validation.Gate cannot be enumerated
// to reconcile from the other side.
//
// # Why an omission here flatters rather than alarms
//
// An analytic missing from Inventory() produces no series AT ALL — not `absent`,
// not zero, no label value. So a coverage ratio computed off this metric goes UP
// when an unvalidated analytic is dropped, because the denominator shrinks. The
// startup Warn reads the same slice, so the log under-reports identically. Both
// halves are blind to the same omission, in the flattering direction.
func TestTheAnalyticsInventoryAgreesWithItsBenchmarks(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	consts := map[string]string{}   // Analytic* ident -> string value
	caseSets := map[string]string{} // exported func returning []validation.Case -> file
	var inventory []analyticEntry   // Inventory()'s elements, in order
	var reported []analyticEntry    // the analytic of each Reports() set
	var reportedCalls []string      // the case-set function each set calls
	var exempt []string             // validationExemptions' keys
	sawInventory, sawReports, sawExemptions := false, false, false

	walkGoFiles(t, root, "internal/risk/benchmarks", fset, func(rel string, f *ast.File) {
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.GenDecl:
				if decl.Tok == token.VAR {
					keys, found := exemptionKeys(decl)
					exempt = append(exempt, keys...)
					sawExemptions = sawExemptions || found
					continue
				}
				if decl.Tok != token.CONST {
					continue
				}
				for _, spec := range decl.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, "Analytic") || i >= len(vs.Values) {
							continue
						}
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							v, err := strconv.Unquote(lit.Value)
							if err == nil {
								consts[name.Name] = v
							}
						}
					}
				}
			case *ast.FuncDecl:
				if decl.Recv != nil || !decl.Name.IsExported() {
					continue
				}
				if returnsValidationCases(decl) {
					caseSets[decl.Name.Name] = rel
				}
				switch decl.Name.Name {
				case "Inventory":
					sawInventory = true
					inventory = inventoryElements(decl)
				case "Reports":
					sawReports = true
					reported, reportedCalls = reportsSets(decl)
				}
			}
		}
	})

	// NON-VACUITY, FOUR WAYS. Each of these is a path or a shape that could move
	// and leave the comparisons below true of nothing — which is the failure mode
	// one level up from the one this guard exists for.
	if !sawInventory {
		t.Fatal("Inventory() was not found under internal/risk/benchmarks — the path or the " +
			"function moved and this guard is comparing against an empty list")
	}
	if !sawReports {
		t.Fatal("Reports() was not found under internal/risk/benchmarks — same problem")
	}
	if len(consts) < 10 {
		t.Fatalf("found %d Analytic* constants, want at least 10 — the const walk is broken, "+
			"not the estate", len(consts))
	}
	if len(caseSets) < 10 {
		t.Fatalf("found %d exported case-set functions, want at least 10 — the "+
			"`func F() []validation.Case` shape moved and a new one would sail past", len(caseSets))
	}
	// DELIBERATELY WELL BELOW TODAY'S TWELVE. This floor's job is to catch a
	// literal that was not read at all, not to pin the count — and pinning it
	// masked the assertion below: dropping one entry reported "want at least 12"
	// instead of naming the constant that had gone missing, which is a guard
	// answering a different question from the one it was asked. Found by
	// mutation, not by reading.
	if len(inventory) < 8 {
		t.Fatalf("Inventory() has %d entries, want at least 8 — the literal was not read", len(inventory))
	}

	// NON-VACUITY, THE FIFTH WAY, and the one that matters most now that the gate
	// is armed. If this declaration is renamed, `licensed` is empty and arm 5
	// below refuses the whole estate rather than passing quietly — loud, but it
	// would blame the analytics for a broken parser. Say which it is.
	if !sawExemptions {
		t.Fatalf("the %s declaration was not found under internal/risk/benchmarks — it was renamed "+
			"or moved, and this guard is checking the exemption list against nothing while the "+
			"risk-engine composition root reads the real one at runtime", exemptionsVar)
	}
	licensed := map[string]bool{}
	for _, name := range exempt {
		licensed[name] = true
	}

	inInventory := map[string]bool{}     // by identifier (or the literal itself)
	inventoryValues := map[string]bool{} // by the analytic NAME the gate keys on
	for _, e := range inventory {
		inInventory[e.name] = true
		if v := e.value(consts); v != "" {
			inventoryValues[v] = true
		}
	}

	// 1. EVERY CONSTANT IS INVENTORIED. A named analytic the gauge never iterates
	//    is invisible: no series, and a coverage ratio that improves by its absence.
	var uncounted []string
	for name := range consts {
		if !inInventory[name] {
			uncounted = append(uncounted, name)
		}
	}
	sort.Strings(uncounted)
	for _, name := range uncounted {
		t.Errorf("%s is declared but is not in Inventory() — analyticsPosture.Collect ranges over "+
			"the inventory and nothing else, so this analytic emits NO series on "+
			"kanz_risk_analytics_validated. Not `absent`, not zero: absent from the metric "+
			"entirely, which makes coverage look better rather than worse.", name)
	}

	// 2. EVERY INVENTORY ENTRY IS A DECLARED CONSTANT, or a named exception. A
	//    bare string cannot be kept honest when the analytic is renamed — the same
	//    badKeys shape measure_catalogue_test.go reports, except here one of them
	//    is deliberate and argued.
	for _, e := range inventory {
		if e.isLiteral {
			if !licensed[e.name] {
				t.Errorf("Inventory() carries the bare string %q, which names no constant and is "+
					"not a declared exception. Nothing keeps it in step when the analytic is "+
					"renamed, and a stale entry reports an analytic that does not exist as "+
					"permanently unvalidated — a red series an operator learns to ignore.", e.name)
			}
			continue
		}
		if _, ok := consts[e.name]; !ok {
			t.Errorf("Inventory() names %s, which is not a declared Analytic* constant — it was "+
				"renamed or deleted and the inventory kept the old name", e.name)
		}
	}

	// 3. EVERY CASE SET REACHES THE GATE. This is a live hole: a benchmark written
	//    and not wired into Reports() produces no evidence, so the analytic reads
	//    `absent` while a complete passing case set sits in the tree.
	called := map[string]bool{}
	for _, c := range reportedCalls {
		called[c] = true
	}
	var unwired []string
	for fn := range caseSets {
		if !called[fn] {
			unwired = append(unwired, fn+" ("+caseSets[fn]+")")
		}
	}
	sort.Strings(unwired)
	for _, fn := range unwired {
		t.Errorf("%s returns []validation.Case but Reports() never calls it — the benchmark runs "+
			"nowhere, gate.Record never sees it, and the analytic it grades reports `absent` with "+
			"a complete passing case set sitting in this package", fn)
	}

	// 4. EVERY REPORTED ANALYTIC IS COUNTED. The sharpest instance: gate.Record
	//    succeeds, signed evidence is held, and the gauge emits nothing at all.
	for _, e := range reported {
		if !inInventory[e.name] {
			t.Errorf("Reports() files evidence for %s, which Inventory() does not list — the "+
				"validation is recorded and signed, and kanz_risk_analytics_validated emits no "+
				"series for it. Validated and invisible is the worst of the five states because "+
				"it is not one of them.", e.name)
		}
	}

	// 5. EVERY INVENTORIED ANALYTIC EITHER HAS EVIDENCE OR IS A NAMED EXEMPTION.
	//
	//    THIS IS THE ARM THAT MAKES ARMING THE GATE SAFE (#471 slice 3).
	//    RISK_REQUIRE_VALIDATED_ANALYTICS defaults to on, so an analytic added to
	//    Inventory() without a case set and without an exemption entry does not
	//    show up as one more zero on a gauge — it stops every risk-engine replica
	//    at startup, on the deploy, with the build that produced it long since
	//    green. This turns that into a test failure on the branch that wrote it.
	//
	//    Note what is NOT checked, because it cannot be from source: whether the
	//    case set PASSES. A benchmark that runs and fails still reaches the gate,
	//    reads `failed` rather than `absent`, and is refused by the armed gate
	//    just the same. Only running it answers that, which the benchmarks
	//    package's own tests do.
	hasEvidence := map[string]bool{}
	for _, e := range reported {
		hasEvidence[e.value(consts)] = true
	}
	var wouldRefuseTheStart []string
	for _, e := range inventory {
		v := e.value(consts)
		if v == "" || hasEvidence[v] {
			continue
		}
		if !licensed[v] {
			wouldRefuseTheStart = append(wouldRefuseTheStart, v)
		}
	}
	sort.Strings(wouldRefuseTheStart)
	for _, v := range wouldRefuseTheStart {
		t.Errorf("Inventory() carries %q, Reports() files no evidence for it, and "+
			"benchmarks.ValidationExemptions() does not name it. With "+
			"RISK_REQUIRE_VALIDATED_ANALYTICS armed — which is the default — this REFUSES THE "+
			"RISK-ENGINE START on every replica. Write the case set and wire it into Reports(), "+
			"or add a NAMED exemption stating why it cannot be graded and what retires the "+
			"entry. There is no count threshold to satisfy: eleven-of-twelve would accept any "+
			"eleven.", v)
	}

	// DEAD-ENTRY ARMS on the exemption list, both directions.
	//
	// THE SECOND ONE IS THE ONE #471 TURNS ON. A licence to serve unvalidated
	// must not outlive the reason for it: the day isda_simm's aggregation becomes
	// gradeable and somebody writes the case set, the exemption stops being an
	// argument and becomes a permission nobody re-examined. Failing the build is
	// what forces the deletion, since the metric would read `validated` either way
	// and nothing else would ever ask.
	for _, name := range exempt {
		if !inventoryValues[name] {
			t.Errorf("benchmarks.ValidationExemptions() names %q, which Inventory() no longer "+
				"carries — delete the entry rather than leaving a licence nobody needs", name)
		}
		if hasEvidence[name] {
			t.Errorf("benchmarks.ValidationExemptions() licenses %q to serve WITHOUT validation, "+
				"and Reports() now files evidence for it. The exemption is dead: delete it. Left "+
				"in place it is a standing permission for an analytic that no longer needs one, "+
				"and the next reader takes it as a statement that this analytic cannot be graded "+
				"— which the case set sitting beside it disproves.", name)
		}
	}
}

// exemptionsVar is the production declaration this guard reads the licensed
// analytics out of.
//
// IT IS PARSED, NOT RESTATED, AND NOT IMPORTED EITHER. This guard used to carry
// its own hand-kept copy of the list. That stopped being tolerable the moment the
// risk-engine composition root began reading the same list at RUNTIME to decide
// what may start unvalidated (#471 slice 3): two hand-kept copies of a control's
// exemption list is one copy that can quietly say yes after the other says no.
//
// The obvious repair — import benchmarks.ValidationExemptions() — was written
// first and REJECTED BY test/arch/risk_boundary_test.go, correctly: test/arch is
// an outsider to the risk module and outsiders go through internal/risk/api/*.
// The same guard rejected the benchmarks package's first placement for the same
// reason. So it is read from source, like everything else in this file, which
// keeps the single source of truth in production code without reaching past the
// boundary to get at it.
//
// frtb_sa WAS ON THAT LIST AND IS NOT ANY MORE, and the reason is the distinction
// it turns on. Its risk WEIGHTS are as unpublished as SIMM's; its AGGREGATION is
// MAR21.4 and MAR21.6, which are public text. A definitional case set grading only
// the aggregation was therefore possible on the weaker bar stress_framework
// already accepted, and writing it found the engine substituting a zero capital
// charge where MAR21.4(5) prescribes an alternative Sb (#471). So "no published
// vector" was never the right test for whether an analytic can be graded — "no
// published SPECIFICATION" is, and the two came apart there.
const exemptionsVar = "validationExemptions"

// exemptionKeys pulls the analytic names out of the `validationExemptions` map
// literal. Keys only: the guard checks the SHAPE of the licence, never the
// argument, which is prose no test can grade.
func exemptionKeys(decl *ast.GenDecl) (out []string, found bool) {
	for _, spec := range decl.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if name.Name != exemptionsVar || i >= len(vs.Values) {
				continue
			}
			found = true
			lit, ok := vs.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.BasicLit); ok && k.Kind == token.STRING {
					if s, err := strconv.Unquote(k.Value); err == nil {
						out = append(out, s)
					}
				}
			}
		}
	}
	return out, found
}

type analyticEntry struct {
	name      string // the identifier, or the unquoted string for a literal
	isLiteral bool
}

// returnsValidationCases reports whether fn has the case-set shape:
// `func F() []validation.Case`, no parameters, one result.
func returnsValidationCases(fn *ast.FuncDecl) bool {
	if fn.Type.Params != nil && len(fn.Type.Params.List) != 0 {
		return false
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		return false
	}
	arr, ok := fn.Type.Results.List[0].Type.(*ast.ArrayType)
	if !ok {
		return false
	}
	sel, ok := arr.Elt.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Case" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "validation"
}

// inventoryElements pulls the elements out of Inventory()'s returned slice.
func inventoryElements(fn *ast.FuncDecl) []analyticEntry {
	var out []analyticEntry
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, el := range lit.Elts {
			switch v := el.(type) {
			case *ast.Ident:
				out = append(out, analyticEntry{name: v.Name})
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					if s, err := strconv.Unquote(v.Value); err == nil {
						out = append(out, analyticEntry{name: s, isLiteral: true})
					}
				}
			}
		}
		return false
	})
	return out
}

// reportsSets pulls (analytic, case-set function) out of Reports()'s `sets`
// literal. The analytic is read in the same shape as an inventory entry — ident
// or bare string — because an exemption that gains evidence could gain it under
// EITHER spelling, and a walk that saw only idents would miss the literal one:
// exactly the entry the exemption list is about.
func reportsSets(fn *ast.FuncDecl) (analytics []analyticEntry, calls []string) {
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, el := range lit.Elts {
			row, ok := el.(*ast.CompositeLit)
			if !ok || len(row.Elts) != 2 {
				continue
			}
			switch v := row.Elts[0].(type) {
			case *ast.Ident:
				analytics = append(analytics, analyticEntry{name: v.Name})
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					if s, err := strconv.Unquote(v.Value); err == nil {
						analytics = append(analytics, analyticEntry{name: s, isLiteral: true})
					}
				}
			}
			if call, ok := row.Elts[1].(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					calls = append(calls, id.Name)
				}
			}
		}
		return false
	})
	return analytics, calls
}

// value resolves an inventory or Reports entry to the analytic NAME the gate and
// the metric use — the constant's value for an ident, the string itself for a
// literal. The exemption list is keyed by that name, so comparing it against
// identifiers would silently match nothing.
func (e analyticEntry) value(consts map[string]string) string {
	if e.isLiteral {
		return e.name
	}
	return consts[e.name]
}

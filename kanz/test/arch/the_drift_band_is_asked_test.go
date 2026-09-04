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

// THE DRIFT BAND MUST BE ASKED BY SOMETHING THAT RUNS (#1010).
//
// # The defect this exists to end
//
// internal/wealth carried the whole back half of target-weight rebalancing —
// SelectModel picks the model portfolio a household's risk profile selects,
// ComputeDrift diffs the household's actual weights against it, Drift.Breached
// decides whether the band is breached — written, unit-tested, and called from
// NOTHING. Not one production caller existed anywhere in the repository. The
// grep in #1010's evidence returned only internal/wealth's own files.
//
// So the platform could EXECUTE a rebalance a human asked for (the optimization
// service serves that path and is wired) and could not NOTICE that one was due.
// A book drifted past its stated allocation, nothing was wrong, nothing alerted,
// and the tracking error accumulated against a target the system held a schema
// for and no data. That is worse than an execution defect because there is no
// symptom: no error, no denial, no DLQ, no metric moving in the wrong direction.
//
// # Why an existing guard did not catch it, and cannot
//
// test/arch/no_dark_capability_test.go works at IMPORT granularity, and
// internal/wealth HAS importers — services/wealth uses Aggregate, Weights,
// AssetClassExposure and goals.go — so the guard saw a live package and passed.
// Its own line about a package "imported and still have a dead function inside
// it, which this will not catch" describes this case exactly. This guard closes
// that gap for the three symbols where the cost was measured, at CALL granularity,
// the way served_rpc_has_a_caller_test.go does for RPCs.
//
// # What this checks
//
// Each named symbol must be REACHED FROM OUTSIDE internal/wealth: either called
// directly from a non-test file elsewhere in the estate, or called by an
// internal/wealth function that is itself called from outside. The one hop is not
// a loosening — it is what makes the guard describe reachability rather than
// call-site location. ModelRegistry.Model resolves a household's target allocation
// THROUGH SelectModel, which is the one-implementation-per-concept shape this
// repository asks for; a guard that only accepted direct external calls would have
// rewarded copying the selector's body into the registry instead.
//
// The hop is exactly ONE deep, deliberately. Two hops would let a chain of dead
// helpers inside the package vouch for each other, which is the failure this guard
// exists to detect.
//
// It reads *ast.CallExpr rather than grepping, because a grep matches this guard's
// own prose, a doc comment naming the function, and a struct field that happens to
// share the name — three ways a guard has already passed in this repository with
// the checked thing deleted.
//
// It deliberately does NOT check what the caller DOES with the answer. Recording
// the drift on a metric, refusing an order, and emitting a proposal are all valid
// resolutions and the repository chose the first; a guard encoding one of them
// would have to be rewritten when another lands. What may never happen again is
// the state before #1010: the band exists, it is correct, and it is asked by
// nobody.
//
// # A LIMIT, recorded rather than left to be rediscovered
//
// wealth.Propose — which turns a breached band into a trade list by reusing the
// OPT-01 rebalance engine — is deliberately NOT in the list below, because it
// still has no production caller. #1010 delivered the DETECTION half: a target
// allocation is stored, a band is evaluated on every valuation FACT, and a
// drifted household is counted, logged and served on the read surface. Emitting a
// wealth.v1.Proposal is a recommendation with a lifecycle (an id, an approval, an
// expiry) and has no consumer, no store and no approval surface, so wiring
// Propose today would recreate the shape this guard exists to prevent one level
// out. Adding it here is the retirement condition for that follow-up, not
// something to do to make this list look complete.
func TestTheDriftBandIsAskedBySomethingThatRuns(t *testing.T) {
	root := moduleRoot(t)

	// The symbols, and what going dark again would cost. The message is the
	// failure output, so it has to be the thing a reader needs rather than a
	// restatement of the symbol name.
	wanted := map[string]string{
		"ComputeDrift": "the per-instrument deviation of a household's book from its model. With no " +
			"caller, no household is ever compared to its target allocation.",
		"Breached": "the band comparison itself — the thing that decides a rebalance is DUE. With no " +
			"caller, a book can sit any distance from its stated allocation and nothing asks.",
		"SelectModel": "the risk-profile → model portfolio selector. With no caller, a stored target " +
			"allocation is never matched to the households it governs.",
	}

	// NON-VACUITY (1/3): the symbols must still be DECLARED in internal/wealth.
	// Renaming or deleting one would otherwise leave this guard asserting a
	// requirement about a function that no longer exists — green, and vouching for
	// nothing.
	declared := declaredWealthFuncs(t, root)
	for name := range wanted {
		if !declared[name] {
			t.Fatalf("internal/wealth no longer declares %s. This guard's requirement is now about a "+
				"symbol that does not exist, so it would pass whatever the estate does. Update the "+
				"list to the new name, or delete the entry deliberately", name)
		}
	}

	callers, filesScanned := wealthSymbolCallers(t, root, wanted)

	// NON-VACUITY (2/3): the walk must actually have read the estate. A scanner
	// that parses nothing satisfies nothing, and the whole point of #1010 is that
	// "not looked at" and "looked at and clean" are indistinguishable from outside.
	if filesScanned < 400 {
		t.Fatalf("scanned only %d non-test .go files across internal/, services/ and cmd/ — the "+
			"walker is broken, so every assertion below is vacuous", filesScanned)
	}

	// The one hop: internal/wealth's own calls count only when the function making
	// them is itself reached from outside. Computed here rather than inside the
	// scanner so the two rules stay legible separately.
	external := externallyCalledWealthFuncs(t, root, declared)
	for name, sites := range internalCallers(t, root, wanted) {
		for _, enclosing := range sites {
			if external[enclosing] {
				callers[name] = appendUnique(callers[name], "internal/wealth via "+enclosing+"()")
			}
		}
	}

	for name, cost := range wanted {
		sites := callers[name]
		if len(sites) == 0 {
			t.Errorf("wealth.%s has NO production caller outside internal/wealth.\n"+
				"  What that costs: %s\n"+
				"  This is the state #1010 found the whole capability in: written, tested, and asked "+
				"by nobody. no_dark_capability_test.go cannot see it, because internal/wealth has "+
				"importers and works at import granularity.", name, cost)
			continue
		}
		sort.Strings(sites)
		t.Logf("wealth.%s is called from %v", name, sites)
	}
}

// declaredWealthFuncs returns the top-level func and method names declared in
// internal/wealth. It is the non-vacuity oracle: the guard's symbol list is
// checked against the package rather than assumed.
func declaredWealthFuncs(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	walkGoFiles(t, root, "internal/wealth", fset, func(_ string, f *ast.File) {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				out[fn.Name.Name] = true
			}
		}
	})
	if len(out) == 0 {
		t.Fatal("internal/wealth declares no functions at all — the parser or the path is wrong")
	}
	return out
}

// wealthSymbolCallers finds, for each wanted symbol, the module-relative files
// outside internal/wealth that CALL it. It returns the number of files scanned so
// the caller can prove the walk happened.
//
// A file only counts if it IMPORTS internal/wealth. That matters for Breached,
// which is a method name generic enough that another package could plausibly own
// one — without the import filter this guard would pass on somebody else's
// unrelated Breached and vouch for a band nobody asks.
func wealthSymbolCallers(t *testing.T, root string, wanted map[string]string) (map[string][]string, int) {
	t.Helper()
	const wealthPkg = `"github.com/eighred/kanz/internal/wealth"`
	callers := map[string][]string{}
	scanned := 0
	fset := token.NewFileSet()

	for _, sub := range []string{"internal", "services", "cmd"} {
		walkGoFiles(t, root, sub, fset, func(rel string, f *ast.File) {
			scanned++
			if strings.HasPrefix(rel, "internal/wealth/") {
				return // the package's own calls are what the defect looked like
			}
			imports := false
			for _, imp := range f.Imports {
				if imp.Path != nil && imp.Path.Value == wealthPkg {
					imports = true
					break
				}
			}
			if !imports {
				return
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				if _, want := wanted[name]; !want {
					return true
				}
				callers[name] = appendUnique(callers[name], rel)
				return true
			})
		})
	}

	// NON-VACUITY (3/3): at least one file in the estate must import
	// internal/wealth at all. If the import path is ever changed, the filter above
	// would silently exclude every file and each symbol would report "no caller" —
	// a loud failure, but for the wrong reason, sending the next reader after a
	// wiring bug that does not exist.
	if len(callers) == 0 {
		t.Log("no calls found; verifying the import filter is not the reason")
		found := false
		fset2 := token.NewFileSet()
		for _, sub := range []string{"internal", "services", "cmd"} {
			walkGoFiles(t, root, sub, fset2, func(rel string, f *ast.File) {
				if strings.HasPrefix(rel, "internal/wealth/") {
					return
				}
				for _, imp := range f.Imports {
					if imp.Path != nil && imp.Path.Value == wealthPkg {
						found = true
					}
				}
			})
		}
		if !found {
			t.Fatalf("no file outside internal/wealth imports %s. Either the package moved and this "+
				"guard's path is stale, or nothing uses the wealth domain at all — check which before "+
				"reading the failures below as a missing caller", wealthPkg)
		}
	}
	return callers, scanned
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// THE RISK PROFILE MUST SURVIVE THE DECODE (#1010).
//
// # The defect this exists to end
//
// wealth.v1.Household.risk_profile had been specified since WEALTH-01a with the
// comment "selects the model portfolio (WEALTH-01d)", and NOTHING IN THE ESTATE
// EVER CONSTRUCTED A Household. The selector's only input had no producer, no
// subject and no consumer, so SelectModel could not have been called even if
// something had wanted to. The field existed, was documented as load-bearing, and
// reached no running code — a silently dropped wire field, which is the #859 shape
// (risk_request_fields_are_read_test.go) applied to an allocation instead of an
// as-of date.
//
// # What this checks, and why that is the right property
//
// The wealth service's proto decoder must READ risk_profile off the wire message
// and the domain Household must CARRY it. Both halves, because either alone is the
// failure: a decoder that reads the field into a local that goes nowhere loses it
// just as completely as one that never reads it, and a domain field nothing
// populates is the state the whole capability was in.
//
// It does NOT check what the value is used for. Selecting a model, refusing an
// unknown profile and reporting it on the read surface are all downstream choices;
// the floor both a real answer and a refusal clear — and the silent third state
// cannot — is that the field crosses the decode at all.
func TestTheHouseholdRiskProfileSurvivesTheDecode(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// Half one: the DOMAIN type carries it. Read off the struct declaration rather
	// than grepped, so a mention in a comment cannot satisfy it.
	householdPath := filepath.Join(root, "internal", "wealth", "household.go")
	f, err := parser.ParseFile(fset, householdPath, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", householdPath, err)
	}
	if !structHasField(f, "Household", "RiskProfile") {
		t.Errorf("internal/wealth.Household has no RiskProfile field. The risk profile is the ONLY "+
			"input that selects a household's model portfolio, so without it on the domain type no "+
			"household can be measured against a target allocation — the state #1010 found. "+
			"(%s)", householdPath)
	}

	// Half two: the DECODER reads it off the wire message. A selector read is the
	// floor; the refusal behaviour for an unknown profile is held by the decoder's
	// own unit tests, which is the same division risk_request_fields_are_read_test.go
	// draws and for the same reason.
	decodePath := filepath.Join(root, "services", "wealth", "internal", "consume", "proto.go")
	df, err := parser.ParseFile(fset, decodePath, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", decodePath, err)
	}
	fn := funcDecl(df, "DecodeProto")
	if fn == nil {
		t.Fatalf("%s no longer declares DecodeProto. This guard is asserting a property of a function "+
			"that does not exist; find where the HouseholdValued decode moved to", decodePath)
	}
	// SCOPED TO DecodeProto'S OWN BODY, and that scoping is not cosmetic. The first
	// version of this arm searched the whole FILE, and a mutation that removed the
	// risk profile from the household decode entirely SURVIVED it — because
	// DecodeModelProto, in the same file, calls GetRiskProfile on a ModelPortfolio.
	// A guard that matches the neighbouring function proves nothing about this one.
	if !fileCallsSelector(fn, "GetRiskProfile") {
		t.Errorf("DecodeProto never calls GetRiskProfile(). A HouseholdValued carrying a risk "+
			"profile would be folded into a household that has none, and every household's drift "+
			"would report outcome=\"no_profile\" forever — indistinguishable from a firm that never "+
			"set one. (%s)", decodePath)
	}
	// And the value must reach the returned Household. Reading a field into a local
	// that goes nowhere loses it exactly as completely as never reading it, and that
	// is the state #859 named on a risk answer.
	if !compositeLiteralHasKey(fn, "Household", "RiskProfile") {
		t.Errorf("DecodeProto reads the risk profile but never puts it on the wealth.Household it "+
			"returns. The field crosses the wire, is decoded, and is dropped on the floor — which "+
			"is worse than not reading it, because the caller cannot tell. (%s)", decodePath)
	}
}

// funcDecl returns the named top-level function in f, or nil.
func funcDecl(f *ast.File, name string) *ast.FuncDecl {
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
			return fn
		}
	}
	return nil
}

// compositeLiteralHasKey reports whether n contains a composite literal of the
// named type carrying the named field key — e.g. wealth.Household{RiskProfile: p}.
func compositeLiteralHasKey(n ast.Node, typeName, key string) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		switch t := lit.Type.(type) {
		case *ast.SelectorExpr:
			if t.Sel.Name != typeName {
				return true
			}
		case *ast.Ident:
			if t.Name != typeName {
				return true
			}
		default:
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				found = true
			}
		}
		return true
	})
	return found
}

// structHasField reports whether the named struct type in f declares the named
// field.
func structHasField(f *ast.File, typeName, fieldName string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typeName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, fld := range st.Fields.List {
			for _, nm := range fld.Names {
				if nm.Name == fieldName {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// fileCallsSelector reports whether n contains a call to a selector with this
// method name, e.g. m.GetRiskProfile(). It takes any node so a caller can scope
// the search to one function body rather than a whole file.
func fileCallsSelector(n ast.Node, method string) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
			found = true
		}
		return true
	})
	return found
}

// externallyCalledWealthFuncs returns the internal/wealth functions and methods
// that ARE called from outside the package. It is the set the one-hop rule above
// is allowed to route through: a wealth function called by nobody cannot vouch for
// what it calls.
func externallyCalledWealthFuncs(t *testing.T, root string, declared map[string]bool) map[string]bool {
	t.Helper()
	const wealthPkg = `"github.com/eighred/kanz/internal/wealth"`
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, sub := range []string{"internal", "services", "cmd"} {
		walkGoFiles(t, root, sub, fset, func(rel string, f *ast.File) {
			if strings.HasPrefix(rel, "internal/wealth/") || !fileImportsQuoted(f, wealthPkg) {
				return
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && declared[sel.Sel.Name] {
					out[sel.Sel.Name] = true
				}
				return true
			})
		})
	}
	return out
}

// internalCallers maps each wanted symbol to the names of the internal/wealth
// functions that call it.
func internalCallers(t *testing.T, root string, wanted map[string]string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	fset := token.NewFileSet()
	walkGoFiles(t, root, "internal/wealth", fset, func(_ string, f *ast.File) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name // a package-local call: SelectModel(...)
				case *ast.SelectorExpr:
					name = fun.Sel.Name // a method call: d.Breached(...)
				default:
					return true
				}
				if _, want := wanted[name]; want && name != fn.Name.Name {
					out[name] = appendUnique(out[name], fn.Name.Name)
				}
				return true
			})
		}
	})
	return out
}

// fileImportsQuoted reports whether f imports the quoted path. Named for the
// quoted form because that is what it compares — test/arch already has an
// importsPath that takes an unquoted path and re-parses the file.
func fileImportsQuoted(f *ast.File, quoted string) bool {
	for _, imp := range f.Imports {
		if imp.Path != nil && imp.Path.Value == quoted {
			return true
		}
	}
	return false
}

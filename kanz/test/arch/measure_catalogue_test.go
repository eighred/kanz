package arch

import (
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"testing"
)

// THE MEASURE CATALOGUE MUST NAME EVERY MEASURE, INCLUDING THE ONES NOBODY
// SERVES (#509).
//
// compute.Catalogue is what MeasurePosture subtracts the live registry from, so
// a measure missing from it is a measure that CANNOT BE COUNTED AS DARK. The
// error is silent and it flatters: coverage reads better, and the one thing the
// metric exists to surface — an implemented measure that reaches no client — is
// the thing that disappears.
//
// # Why this guard and not a comment
//
// benchmarks.Inventory has the same hand-maintained shape and calls the gap "the
// known weakness … an analytic added without a line here is invisible to the
// count, which is the wrong direction for a control". That paragraph is correct
// and it is not enforcement. This is the enforcement, and the estate's rule says
// so: an invariant worth keeping is a guard, not a paragraph.
//
// # Both directions
//
// A missing entry hides a dark measure. A stale entry invents one — it reports a
// measure as dark forever after its constant was renamed or deleted, and a
// permanently-red gauge is one an operator learns to ignore, which costs more
// than the gauge was worth.
//
// # Entirely static, and that is not a compromise
//
// This reads BOTH sides out of the source rather than importing compute, because
// risk_boundary_test.go forbids outsiders from importing internal/risk/compute —
// api/* only — and test/arch is an outsider. Weakening that boundary to let a
// guard peek would have been the wrong trade: the boundary is load-bearing and
// this guard does not need it. Comparing the declared constant IDENTIFIERS
// against the catalogue's map KEYS is exactly equivalent, and it keeps working
// if compute stops compiling — which is when an inventory check is most useful.
//
// # Why the AST rather than a grep
//
// A grep for `Measure[A-Za-z]*` silently drops every name containing a DIGIT —
// VaR99, ES99, DV01, LVaR99, FactorVaR99 — which is five of the twenty-six, and
// four of those five are dark. The first pass at the catalogue was written from
// such a grep and was wrong in exactly that way. Parsing the declaration cannot
// make that mistake.

const cataloguePath = "internal/risk/compute/catalogue.go"

func TestMeasureCatalogueNamesEveryImplementedMeasure(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// ===== Every `MeasureXxx v1.MeasureName = "…"` constant under internal/risk
	declared := map[string]string{} // Ident -> "file"
	walkGoFiles(t, root, "internal/risk", fset, func(rel string, f *ast.File) {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || !isMeasureNameType(vs.Type) {
					continue
				}
				for _, ident := range vs.Names {
					declared[ident.Name] = rel
				}
			}
		}
	})

	// NON-VACUITY. A broken walk or a changed const idiom would find nothing and
	// the guard would pass having compared two empty sets — the failure mode it
	// exists to prevent, one level up.
	if len(declared) < 20 {
		t.Fatalf("found only %d measure-name constants under internal/risk — the walk or the "+
			"const-shape match is broken, not the estate", len(declared))
	}

	catalogued, badKeys := catalogueKeys(t, root, fset)
	if len(catalogued) < 20 {
		t.Fatalf("read only %d entries from %s — the map literal was not found, so this guard "+
			"would compare against nothing", len(catalogued), cataloguePath)
	}

	var missing, stale []string
	for name, where := range declared {
		if _, ok := catalogued[name]; !ok {
			missing = append(missing, name+" ("+where+")")
		}
	}
	for name := range catalogued {
		if _, ok := declared[name]; !ok {
			stale = append(stale, name)
		}
	}
	stale = append(stale, badKeys...)

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d measure(s) are implemented and ABSENT from the catalogue: %v.\n"+
			"A measure the catalogue does not name cannot be counted as dark — "+
			"kanz_risk_measure_live emits no series for it, and an implemented measure that reaches "+
			"no client becomes invisible in the direction that flatters coverage. Add it to %s with "+
			"the family whose seam registers it (#509).",
			len(missing), missing, cataloguePath)
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("%d catalogue entr(ies) name a measure that is not declared: %v.\n"+
			"The gauge would report it dark forever, and a permanently-red series is one an "+
			"operator learns to ignore. Delete the entry.", len(stale), stale)
	}
}

// AND EVERY ENTRY'S FAMILY MUST BE A DECLARED FamilyXxx CONSTANT.
//
// MeasureFamily is a string type, so a typo is not a compile error — it would
// split one family's gauge across two label values, and a dashboard filtering on
// the real name would show a family fully live while some of its measures sat
// under the misspelling.
func TestMeasureCatalogueUsesDeclaredFamilies(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	families := map[string]bool{}
	walkGoFiles(t, root, "internal/risk/compute", fset, func(rel string, f *ast.File) {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || !isIdentNamed(vs.Type, "MeasureFamily") {
					continue
				}
				for _, ident := range vs.Names {
					families[ident.Name] = true
				}
			}
		}
	})
	if len(families) < 4 {
		t.Fatalf("found only %d MeasureFamily constants — the scan is broken", len(families))
	}

	catalogued, _ := catalogueKeys(t, root, fset)
	used := map[string]bool{}
	var bad []string
	for measure, family := range catalogued {
		if !families[family] {
			bad = append(bad, measure+" -> "+family)
			continue
		}
		used[family] = true
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("catalogue entries use a family that is not a declared constant: %v.\n"+
			"MeasureFamily is a string type, so this is not a compile error — it silently splits "+
			"one family's gauge across two label values.", bad)
	}

	// Every declared family must be USED, or it is a label value no series will
	// ever carry — which reads as an empty dashboard panel rather than a missing
	// one.
	var unused []string
	for f := range families {
		if !used[f] {
			unused = append(unused, f)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		t.Errorf("declared famil(ies) %v name no catalogued measure — a label value no series "+
			"carries", unused)
	}
}

// catalogueKeys reads the `catalogue` map literal: measure identifier → family
// identifier. badKeys collects keys that are not identifiers at all (a string
// literal naming no constant), which is a stale entry by another route.
func catalogueKeys(t *testing.T, root string, fset *token.FileSet) (map[string]string, []string) {
	t.Helper()
	out := map[string]string{}
	var badKeys []string
	found := false
	walkGoFiles(t, root, "internal/risk/compute", fset, func(rel string, f *ast.File) {
		if rel != cataloguePath {
			return
		}
		found = true
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "catalogue" || len(vs.Values) != 1 {
					continue
				}
				lit, ok := vs.Values[0].(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						// A string-literal key names no constant, so nothing can
						// keep it honest when the measure is renamed.
						if bl, isLit := kv.Key.(*ast.BasicLit); isLit {
							if s, err := strconv.Unquote(bl.Value); err == nil {
								badKeys = append(badKeys, s+" (string literal, not a constant)")
							}
						}
						continue
					}
					val, ok := kv.Value.(*ast.Ident)
					if !ok {
						continue
					}
					out[key.Name] = val.Name
				}
			}
		}
	})
	if !found {
		t.Fatalf("%s was not walked — the path moved and this guard is comparing against nothing",
			cataloguePath)
	}
	return out, badKeys
}

// isMeasureNameType matches `v1.MeasureName` and a same-package `MeasureName`,
// so the guard does not depend on the import alias a file happens to use.
func isMeasureNameType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		return t.Sel.Name == "MeasureName"
	case *ast.Ident:
		return t.Name == "MeasureName"
	}
	return false
}

func isIdentNamed(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

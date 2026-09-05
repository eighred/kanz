package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// A MEASURE THAT NAMES NO MODEL CANNOT BE REFUSED (#1037).
//
// # What this exists to stop
//
// `compute.VaR99` is `0.01 × GrossExposure` — an illustrative constant, and the
// answer served by every deployment with no RISK_ENGINE_MARKETDATA_DATABASE_URL,
// which is every manifest in infra/. `varmodel.Historical` is historical
// simulation over a price panel. Both are published as `RiskMeasure{Name:
// "VaR99", Value: <Decimal>}`, and until #1037 that was the whole message: the
// two were BYTE-IDENTICAL on the wire. The OMS's pre-trade gate checked a
// mandate's VaR limit against whichever one arrived, and 1% of gross sits below a
// real one-day 99% VaR on a leveraged book, so the gate admitted orders the real
// number would have refused.
//
// # Why the coverage exemptions are exactly the population at risk
//
// The sibling guard, TestEveryMeasureCarriesItsCoverage, requires every measure
// to report how much of the book it was computed over — and exempts the measures
// that have NO PROVIDER, because nothing can decline for them and there is
// nothing to exclude. That exemption is correct and it is also the hole: those
// measures ship the one thing a consumer can act on (coverage) permanently
// absent, so provenance is the ONLY field left that separates arithmetic the
// engine stands behind from an illustrative constant.
//
// So the population is DERIVED from bareMeasureExempt rather than listed again
// here. A hand-written second list would be a further copy of the set that
// already broke, and it would drift the first time somebody exempts a new
// measure from coverage.
//
// # Why the method vocabulary is derived too
//
// riskview refuses on v1.MeasureMethod.IsPlaceholder, so a producer that invents
// a free-text method — "placeholder1pctgross", "placeholder-1pct-gross" — is
// unclassified, folds, and gates admission exactly as before. This guard resolves
// each declared method through the same predicate the OMS uses, so the two cannot
// disagree.
//
// The scan walks internal/risk/compute and internal/risk/api/v1 by absolute path
// only; it never walks the repository root, so sibling agent worktrees under
// .claude/ are outside it by construction.
func TestPlaceholderDeclaresItself(t *testing.T) {
	root := moduleRoot(t)
	computeDir := filepath.Join(root, "internal", "risk", "compute")
	apiDir := filepath.Join(root, "internal", "risk", "api", "v1")

	methodConsts := declaredMethodConstants(t, apiDir)
	if len(methodConsts) < 3 {
		t.Fatalf("found only %d v1.Method* constants in internal/risk/api/v1 — the scan is "+
			"broken, not the estate; the vocabulary has a placeholder and at least one real model",
			len(methodConsts))
	}

	var (
		undeclared  []string
		freeText    []string
		placeholder []string
		literals    int
	)

	for _, path := range goFilesInTree(t, computeDir) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			ast.Inspect(fn, func(m ast.Node) bool {
				lit, ok := m.(*ast.CompositeLit)
				if !ok || !isMeasureLiteral(lit) {
					return true
				}
				literals++

				prov := fieldValue(lit, "Provenance")
				if _, coverageExempt := bareMeasureExempt[fn.Name.Name]; coverageExempt && prov == nil {
					undeclared = append(undeclared,
						rel+" "+fn.Name.Name+" builds a v1.Measure with neither Coverage nor Provenance")
					return true
				}
				if prov == nil {
					return true
				}

				// The method as written. A string literal is refused outright:
				// the vocabulary is closed and riskview matches it exactly.
				switch method := provenanceMethod(prov).(type) {
				case nil:
					// Method comes from a parameter or a variable (varmodel's
					// zeroNamed takes it from its caller). Nothing to resolve
					// statically; the call sites are checked where they are written.
				case *ast.BasicLit:
					if method.Kind == token.STRING {
						s, _ := strconv.Unquote(method.Value)
						freeText = append(freeText, rel+" "+fn.Name.Name+" declares method "+
							strconv.Quote(s)+" as a bare string")
					}
				case *ast.SelectorExpr:
					id, isPkg := method.X.(*ast.Ident)
					if !isPkg || id.Name != "v1" {
						return true
					}
					val, known := methodConsts[method.Sel.Name]
					if !known {
						freeText = append(freeText, rel+" "+fn.Name.Name+" declares method v1."+
							method.Sel.Name+", which is not a declared method constant")
						return true
					}
					if v1.MeasureMethod(val).IsPlaceholder() {
						placeholder = append(placeholder, fn.Name.Name)
					}
				}
				return true
			})
			return false // one enclosing FuncDecl per literal is enough
		})
	}

	// NON-VACUITY. A scan that finds no measure literals, or resolves no method
	// at all, passes every arm above while the estate serves undeclared
	// placeholders.
	if literals < 8 {
		t.Fatalf("found only %d v1.Measure literals under internal/risk/compute — the scan is "+
			"broken, not the estate", literals)
	}
	sort.Strings(placeholder)
	if len(placeholder) < 2 {
		t.Errorf("only %d measure(s) declare a placeholder method (%v) — VaR99 (1%%×gross) and "+
			"Delta (net exposure) are BOTH placeholders and both are registered in "+
			"compute.DefaultRegistry, so a scan that sees fewer than two has stopped resolving "+
			"them and this guard is no longer checking anything",
			len(placeholder), placeholder)
	}

	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		t.Errorf("%d measure(s) are exempt from InputCoverage AND name no model:\n  %s\n\n"+
			"A coverage-exempt measure has already given up the field that says how much of the "+
			"book it saw. Provenance is the only thing left that separates arithmetic the engine "+
			"stands behind from an illustrative constant, and the OMS gate (riskview) refuses on "+
			"exactly that. Set Provenance: v1.MeasureProvenance{Method: v1.Method...} on the "+
			"literal — a placeholder method for a placeholder, the model's own method otherwise.",
			len(undeclared), strings.Join(undeclared, "\n  "))
	}
	sort.Strings(freeText)
	if len(freeText) > 0 {
		t.Errorf("%d measure(s) declare a method outside the v1.Method* vocabulary:\n  %s\n\n"+
			"riskview refuses on v1.MeasureMethod.IsPlaceholder, which matches the declared "+
			"constants exactly. A free-text method is unclassified, folds, and gates order "+
			"admission exactly as an undeclared placeholder does.",
			len(freeText), strings.Join(freeText, "\n  "))
	}
}

// TestPlaceholderRefusalIsNotSwitchedOff pins the OMS arm itself. Every symbol
// the guard above reads survives a `false &&` in front of the refusal (#771,
// #957): the constant, the predicate and the call site all still parse, and the
// gate silently folds placeholders again.
func TestPlaceholderRefusalIsNotSwitchedOff(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "services", "oms", "internal", "riskview", "riskview.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || !mentionsIdent(ifs.Cond, "IsPlaceholder") {
			return true
		}
		found++
		ast.Inspect(ifs.Cond, func(c ast.Node) bool {
			id, ok := c.(*ast.Ident)
			if ok && (id.Name == "true" || id.Name == "false") {
				t.Errorf("the placeholder refusal in riskview.go is guarded by the constant %q — "+
					"the branch is dead and an illustrative VaR gates order admission again, "+
					"while every symbol this guard reads is still present", id.Name)
			}
			return true
		})
		return true
	})
	if found == 0 {
		t.Fatal("riskview.go contains no `if ...IsPlaceholder...` refusal — the OMS no longer " +
			"declines a measure produced by a placeholder model, and #1037 is back on the " +
			"capital path")
	}
}

// declaredMethodConstants returns the v1.Method* constant names in dir mapped to
// their string values. Parsed rather than listed, so the vocabulary has exactly
// one definition.
func declaredMethodConstants(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, path := range goFilesInTree(t, dir) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Method") || i >= len(vs.Values) {
						continue
					}
					call, ok := vs.Values[i].(*ast.CallExpr)
					var lit *ast.BasicLit
					if ok && len(call.Args) == 1 {
						lit, _ = call.Args[0].(*ast.BasicLit)
					} else {
						lit, _ = vs.Values[i].(*ast.BasicLit)
					}
					if lit == nil || lit.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					out[name.Name] = s
				}
			}
		}
	}
	return out
}

// fieldValue returns the value expression for a named field of a composite
// literal, or nil when the field is not set.
func fieldValue(lit *ast.CompositeLit, name string) ast.Expr {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == name {
			return kv.Value
		}
	}
	return nil
}

// provenanceMethod digs the Method expression out of a Provenance value, which
// is either a v1.MeasureProvenance composite literal or a call/identifier the
// producer built elsewhere. Returns a nil ast.Expr interface when there is
// nothing to resolve statically.
func provenanceMethod(prov ast.Expr) ast.Expr {
	lit, ok := prov.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	return fieldValue(lit, "Method")
}

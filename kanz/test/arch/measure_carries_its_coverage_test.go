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

// A MEASURE THAT COMPUTED OVER NOTHING MUST SAY SO ON THE RESPONSE (#527, #509).
//
// # What this exists to stop
//
// #527 fixed four measure families that returned a confident zero when their
// provider declined — a DV01 of zero on a book of unpriceable bonds reads
// exactly like a book holding no bonds. It fixed them one file at a time, and
// nothing was left behind to hold the property.
//
// So liquidity shipped with the same shape and nobody noticed. LiquidationHorizon
// is REGISTERED IN PRODUCTION — unlike the families #527 fixed, which were all
// unregistered and therefore harmless-by-schedule — and it returned a bare zero.
// A horizon of zero days says the book unwinds instantly, which is the flattering
// direction and a number somebody sizes a position against.
//
// # Why a counter was not enough, in this repository's own words
//
// fi.go, on its own OnSkip hook:
//
//	THIS IS A METRIC, AND IT DOES NOT REPLACE THE RESPONSE ANNOTATION ... OnSkip
//	is a counter an operator watches across all portfolios and only sees if they
//	are looking; the InputCoverage this measure attaches to its own value travels
//	WITH the number, to the one caller acting on that one portfolio, at the moment
//	they act. #527 is precisely the case where the counter existed and was not
//	enough: the skips WERE counted, and the DV01 on the wire was still a confident
//	zero.
//
// Liquidity had the counter. It did not have the annotation.
//
// # What this checks
//
// Every v1.Measure composite literal built in internal/risk/compute sets
// Coverage, or its enclosing function is exempted with the reason. It is a
// structural check rather than a behavioural one on purpose: Measure.Coverage's
// own doc says its zero value means "this measure does not report input
// coverage", NOT "everything resolved" — so an unset field is indistinguishable
// at runtime from a clean one, and the only place to catch it is where it is
// written.
func TestEveryMeasureCarriesItsCoverage(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "internal", "risk", "compute")

	var bare []string
	seen := 0
	for _, path := range goFilesInTree(t, dir) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, _ := filepath.Rel(moduleRoot(t), path)
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
				seen++
				if hasField(lit, "Coverage") {
					return true
				}
				if _, exempt := bareMeasureExempt[fn.Name.Name]; exempt {
					return true
				}
				bare = append(bare, rel+" "+fn.Name.Name+" builds a v1.Measure with no Coverage")
				return true
			})
			return false // one enclosing FuncDecl per literal is enough
		})
	}

	// NON-VACUITY. A parse that finds no Measure literals passes this guard no
	// matter how many families report a bare zero.
	if seen < 8 {
		t.Fatalf("found only %d v1.Measure literals under internal/risk/compute — the scan is "+
			"broken, not the estate; there are measures in at least six families", seen)
	}

	sort.Strings(bare)
	if len(bare) > 0 {
		t.Errorf("%d measure(s) return a value with no input coverage:\n  %s\n\n"+
			"A measure that resolved nothing then returns the flattering number — zero exposure, "+
			"zero risk, an instant liquidation — and the caller cannot tell it from a real one. "+
			"Attach compute.Coverage: Contributed++ per position that reaches the arithmetic, "+
			"Exclude/ExcludeWhole for what does not, Coverage: cov.Result() on the way out.\n\n"+
			"If the measure has no provider and therefore nothing to exclude, add it to "+
			"bareMeasureExempt with that reason.",
			len(bare), strings.Join(bare, "\n  "))
	}

	// DEAD-ENTRY ARM. An exemption for a function that no longer builds a bare
	// measure is a standing licence the next reader takes as a decision.
	for fn := range bareMeasureExempt {
		if !strings.Contains(measureBuilderNames(t, dir), fn) {
			t.Errorf("bareMeasureExempt names %q, which builds no v1.Measure any more — delete it", fn)
		}
	}
}

// bareMeasureExempt are functions that build a v1.Measure without Coverage, with
// the reason. Two kinds only.
var bareMeasureExempt = map[string]string{
	// NO PROVIDER, SO NOTHING TO EXCLUDE. These read the portfolio directly; a
	// zero from them is arithmetic over the whole book and is always a claim the
	// engine can stand behind. Currency exclusions ride on the SET (#257), not on
	// the measure.
	"GrossExposure": "reads domain.Portfolio directly — no provider can decline",
	"NetExposure":   "reads domain.Portfolio directly — no provider can decline",
	"VaR99":         "the 1%-of-gross placeholder; arithmetic over the book, no provider",
	"Delta":         "the net-exposure placeholder RegisterGreeks overwrites; no provider",
	"HHI":           "concentration over the book itself — no provider can decline",

	// DARK AND TRACKED. These DO have providers and DO have the #527 shape, and
	// they are unregistered — which is a schedule, not a safeguard. Each is
	// blocked on something with its own home, and the entry retires when the
	// blocker does.
	"greekMeasure": "#509/#345 — RegisterGreeks has no production caller; blocked on a VolProvider, " +
		"which needs option premiums nothing in this estate persists. Widen this seam when it is wired.",

	// structMeasure WAS HERE AND IS NOT ANY MORE (#572). Its entry said the
	// schema had to come first — and that is still true of the schema ruling,
	// which #572 still needs — but it was doing double duty as a licence for the
	// measures to report a confident zero in the meantime. They do not:
	// structMeasure attaches coverage now, so a wired structured family refuses
	// instead of averaging over the empty set. The dark-seam exemption in
	// no_dark_measure_seam_test.go is untouched; that one is about wiring, this
	// one was about safety, and only the second was ever fixable without a
	// decision.
}

// isMeasureLiteral reports whether lit is a `v1.Measure{...}` composite literal.
func isMeasureLiteral(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Measure" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "v1"
}

func hasField(lit *ast.CompositeLit, name string) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	return false
}

// measureBuilderNames returns the concatenated names of every function in dir
// that builds a v1.Measure, for the dead-entry arm.
func measureBuilderNames(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, path := range goFilesInTree(t, dir) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			ast.Inspect(fn, func(m ast.Node) bool {
				if lit, ok := m.(*ast.CompositeLit); ok && isMeasureLiteral(lit) {
					b.WriteString(fn.Name.Name)
					b.WriteString(" ")
				}
				return true
			})
			return false
		})
	}
	return b.String()
}

// goFilesInTree lists the .go files directly under dir and its subdirectories.
func goFilesInTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v — the guard cannot check what it cannot read", dir, err)
	}
	sort.Strings(out)
	return out
}

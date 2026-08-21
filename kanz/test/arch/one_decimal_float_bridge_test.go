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

// THERE IS ONE Decimal→float64 CONVERSION, AND IT LIVES IN internal/dec (#628).
//
// # What went wrong without it
//
// There were EIGHTEEN implementations of this one concept in this module, in
// SEVEN mutually incompatible variants. They disagreed about all three
// properties that matter — what nil means, whether NaN/Inf is refused, and how a
// negative exponent is applied:
//
//	variant  nil ⇒      NaN/Inf    negative exponent   sites
//	A        0 silent   passes     × Pow10(exp)        6
//	B        0 silent   passes     × Pow10(exp)        3
//	C        0, false   passes     × Pow10(exp)        3
//	D        0, false   refused    × Pow10(exp)        2
//	E        0, false   refused    ÷ Pow10(-exp)       2
//	F        0 silent   passes     ÷ Pow10(-exp)       1
//	G        0, false   exact      exact (big.Rat)     1
//
// #514 found and documented the arithmetic: `coeff * Pow10(exp)` ROUNDS TWICE
// for a negative exponent, because Pow10(-6) is not exactly 1e-6. Its fix
// reached THREE of the eighteen. The fifteen that still carried the defect
// included every risk measure, every VaR/ES computation, the scenario
// revaluation shock factor and the performance layer's portfolio mark.
//
// This is the secret() shape the coding standard names, at eighteen sites rather
// than seventeen: a fix landed in the copies its author touched, reached nothing
// else, and no signal anywhere said so. Six of the copies even carried doc
// comments asserting they were "the same as" another copy — and by then that was
// false, because termsource had been made exact and spotsource had not.
//
// # What it checks
//
// A function whose parameters mention a Decimal and whose results include a
// float64 IS a conversion, wherever it lives. That is read from the AST — the
// same shape the issue's own scan used — rather than by name, because the
// eighteen were called decimalToFloat, decimalFloat, decFloat and decToFloat,
// and a nineteenth would be called something else again.
//
// internal/dec is the one place allowed to hold it. Test files are deliberately
// out of scope: a test may compute a float however it likes, and widening this
// to _test.go would trade real signal for noise on fixtures.
func TestThereIsOneDecimalFloatBridge(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	scanned, found := 0, 0
	seen := map[string]bool{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "gen" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/dec/") {
			return nil // the one home
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		scanned++
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Type == nil || fn.Name == nil {
				continue
			}
			if !mentionsDecimal(fn.Type.Params) || !returnsFloat64(fn.Type.Results) {
				continue
			}
			found++
			key := rel + ":" + fn.Name.Name
			seen[key] = true
			if _, exempt := decimalFloatBridgeExempt[key]; exempt {
				continue
			}
			offenders = append(offenders, key+" converts a Decimal to a float64 outside internal/dec. "+
				"There were eighteen of these in seven variants that disagreed about nil, about NaN/Inf, "+
				"and about the arithmetic itself — #514's one-ULP fix reached three of them. Use "+
				"dec.Float64 (or dec.Float64Or where a fallback is genuinely intended).")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// NON-VACUITY. This module has hundreds of non-test Go files. A walk that
	// scanned none would pass however many copies had grown back.
	if scanned < 200 {
		t.Fatalf("scanned only %d non-test Go file(s) — the walk is broken and this guard proves "+
			"nothing", scanned)
	}

	for key := range decimalFloatBridgeExempt {
		if !seen[key] {
			offenders = append(offenders, "the exemption for "+key+" is DEAD: no such Decimal→float64 "+
				"function exists any more")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("%d Decimal→float64 conversion(s) outside internal/dec:\n\n  %s",
			len(offenders), strings.Join(offenders, "\n\n  "))
	}
}

// decimalFloatBridgeExempt names Decimal→float64 functions allowed to live
// outside internal/dec, keyed "path:FuncName", with the issue that retires each.
//
// Empty, and that is the point: all eighteen were retired into internal/dec and
// none needed to stay. An entry here is permission; the dead-entry arm above
// removes it when the function goes.
var decimalFloatBridgeExempt = map[string]string{}

// mentionsDecimal reports whether any parameter's type names a Decimal — as a
// bare Decimal, a *Decimal, or a qualified commonpb.Decimal under any import
// alias, since the eighteen used several.
func mentionsDecimal(fl *ast.FieldList) bool {
	if fl == nil {
		return false
	}
	for _, f := range fl.List {
		if typeNames(f.Type, "Decimal") {
			return true
		}
	}
	return false
}

// returnsFloat64 reports whether any result is a float64. Any, not all: the
// eighteen split between `float64` and `(float64, bool)`.
func returnsFloat64(fl *ast.FieldList) bool {
	if fl == nil {
		return false
	}
	for _, f := range fl.List {
		if typeNames(f.Type, "float64") {
			return true
		}
	}
	return false
}

// typeNames reports whether e's type expression ends in name, looking through a
// pointer, a slice and a selector so *commonpb.Decimal and []*Decimal both match.
func typeNames(e ast.Expr, name string) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == name
	case *ast.StarExpr:
		return typeNames(t.X, name)
	case *ast.ArrayType:
		return typeNames(t.Elt, name)
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == name
	}
	return false
}

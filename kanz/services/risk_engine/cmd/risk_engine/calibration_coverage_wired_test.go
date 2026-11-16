package main

// THE CALIBRATOR MUST BE WIRED TO REPORT WHAT IT BUILT EACH CURVE FROM (#908).
//
// curve.Calibrator.OnCoverage is optional by type: nil disables it, and a
// Calibrator with no observer calibrates, publishes and serves exactly as
// before — no error, no warning, no metric. That is the state this issue found
// and it is one deleted line away at any time.
//
// The property is a COMPOSITION-ROOT property. Every unit test in the curve and
// livequote packages passes with this field unset, because the coverage still
// rides on the published curve; what disappears is the operator-facing half —
// the gauges and the one warning that names the dead instrument — and it
// disappears silently. No unit test under main.go sees this wiring, and this
// platform has twice shipped a startup defect under a green suite, so the test
// reads the composition root's own source and pins it.
//
// Same shape and same reason as calibration_cache_bound_test.go beside it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestCalibratorReportsStripCoverage(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var (
		sawCalibrator bool
		observer      ast.Expr
	)
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Calibrator" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "curve" {
			return true
		}
		sawCalibrator = true
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "OnCoverage" {
				observer = kv.Value
			}
		}
		return true
	})

	// NON-VACUITY. If the calibrator is built somewhere else this guard is
	// scanning nothing, and it must say so rather than pass.
	if !sawCalibrator {
		t.Fatal("main.go no longer constructs a curve.Calibrator literal — this guard is scanning " +
			"nothing. If calibration moved, move the check with it.")
	}

	if observer == nil {
		t.Fatal("curve.Calibrator is built with no OnCoverage (#908). Without it the pod publishes " +
			"a curve calibrated from three of nine configured instruments with no warning and no " +
			"metric, and it prices every flow past its last surviving pillar off a flat " +
			"extrapolation. The gauges kanz_risk_calibration_strip_{configured,quoted,missing} " +
			"are the only thing that says so — the coverage on the curve reaches a caller " +
			"pricing off it, not the operator whose reference spec has a typo in it.")
	}
	if id, ok := observer.(*ast.Ident); ok && id.Name == "nil" {
		t.Fatal("curve.Calibrator.OnCoverage is explicitly nil, which is the same as absent: the " +
			"strip-coverage gauges are never set and a short strip is silent again (#908).")
	}
}

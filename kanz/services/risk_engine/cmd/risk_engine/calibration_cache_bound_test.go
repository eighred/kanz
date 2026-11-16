package main

// The calibration quote cache must be bounded by the CONFIGURED strip (#894).
//
// livequote.LiveQuotes admits only the instruments it is constructed with, and
// that is the whole bound: there is no evictor, no TTL and no cap behind it. The
// property that makes it safe is a composition-root property — the cache and the
// rate source have to be given the SAME instrument slice. Hand livequote.New a
// different set (or an empty one) and the type is still correct while this pod
// either caches instruments nothing reads, or reads instruments it never cached.
//
// cfg.MarketSubjects defaults to `market.>`, so "caches instruments nothing
// reads" means one permanent entry per instrument the entire market spine has
// ever published, for the life of the pod. No unit test under main.go sees this
// wiring, and this platform has twice shipped a startup defect under a green
// suite — so the test reads the composition root's own source and pins it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// livequoteWiring returns the identifier passed to livequote.New and the one
// passed as NewSnapshotRateSource's instrument argument, "" when the call is
// absent or is not given a plain identifier.
func livequoteWiring(t *testing.T) (cacheUniverse, readerUniverse string, sawNew, sawSource bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	// identArg names arg i when it is a bare identifier — nil, a literal or a
	// re-derived expression all report "" and fail the comparison below, which
	// is the intent: the two calls must share one variable.
	identArg := func(call *ast.CallExpr, i int) string {
		if len(call.Args) <= i {
			return ""
		}
		id, ok := call.Args[i].(*ast.Ident)
		if !ok || id.Name == "nil" {
			// nil parses as an Ident; it is the empty case, not a variable.
			return ""
		}
		return id.Name
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
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "livequote" {
			return true
		}
		switch sel.Sel.Name {
		case "New":
			sawNew = true
			cacheUniverse = identArg(call, 0)
		case "NewSnapshotRateSource":
			sawSource = true
			readerUniverse = identArg(call, 1)
		}
		return true
	})
	return cacheUniverse, readerUniverse, sawNew, sawSource
}

func TestCalibrationCacheIsBoundedByTheConfiguredStrip(t *testing.T) {
	cacheUniverse, readerUniverse, sawNew, sawSource := livequoteWiring(t)

	// NON-VACUITY: both calls must still be here. If calibration is rewired
	// elsewhere this guard silently checks nothing.
	if !sawNew {
		t.Fatal("main.go no longer calls livequote.New — this guard is scanning nothing. If the " +
			"calibration cache moved, move the check with it.")
	}
	if !sawSource {
		t.Fatal("main.go no longer calls livequote.NewSnapshotRateSource — this guard is scanning " +
			"nothing.")
	}

	if cacheUniverse == "" {
		t.Fatal("livequote.New is not given a plain instrument-slice identifier. The cache's admitted " +
			"key space IS its bound (#894): constructed with nil it admits nothing and the calibrator " +
			"silently never gets a quote, and constructed from a re-derived expression the set can " +
			"drift from the one the rate source reads.")
	}
	if cacheUniverse != readerUniverse {
		t.Fatalf("livequote.New is built from %q while NewSnapshotRateSource reads %q. They must be the "+
			"SAME slice: on the `market.>` wildcard default, caching a set the reader does not ask "+
			"about is one permanent entry per instrument the spine has ever published, and reading a "+
			"set the cache does not admit is a strip that never ticks.", cacheUniverse, readerUniverse)
	}

	// And that identifier has to be the PARSED reference spec, not some other
	// slice that happens to be in scope: ParseRateInstruments is what refuses a
	// mistyped strip at startup.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	fromParse := false
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) == 0 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || id.Name != cacheUniverse {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ParseRateInstruments" {
			fromParse = true
		}
		return true
	})
	if !fromParse {
		t.Errorf("%q is not assigned from livequote.ParseRateInstruments — the cache's key space must "+
			"come from the configured reference spec, which is the thing that fails loudly on a "+
			"mistyped strip", cacheUniverse)
	}
}

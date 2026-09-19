package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// #1082 guards the four periodic return values and both connector/root seams.
// Completeness alone permits a named nil callback, which is not observability.
func TestReconcileErrorsReachProductionObserver(t *testing.T) {
	root := moduleRoot(t)
	parse := func(path string) *ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, path), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, venue := range []string{"binance", "okx"} {
		base := "services/venue-" + venue
		rec := parse(base + "/internal/" + venue + "/" + venue + "_recon.go")
		for _, method := range []string{"Reconcile", "HealClosures"} {
			observed := false
			ast.Inspect(rec, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "onReconcileError" {
					return true
				}
				for _, arg := range call.Args {
					if inner, ok := arg.(*ast.CallExpr); ok {
						if s, ok := inner.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == method {
							observed = true
						}
					}
				}
				return true
			})
			if !observed {
				t.Errorf("%s %s error is not observed", venue, method)
			}
		}
		connector := parse(base + "/internal/" + venue + "/" + venue + "_connector.go")
		forwarded := false
		ast.Inspect(connector, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "OnReconcileError" {
				return true
			}
			value, ok := kv.Value.(*ast.SelectorExpr)
			if ok && value.Sel.Name == "OnReconcileError" {
				forwarded = true
			}
			return true
		})
		if !forwarded {
			t.Errorf("%s connector drops observer", venue)
		}
		main := parse(base + "/cmd/venue-" + venue + "/main.go")
		wired := false
		ast.Inspect(main, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "OnReconcileError" {
				return true
			}
			sel, ok := kv.Value.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Observe" {
				return true
			}
			call, ok := sel.X.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			ctor, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || ctor.Sel.Name != "NewReconcileErrorObserver" {
				return true
			}
			reg, ok := call.Args[0].(*ast.SelectorExpr)
			if ok && reg.Sel.Name == "Registry" {
				wired = true
			}
			return true
		})
		if !wired {
			t.Errorf("%s production observer lacks registered counter", venue)
		}
	}
}

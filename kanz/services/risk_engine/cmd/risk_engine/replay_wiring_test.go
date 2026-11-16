package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Arithmetic tests using wrapped fixtures cannot catch a raw provider left in
// the production composition root. Such a provider computes live successfully
// but leaves no historical input. Hold every live registration at this boundary.
func TestProductionRiskProvidersEnterTheRetainedInputScope(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	isWrapper := func(expr ast.Expr, name string) bool {
		literal, ok := expr.(*ast.CompositeLit)
		if !ok {
			return false
		}
		typ, ok := literal.Type.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := typ.X.(*ast.Ident)
		return ok && pkg.Name == "replay" && typ.Sel.Name == name
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 3 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			owner, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			name := selector.Sel.Name
			wrapper := ""
			var fields map[string]string
			switch {
			case owner.Name == "varmodel" && name == "Register":
				wrapper = "Returns"
			case owner.Name == "compute" && name == "RegisterFactorRisk":
				fields = map[string]string{"Model": "Models"}
			case owner.Name == "compute" && name == "RegisterFIRisk":
				fields = map[string]string{"Terms": "Bonds", "Curve": "Curves"}
			case owner.Name == "compute" && name == "RegisterStructuredRisk":
				fields = map[string]string{"Terms": "Structures", "Curve": "Curves"}
			case owner.Name == "compute" && name == "RegisterLiquidityRisk":
				wrapper = "Liquidity"
			case owner.Name == "compute" && name == "RegisterMarginRisk":
				if id, ok := call.Args[2].(*ast.Ident); ok && id.Name == "nil" {
					return true
				}
				wrapper = "Margins"
			default:
				return true
			}
			seen[name] = true
			if wrapper != "" {
				if !isWrapper(call.Args[2], wrapper) {
					t.Errorf("%s: %s must retain provider inputs with replay.%s", fset.Position(call.Pos()), name, wrapper)
				}
				return true
			}
			literal, ok := call.Args[2].(*ast.CompositeLit)
			if !ok {
				t.Errorf("%s: uninspectable provider binding", name)
				return true
			}
			for field, expected := range fields {
				found := false
				for _, element := range literal.Elts {
					kv, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					id, ok := kv.Key.(*ast.Ident)
					if ok && id.Name == field {
						found = isWrapper(kv.Value, expected)
					}
				}
				if !found {
					t.Errorf("%s: %s.%s must retain inputs with replay.%s", fset.Position(call.Pos()), name, field, expected)
				}
			}
			return true
		})
	}
	if len(seen) != 6 {
		t.Fatalf("provider registration coverage changed: %v", seen)
	}
}

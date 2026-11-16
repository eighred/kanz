package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

func numericFunction(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	for _, path := range []string{"numeric.go", "server.go"} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, data, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == name {
				return f
			}
		}
	}
	t.Fatalf("owning function %s missing", name)
	return nil
}

func numericCalls(node ast.Node, name string) bool {
	if node == nil {
		return false
	}
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			found = found || fn.Name == name
		case *ast.SelectorExpr:
			found = found || fn.Sel.Name == name
		}
		return true
	})
	return found
}

// The work limit must gate the real converter, not just reject its already
// expanded result. Source order proves that without executing hostile powers.
func TestAccountingNumericSourceOrder(t *testing.T) {
	t.Run("work_guard_before_conversion", func(t *testing.T) {
		f := numericFunction(t, "parseRat")
		guarded := false
		conversions := 0
		for _, statement := range f.Body.List {
			if numericCalls(statement, "SetString") {
				conversions++
				if !guarded {
					t.Fatal("SetString can expand before the work refusal")
				}
			}
			if gate, ok := statement.(*ast.IfStmt); ok {
				negation, ok := gate.Cond.(*ast.UnaryExpr)
				if ok && negation.Op == token.NOT && len(gate.Body.List) == 1 {
					call, ok := negation.X.(*ast.CallExpr)
					if ok && numericCalls(call, "numericWorkBound") && len(call.Args) == 1 {
						input, ok := call.Args[0].(*ast.Ident)
						if ok && input.Name == "s" {
							_, guarded = gate.Body.List[0].(*ast.ReturnStmt)
						}
					}
				}
			}
		}
		if conversions != 1 || !guarded {
			t.Fatalf("expected one qualified conversion, got %d", conversions)
		}
	})
	t.Run("shared_result_guard_after_conversion", func(t *testing.T) {
		converted, qualified := false, false
		for _, statement := range numericFunction(t, "parseRat").Body.List {
			converted = converted || numericCalls(statement, "SetString")
			if gate, ok := statement.(*ast.IfStmt); ok && converted && numericCalls(gate.Init, "ExactFromRat") && len(gate.Body.List) == 1 {
				_, qualified = gate.Body.List[0].(*ast.ReturnStmt)
			}
			if ret, ok := statement.(*ast.ReturnStmt); ok && len(ret.Results) == 2 {
				if err, ok := ret.Results[1].(*ast.Ident); ok && err.Name == "nil" && !qualified {
					t.Fatal("financial result returned without shared exact-domain refusal")
				}
			}
		}
		if !qualified {
			t.Fatal("shared result qualification missing")
		}
	})
	t.Run("request_FX_before_book_read", func(t *testing.T) {
		qualified := false
		for _, statement := range numericFunction(t, "handleNAV").Body.List {
			if numericCalls(statement, "toRatMap") {
				ast.Inspect(statement, func(node ast.Node) bool {
					if field, ok := node.(*ast.SelectorExpr); ok && field.Sel.Name == "FX" {
						qualified = true
					}
					return true
				})
			}
			if numericCalls(statement, "materialize") && !qualified {
				t.Fatal("request FX is parsed only after financial reads")
			}
		}
		if !qualified {
			t.Fatal("request FX qualification missing")
		}
	})
	t.Run("shared_body_limit_before_decode", func(t *testing.T) {
		limited := false
		for _, statement := range numericFunction(t, "decode").Body.List {
			if numericCalls(statement, "MaxBytesReader") {
				ast.Inspect(statement, func(node ast.Node) bool {
					if field, ok := node.(*ast.SelectorExpr); ok && field.Sel.Name == "MaxBytes" {
						limited = true
					}
					return true
				})
			}
			if numericCalls(statement, "Decode") && !limited {
				t.Fatal("request decoded before the shared body ceiling")
			}
		}
		if !limited {
			t.Fatal("shared body ceiling missing")
		}
	})
}

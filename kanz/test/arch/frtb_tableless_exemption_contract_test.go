package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// ChargeForScenario cannot report a missing supervisory weight. Its exemption
// remains safe only while Charge validates every bucket before maxScenario and
// no other capital-path caller reaches the tableless helper (#1331).
func TestFRTBTablelessExemptionHasCheckedCallers(t *testing.T) {
	root := moduleRoot(t)
	const sbm = "internal/regulatory/frtb/sbm.go"
	const benchmarks = "internal/risk/benchmarks/frtb.go"
	var chargeDecl *ast.FuncDecl
	maxCalls, scenarioCalls, benchmarkCalls := 0, 0, 0
	maxRefs, scenarioRefs := 0, 0
	for path, source := range productionSources(t, root) {
		if !strings.Contains(source, "ChargeForScenario(") && !strings.Contains(source, "maxScenario(") {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		var functions []*ast.FuncDecl
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				functions = append(functions, fn)
				if rel == sbm && fn.Name.Name == "Charge" {
					chargeDecl = fn
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok {
				switch ident.Name {
				case "maxScenario":
					maxRefs++
				case "ChargeForScenario":
					scenarioRefs++
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := frtbCallName(call.Fun)
			if name != "ChargeForScenario" && name != "maxScenario" {
				return true
			}
			owner := "<package init>"
			for _, fn := range functions {
				if fn.Pos() <= call.Pos() && call.End() <= fn.End() {
					owner = fn.Name.Name
					break
				}
			}
			switch name {
			case "maxScenario":
				maxCalls++
				if rel != sbm || owner != "Charge" {
					t.Errorf("unchecked maxScenario caller: %s in %s", owner, rel)
				}
			case "ChargeForScenario":
				if rel == benchmarks {
					benchmarkCalls++ // fixed worked examples, never an admitted book
				} else {
					scenarioCalls++
					if rel != sbm || owner != "maxScenario" {
						t.Errorf("unchecked ChargeForScenario caller: %s in %s", owner, rel)
					}
				}
			}
			return true
		})
	}
	if maxCalls != 1 || scenarioCalls != 2 || benchmarkCalls < 1 {
		t.Fatalf("tableless call inventory changed: max=%d, scenario=%d, benchmarks=%d", maxCalls, scenarioCalls, benchmarkCalls)
	}
	// A function-value alias can bypass call-site discovery. Every symbol use
	// must be its declaration or one of the direct calls counted above.
	if maxRefs != maxCalls+1 || scenarioRefs != scenarioCalls+benchmarkCalls+1 {
		t.Fatalf("tableless helper used as a value: max refs=%d calls=%d, scenario refs=%d calls=%d",
			maxRefs, maxCalls, scenarioRefs, scenarioCalls+benchmarkCalls)
	}
	if chargeDecl == nil || !frtbChargeChecksBucketsBeforeScenario(chargeDecl) {
		t.Fatal("Charge no longer refuses missing bucket weights immediately before maxScenario")
	}
}

func frtbCallName(expr ast.Expr) string {
	switch call := expr.(type) {
	case *ast.Ident:
		return call.Name
	case *ast.SelectorExpr:
		return call.Sel.Name
	default:
		return ""
	}
}

func frtbChargeChecksBucketsBeforeScenario(charge *ast.FuncDecl) bool {
	for _, statement := range charge.Body.List {
		loop, ok := statement.(*ast.RangeStmt)
		if !ok {
			continue
		}
		for i, statement := range loop.Body.List {
			guard, ok := statement.(*ast.IfStmt)
			if !ok || i+1 >= len(loop.Body.List) {
				continue
			}
			init, ok := guard.Init.(*ast.AssignStmt)
			if !ok || len(init.Rhs) != 1 {
				continue
			}
			call, ok := init.Rhs[0].(*ast.CallExpr)
			if !ok || frtbCallName(call.Fun) != "coversBuckets" {
				continue
			}
			condition, ok := guard.Cond.(*ast.BinaryExpr)
			if !ok || condition.Op != token.NEQ || !frtbIsIdent(condition.X, "err") || !frtbIsIdent(condition.Y, "nil") {
				continue
			}
			if len(guard.Body.List) != 1 {
				continue
			}
			refusal, ok := guard.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(refusal.Results) != 2 || !frtbIsIdent(refusal.Results[1], "err") {
				continue
			}
			checkedScenario := false
			ast.Inspect(loop.Body.List[i+1], func(n ast.Node) bool {
				if next, ok := n.(*ast.CallExpr); ok && frtbCallName(next.Fun) == "maxScenario" {
					checkedScenario = true
				}
				return true
			})
			if checkedScenario {
				return true
			}
		}
	}
	return false
}

func frtbIsIdent(expr ast.Expr, want string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == want
}

func TestFRTBTablelessGuardRejectsUncheckedMutation(t *testing.T) {
	for _, test := range []struct {
		name, body string
		want       bool
	}{
		{"checked", `for range book { if err := coversBuckets(); err != nil { return 0, err }; total += maxScenario() }`, true},
		{"scenario before check", `for range book { total += maxScenario(); if err := coversBuckets(); err != nil { return 0, err } }`, false},
		{"ignored coverage error", `for range book { _ = coversBuckets(); total += maxScenario() }`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "mutation.go", "package frtb\nfunc Charge() { "+test.body+" }", 0)
			if err != nil {
				t.Fatal(err)
			}
			charge := file.Decls[0].(*ast.FuncDecl)
			if got := frtbChargeChecksBucketsBeforeScenario(charge); got != test.want {
				t.Fatalf("coverage gate accepted = %t, want %t", got, test.want)
			}
		})
	}
}

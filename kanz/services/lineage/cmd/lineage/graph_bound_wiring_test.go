package main

// The event index must be constructed WITH A CAP and WITHOUT a completeness
// claim (#244).
//
// Same composition-root technique as services/oms/cmd/oms/mandate_arm_wiring_test.go:
// there is no unit under main.go's wiring to exercise, so the test reads main.go's
// own source and checks the shape.
//
// TWO SEPARATE REGRESSIONS, AND THE SECOND IS THE DANGEROUS ONE.
//
// graph.NewMemory() with no cap is the OOMKill the issue opens with, and it is
// at least loud — the pod dies and somebody looks.
//
// graph.WithCompleteHistory() is silent and worse. It is the one-word edit that
// makes the 410s stop, and it is very easy to reach for: the answers get tidier
// and nothing appears to break. What it actually does is re-assert that this
// pod's few minutes of retained index is the estate's whole history, so every
// unretained event goes back to answering 404 "there is no record" — the exact
// defect, restored, with a cap in place making the misses MORE common than they
// were before. It is only ever earned by a graph rebuilt from the stream's first
// sequence; the consumer in runHarvest resumes at last ack and cannot earn it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestEventIndexIsWiredBoundedAndNotDeclaredComplete(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var (
		newMemoryCalls int
		capped         bool
		completePos    token.Pos
	)
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
		if !ok || pkg.Name != "graph" {
			return true
		}
		switch sel.Sel.Name {
		case "NewMemory":
			newMemoryCalls++
			for _, arg := range call.Args {
				inner, ok := arg.(*ast.CallExpr)
				if !ok {
					continue
				}
				if s, ok := inner.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "WithEventIndexCapacity" {
					capped = true
				}
			}
		case "WithCompleteHistory":
			completePos = call.Pos()
		}
		return true
	})

	if newMemoryCalls != 1 {
		t.Fatalf("found %d graph.NewMemory call(s) in main.go, want exactly 1", newMemoryCalls)
	}
	if !capped {
		t.Error("graph.NewMemory is constructed without graph.WithEventIndexCapacity — " +
			"this service subscribes to \">\", so an uncapped event index grows with the " +
			"estate's entire event rate until the pod is OOMKilled (#244)")
	}
	if completePos.IsValid() {
		t.Errorf("%s: main.go passes graph.WithCompleteHistory. The harvester below is a "+
			"DURABLE consumer resuming at last ack — it never re-reads the history a "+
			"restart dropped, so this pod cannot know that a miss means 'never observed'. "+
			"With this option every unretained event answers 404 'there is no record' "+
			"instead of 410 'I have no record', which is the whole of #244",
			fset.Position(completePos))
	}
}

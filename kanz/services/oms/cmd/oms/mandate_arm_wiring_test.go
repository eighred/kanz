package main

// The mandate registry must arm from a REPLAY-COMPLETE signal, and readiness
// must not flip true until it has (EXEC-M13).
//
// This is the same composition-root property price_spine_test.go pins for the
// price spine: there is no unit under main.go's wiring to exercise directly, so
// the test reads main.go's own source and checks the shape, the same technique
// already established in this package rather than a new one.
//
// Before this fix, main.go called consumer.SubscribeBroadcast(ctx, mandateSub,
// mandateConsumer.Handle) — no ready signal at all — and readiness.Set(true) ran
// immediately after LAUNCHING that goroutine. OMS_REQUIRE_MANDATE defaults to
// false, so the window between "pod reports Ready" and "the mandate replay has
// actually folded" is a window where every portfolio resolves as having no
// mandate and every order is admitted unconstrained — EXEC-M13 turned from "only
// on a broken durable-group replay" into "a race on every rolling restart".

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestMandateRegistryArmsBeforeReadinessIsSet(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var (
		readyArgWired  bool // SubscribeBroadcastReady(ctx, mandateSub, ..., mandateReg.Arm)
		armedGatesWait bool // the wait loop's condition calls mandateReg.Armed()
		armedPos       token.Pos
		readySetPos    token.Pos
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

		switch sel.Sel.Name {
		case "SubscribeBroadcastReady":
			// Last argument is the ready callback: must be mandateReg.Arm, not nil
			// and not omitted (which would make this plain SubscribeBroadcast again).
			if len(call.Args) == 0 {
				return true
			}
			if argSel, ok := call.Args[len(call.Args)-1].(*ast.SelectorExpr); ok {
				if id, ok := argSel.X.(*ast.Ident); ok && id.Name == "mandateReg" && argSel.Sel.Name == "Arm" {
					readyArgWired = true
				}
			}
		case "Armed":
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "mandateReg" {
				armedGatesWait = true
				armedPos = call.Pos()
			}
		case "Set":
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "readiness" {
				// readiness.Set(true) — the specific call this test cares about is the
				// one inside runConsumers gating on the mandate replay, which is the
				// LAST such call lexically before wg.Wait() in that function. Recording
				// every Set(true)/Set(false) call's position and taking the one that
				// follows armedPos is sufficient here because main.go has exactly one
				// readiness.Set(true) in runConsumers.
				if len(call.Args) == 1 {
					if lit, ok := call.Args[0].(*ast.Ident); ok && lit.Name == "true" {
						readySetPos = call.Pos()
					}
				}
			}
		}
		return true
	})

	if !readyArgWired {
		t.Error("the mandate subscription is not wired through SubscribeBroadcastReady with mandateReg.Arm as the " +
			"ready callback — without it the registry never learns when the initial replay has folded, and " +
			"readiness can only be a guess (EXEC-M13)")
	}
	if !armedGatesWait {
		t.Error("no wait loop in main.go polls mandateReg.Armed() — readiness must not flip true until the " +
			"mandate replay has folded, or a restarted OMS reports healthy while every portfolio is unconstrained")
	}
	if armedGatesWait && readySetPos != 0 && readySetPos < armedPos {
		t.Error("readiness.Set(true) appears BEFORE the mandateReg.Armed() wait loop in main.go's source — " +
			"it must be gated behind the wait, not precede it, or the pod reports ready before the mandate " +
			"replay has actually landed (EXEC-M13)")
	}
}

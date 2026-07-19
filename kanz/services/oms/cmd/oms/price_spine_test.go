package main

// The price spine must be BROADCAST, not a work queue.
//
// cfg.PriceSubject folds into an in-process mark map that the pre-trade
// compliance gate values MARKET and STOP orders from. A durable consumer group
// LOAD-BALANCES (pkg/bus/consumer.go), so with the shipped replicas: 2 each pod
// folded only the ticks it happened to receive — the same MARKET order admitted
// by one pod and refused PRICE_UNAVAILABLE by the other, and for a thin
// instrument a pod holding no mark at all indefinitely. It fails safe, but
// admission stops being deterministic, which on a compliance gate is its own
// defect. This is exactly the argument main.go already makes for the mandate
// registry directly below the price subscription.
//
// This is a composition-root property: there is no unit under it to test. So the
// test reads the composition root's own source and pins the wiring.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// priceSubjectWiring reports how cfg.PriceSubject is wired in main.go.
func priceSubjectWiring(t *testing.T) (broadcast bool, workQueue bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	isPriceSubject := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "PriceSubject" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "cfg"
	}
	containsPriceSubject := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok && isPriceSubject(e) {
				found = true
			}
			return !found
		})
		return found
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || !containsPriceSubject(n) {
				return true
			}
			switch {
			case strings.HasPrefix(sel.Sel.Name, "SubscribeBroadcast"):
				broadcast = true
			case sel.Sel.Name == "Subscribe":
				workQueue = true
			}
		case *ast.CompositeLit:
			// subs = append(subs, sub{cfg.PriceSubject, marks.Handle}) — the
			// slice consumed by the work-queue Subscribe loop.
			if id, ok := n.Type.(*ast.Ident); ok && id.Name == "sub" && containsPriceSubject(n) {
				workQueue = true
			}
		}
		return true
	})
	return broadcast, workQueue
}

func TestPriceSpineIsBroadcastNotAWorkQueue(t *testing.T) {
	broadcast, workQueue := priceSubjectWiring(t)
	if workQueue {
		t.Error("cfg.PriceSubject is wired through the WORK-QUEUE path (consumer.Subscribe / the subs slice). " +
			"A consumer group load-balances, so each OMS replica would fold only some of the ticks and the same " +
			"MARKET order would be admitted by one pod and refused PRICE_UNAVAILABLE by another")
	}
	if !broadcast {
		t.Error("cfg.PriceSubject is not subscribed via SubscribeBroadcast — a mark is replicated STATE, " +
			"not work, and every replica's compliance gate needs it")
	}
}

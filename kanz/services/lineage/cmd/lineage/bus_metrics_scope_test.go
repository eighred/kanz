package main

// THE BUS SERIES ARE REGISTERED ONLY WHERE THERE IS A BUS (#352).
//
// Same composition-root technique as graph_bound_wiring_test.go beside it: there
// is no unit under run()'s wiring to exercise, so this reads main.go's own source
// and checks the shape.
//
// WHAT IT DEFENDS. Hoisting the transport out of runHarvest put the metric
// registrations in run(), where the obvious placement — next to the other
// MustRegister calls at the top — is wrong in a way nothing else catches:
//
//	kanz_bus_publish_total                        0
//	kanz_lineage_auth_decisions_lost_total{...}   0
//
// A lineage pod with no LINEAGE_NATS_URL would export exactly that. A counter of
// LOST audit decisions reading zero is read as "the observation stream is
// complete" — while in truth nothing was ever published to it and every AUTH-01d
// decision exists only in a log line that dies at the next rollout. It is the
// same silence #352 exists to remove, reintroduced by a metric that looks
// healthy.
//
// Absent means not wired. Zero means wired and clean. Prometheus can express the
// difference and an operator relies on it, so this pins the registrations inside
// the `cfg.NATSURL != ""` branch and fails if they drift back out.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// busBranch returns the `if cfg.NATSURL != ""` statement that dials the bus.
// There are two such conditions in run() — this picks the one whose body calls
// bus.DialNATS, so the test cannot silently anchor on the harvest branch.
func busBranch(t *testing.T, f *ast.File) *ast.IfStmt {
	t.Helper()
	var found *ast.IfStmt
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		dials := false
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			if sel, ok := callee(m); ok && sel == "bus.DialNATS" {
				dials = true
			}
			return true
		})
		if dials && found == nil {
			found = ifs
		}
		return true
	})
	if found == nil {
		t.Fatal("no `if` branch in main.go calls bus.DialNATS.\n\n" +
			"Either the transport moved again or the dial is now unconditional. If it is " +
			"unconditional, lineage can no longer start without a broker and the read-only " +
			"deployment is broken — see recorder_wiring_test.go.")
	}
	return found
}

// callee renders a call's function as "pkg.Name" (or "a.b.Name"), or reports
// false for anything that is not a selector call.
func callee(n ast.Node) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	switch x := sel.X.(type) {
	case *ast.Ident:
		return x.Name + "." + sel.Sel.Name, true
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + x.Sel.Name + "." + sel.Sel.Name, true
		}
	}
	return "", false
}

func TestBusSeriesAreRegisteredOnlyWhenABusIsConfigured(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	branch := busBranch(t, f)

	// Every registration of a bus-scoped series, wherever it appears.
	type site struct {
		what string
		pos  token.Pos
	}
	var sites []site
	ast.Inspect(f, func(n ast.Node) bool {
		name, ok := callee(n)
		if !ok {
			return true
		}
		switch name {
		case "bus.NewBusMetrics":
			sites = append(sites, site{"bus.NewBusMetrics (kanz_bus_*)", n.Pos()})
		case "obs.Registry.MustRegister":
			// Only the lost-decisions counter is bus-scoped; the coverage gauges
			// beside it are meaningful with no broker at all.
			for _, arg := range n.(*ast.CallExpr).Args {
				if id, ok := arg.(*ast.Ident); ok && id.Name == "authDecisionsLost" {
					sites = append(sites, site{"authDecisionsLost (kanz_lineage_auth_decisions_lost_total)", n.Pos()})
				}
			}
		}
		return true
	})

	// NON-VACUITY: if neither registration is found the loop above proves nothing,
	// and a rename would turn this test permanently green.
	if len(sites) != 2 {
		t.Fatalf("found %d bus-scoped metric registrations in main.go, want 2 "+
			"(bus.NewBusMetrics and MustRegister(authDecisionsLost)).\n\n"+
			"They were renamed or removed, and this guard is now matching nothing.", len(sites))
	}

	for _, s := range sites {
		if s.pos < branch.Body.Pos() || s.pos > branch.Body.End() {
			t.Errorf("%s is registered at %s, OUTSIDE the `cfg.NATSURL != \"\"` branch at %s.\n\n"+
				"A deployment with no broker will now export this series at zero. For the lost-decisions "+
				"counter that reads as \"no audit decisions were lost\" when in fact none were ever "+
				"published — the silence #352 exists to remove, wearing a healthy-looking metric.\n\n"+
				"Absent means not wired; zero means wired and clean. Move the registration inside the branch.",
				s.what, fset.Position(s.pos), fset.Position(branch.Pos()))
		}
	}
}

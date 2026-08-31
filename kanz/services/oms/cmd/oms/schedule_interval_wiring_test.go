package main

// ADMISSION AND THE DRIVER MUST BE GIVEN THE SAME TICK (#898).
//
// order.Service refuses a schedule whose slices are closer together than the
// driver's tick — but only if it is TOLD what that tick is. A Service built
// without WithScheduleInterval falls back to DefaultScheduleInterval, which is
// fail-closed and still wrong the moment a deployment sets OMS_SCHEDULE_INTERVAL
// to anything else: admission would then refuse schedules the driver could
// actually work (a lowered tick), or admit ones it cannot (a raised one).
//
// Neither failure is visible. The order is accepted or refused, and the only
// evidence that the two halves disagree is in child send times nobody is
// reading — the same invisibility that let #898's overflow sit behind a warning
// gauge nobody acted on.
//
// This is composition-root wiring, so there is no unit under it to test: the
// value is read from config in one file and consumed in another, and a dropped
// option compiles perfectly. The test reads the composition root's own source
// and pins the fact that BOTH consumers are handed the SAME config field.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestScheduleIntervalIsWired(t *testing.T) {
	files := []*ast.File{parseMainHere(t)}

	var handedToService, handedToDriver bool
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// order.WithScheduleInterval(cfg.ScheduleInterval)
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "order" &&
				sel.Sel.Name == "WithScheduleInterval" && len(call.Args) == 1 &&
				isCfgScheduleInterval(call.Args[0]) {
				handedToService = true
			}
			return true
		})
		// The driver's own use of the same field — the other half of the pair.
		ast.Inspect(f, func(n ast.Node) bool {
			if isCfgScheduleInterval(n) {
				handedToDriver = true
			}
			return true
		})
	}

	if !handedToDriver {
		t.Fatal("cfg.ScheduleInterval is not read anywhere under services/oms/cmd — this guard " +
			"is asserting nothing, because the field it pins has no consumer at all")
	}
	if !handedToService {
		t.Fatal("nothing under services/oms/cmd calls order.WithScheduleInterval(cfg.ScheduleInterval).\n\n" +
			"The order Service then falls back to DefaultScheduleInterval while the driver ticks " +
			"on OMS_SCHEDULE_INTERVAL, so the two disagree about which schedules are workable. A " +
			"deployment that lowered the tick would have legitimate schedules refused at " +
			"admission; one that raised it would admit schedules the driver silently coarsens. " +
			"Neither is visible in anything but child send times (#898).")
	}
}

// isCfgScheduleInterval reports whether n is the selector `cfg.ScheduleInterval`.
func isCfgScheduleInterval(n ast.Node) bool {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ScheduleInterval" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "cfg"
}

// parseMainHere parses this composition root's main.go, which is where both the
// config field and the option call live. Same approach as price_spine_test.go
// and mandate_arm_wiring_test.go, which pin other wiring in this file.
func parseMainHere(t *testing.T) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v — this guard reads the composition root itself; if the "+
			"wiring moved, move the guard rather than dropping the check", err)
	}
	return f
}

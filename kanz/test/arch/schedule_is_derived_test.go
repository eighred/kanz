package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// AN EXECUTION SCHEDULE IS DERIVED, NEVER HELD (#435).
//
// #435 names the trap it must not fall into, in its own words:
//
//	"An algo that keeps its schedule in memory turns a pod restart into an
//	 abandoned parent order with children half-sent — the exact failure class
//	 #292 (transactional outbox) and the PENDING_NEW sweep exist to prevent. The
//	 schedule must be durable from slice 1, or slice 1 is not done."
//
// internal/execution/algo answers that not with a schedule table but by being a
// PURE FUNCTION of fields the parent order already stores durably: quantity,
// window, slice count. Two pods reach the same schedule, and a pod that has just
// booted reaches the same schedule as the one it replaced. There is nothing to
// lose, so nothing can be lost.
//
// THAT PROPERTY IS AN ACCIDENT OF THE CURRENT CODE UNLESS SOMETHING HOLDS IT.
// TWAP is the easy algo. #435's later slices are POV and IS, and both are
// genuinely tempted toward state: POV wants to remember how much volume it has
// already participated in, IS wants to remember its decay. The natural way to
// write either is a struct with a mutex and a running total — and the day that
// lands, a rolling deploy starts abandoning parent orders again, silently, with
// every unit test still green because a test never restarts the process.
//
// WHAT THIS CHECKS, in internal/execution/algo only:
//
//  1. No package-level mutable state. A schedule that can be mutated between two
//     calls is not derivable from the order.
//  2. No wall-clock read. A schedule that reads time.Now() inside computes a
//     different answer on the pod that recovers it than on the pod that lost it,
//     which is the same failure wearing a different shape. The clock belongs in
//     the DRIVER, which asks "what is due now" — never in the schedule.
//  3. No import that IS state or I/O: a store, a bus, a database, or sync. A
//     scheduler that needs a mutex is a scheduler holding something.
//
// WHAT IT CANNOT CHECK: that the durable fields a schedule derives FROM are
// actually persisted on the parent order, or that the driver sends each child
// exactly once. Those are properties of the order store and its outbox, proved
// where they live. This guard bounds one package to purity; it does not prove
// the rest of the path correct.

// schedulePkg is the tree this guard bounds. Narrow on purpose: the point is
// that ONE package is pure, so a driver elsewhere may hold whatever it needs.
const schedulePkg = "internal/execution/algo"

// scheduleForbiddenImports are packages whose presence means state or I/O has
// entered the schedule, mapped to what their arrival would break.
var scheduleForbiddenImports = map[string]string{
	"sync":                            "a scheduler needing a mutex is a scheduler holding something between calls",
	"sync/atomic":                     "same, in the shape that looks cheapest",
	"database/sql":                    "a schedule read from a store is a second copy of the order to fall out of sync with",
	"github.com/jackc/pgx/v5":         "same",
	"github.com/eighred/kanz/pkg/bus": "a schedule that emits is a driver, not a schedule; the driver is what may send",
	"github.com/eighred/kanz/internal/marketdata/store": "POV's temptation: read the volume here and the schedule stops being derivable",
}

// scheduleStateExempt maps a module-relative file to an argued reason it may
// hold package-level mutable state, and what retires the entry.
//
// EMPTY, AND THAT IS THE POINT. An entry here is a decision that some part of an
// execution schedule may not survive a restart, and it should be as hard to add
// as that sentence sounds.
var scheduleStateExempt = map[string]string{}

func TestExecutionScheduleIsDerivedNotHeld(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(schedulePkg))

	files := goFilesUnder(t, dir)
	// NON-VACUITY, the walk half: a renamed or moved package scans nothing and
	// this guard passes while asserting nothing about anything.
	nonTest := 0
	for _, f := range files {
		if !strings.HasSuffix(f.rel, "_test.go") {
			nonTest++
		}
	}
	if nonTest == 0 {
		t.Fatalf("no non-test Go files under %s — the package moved and this guard is now "+
			"protecting an empty directory, not an execution schedule", schedulePkg)
	}

	fset := token.NewFileSet()
	var (
		offenders  []string
		seenExempt = map[string]bool{}
		sawPlanner bool
	)

	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			// A TEST MAY HOLD STATE. It is not what restarts.
			continue
		}
		rel := schedulePkg + "/" + f.rel
		file, err := parser.ParseFile(fset, filepath.Join(dir, filepath.FromSlash(f.rel)), f.body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		note := func(format string, args ...any) {
			if reason, ok := scheduleStateExempt[rel]; ok {
				seenExempt[rel] = true
				t.Logf("%s: exempt — %s", rel, reason)
				return
			}
			offenders = append(offenders, rel+": "+strings.TrimSpace(fmt.Sprintf(format, args...)))
		}

		// (3) IMPORTS.
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for bad, why := range scheduleForbiddenImports {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					note("imports %s — %s", path, why)
				}
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.FuncDecl:
				if v.Name.Name == "TWAP" {
					// NON-VACUITY, the subject half: if the planner is gone or
					// renamed, this guard is checking the purity of nothing.
					sawPlanner = true
				}

			case *ast.GenDecl:
				// (1) PACKAGE-LEVEL MUTABLE STATE.
				//
				// `var` at package scope only. Sentinel errors are the one shape
				// that is genuinely immutable in practice and universal in this
				// repo, so they are allowed by shape rather than by exemption —
				// a caller that reassigns one has a much louder problem than a
				// lost schedule.
				if v.Tok != token.VAR {
					return true
				}
				for _, spec := range v.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					if allSentinelErrors(vs) {
						continue
					}
					for _, name := range vs.Names {
						if name.Name == "_" {
							continue // an interface-satisfaction assertion holds nothing
						}
						note("declares package-level var %q — a schedule that can differ between "+
							"two calls is not derivable from the parent order", name.Name)
					}
				}

			case *ast.SelectorExpr:
				// (2) THE WALL CLOCK.
				if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "time" {
					switch v.Sel.Name {
					case "Now", "Since", "Until", "Tick", "After", "NewTicker", "NewTimer":
						note("reads the wall clock via time.%s — the schedule must be the same on "+
							"the pod that recovers a parent order as on the pod that lost it; the "+
							"clock belongs to the driver asking what is DUE", v.Sel.Name)
					}
				}
			}
			return true
		})
	}

	if !sawPlanner {
		t.Fatalf("found no TWAP planner under %s — this guard asserts the purity of a scheduler "+
			"that is no longer there", schedulePkg)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d violation(s) of the derived-schedule rule in %s:\n  %s\n\n"+
			"#435: \"An algo that keeps its schedule in memory turns a pod restart into an "+
			"abandoned parent order with children half-sent.\" This package answers that by being a "+
			"pure function of the parent's durable fields — quantity, window, slice count — so a "+
			"restarted pod recomputes exactly what the lost one intended to send. State, a clock, "+
			"or a store inside it breaks that silently: every unit test stays green, because a unit "+
			"test never restarts the process, and the loss only appears as a part-filled parent "+
			"nobody can explain after a rolling deploy.\n"+
			"Put the state in the DRIVER, which may hold it because the order store is what makes "+
			"it durable — or add an argued entry to scheduleStateExempt.",
			len(offenders), schedulePkg, strings.Join(offenders, "\n  "))
	}

	// DEAD-ENTRY ARM: an exemption whose file no longer offends has outlived its
	// repair and must not stay to license the next one.
	for rel, reason := range scheduleStateExempt {
		if !seenExempt[rel] {
			t.Errorf("exemption for %q (%s) matches nothing — the file was repaired, moved, or "+
				"deleted; remove the entry so it cannot silently license a future violation",
				rel, reason)
		}
	}
}

// allSentinelErrors reports whether every name in the spec is bound to an
// errors.New / fmt.Errorf value — the one package-level shape that is immutable
// in practice.
func allSentinelErrors(vs *ast.ValueSpec) bool {
	if len(vs.Values) == 0 || len(vs.Values) != len(vs.Names) {
		return false
	}
	for _, val := range vs.Values {
		call, ok := val.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return false
		}
		switch {
		case pkg.Name == "errors" && sel.Sel.Name == "New":
		case pkg.Name == "fmt" && sel.Sel.Name == "Errorf":
		default:
			return false
		}
	}
	return true
}

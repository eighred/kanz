package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A PRODUCTION SITE THAT TRACKS AN IN-FLIGHT CLOSE MUST NAME THE ORDER *AND* THE
// INSTRUMENT (#1036).
//
// execution.CloseIntent is what the In-Flight Certainty watchdog is handed when a
// cancel is dispatched. The watchdog's first act is to map InstrumentID to a
// venue symbol so it can ask the exchange what became of the order; a lookup miss
// makes the intent undropped-but-unanswerable, and both reconcilers drop it.
//
// venueadapter/server.CancelOrder built the intent with `OrderID` alone. NOTHING
// FAILED: Go zero-fills an omitted field, StaticSymbolMap.Symbol("") is an
// ordinary map miss, and the miss was handled as the perfectly reasonable "this
// venue does not trade that instrument". So in the out-of-process deployment —
// the ONLY one that reaches a real exchange, because GRPCVenue.OwnsCloseTracking
// makes the adapter rather than the OMS the writer — every ambiguous cancel was
// discarded without one question asked of the venue: no StateHealed, no
// force-clear, no sweep, no balance re-anchor.
//
// THE CONSEQUENCE IS AN UNBOOKED POSITION. handleCancel writes CANCELLED
// regardless, CANCELLED is terminal, and both sweeps and resume() list only
// PENDING_NEW / ROUTED / PARTIALLY_FILLED — so the order is never revisited. The
// exchange keeps a resting, fillable order while the OMS, the position book,
// risk, compliance and the IBOR all believe it withdrawn, and a later fill
// reconciles to QUARANTINE ("this is a caller defect").
//
// WHY A GUARD AND NOT ONE EDIT. Every test covering this seam constructed the
// intent by hand with InstrumentID already set, so the suite proved the watchdog
// worked on an intent nothing in production produced, and stayed green for as
// long as the defect existed. A third writer would repeat it with nothing to
// stop it. This is the same mechanism, and the same argument, as
// TestEveryWorkerDepsLiteralNamesEveryCollaborator (#418).
//
// WHAT IT CHECKS: every keyed execution.CloseIntent literal outside test files
// names every exported field of the struct that has no safe default.
//
// WHAT IT CANNOT CHECK: that the value is RIGHT. `InstrumentID: ""` satisfies
// this guard — deliberately. The point is to make the field a decision visible in
// a diff rather than one nobody typed.

// closeIntentType is the struct every literal below must fill.
const closeIntentType = "CloseIntent"

// closeIntentDecl is where it is declared, module-relative.
const closeIntentDecl = "internal/execution/closes.go"

// closeIntentScopes are the trees searched for literals of it. BOTH are needed:
// the OMS writes one from services/, and the venue adapter's gRPC shell — the
// writer that carried the defect — lives in internal/.
var closeIntentScopes = []string{"internal", "services"}

// closeIntentOptional are fields a literal need not name, with the reason.
//
// EVERY ENTRY HAS A DOCUMENTED DEFAULT WHOSE BEHAVIOUR IS IDENTICAL TO NAMING IT,
// and that is the whole rule. OrderID and InstrumentID are absent from this map
// because neither has one: an unnamed OrderID addresses no order and an unnamed
// InstrumentID makes the venue query unformable, and in both cases the zero value
// silently disables the healing seam rather than selecting a documented default.
var closeIntentOptional = map[string]string{
	"SweepSide":   "zero is SIDE_UNSPECIFIED, which suppresses the sweep — the REQUIRED behaviour for a cancelled resting order, since it has not traded and sweeping would open a brand-new position",
	"Leaves":      "nil means no residual exposure to flatten (force-clear only); omitting it is the same behaviour as naming a non-positive quantity",
	"RequestedAt": "zero ⇒ CloseRegistry.Track stamps time.Now().UTC(); omitting it is the same behaviour as naming the dispatch instant",
}

func TestEveryCloseIntentLiteralNamesTheOrderAndTheInstrument(t *testing.T) {
	root := moduleRoot(t)

	want := exportedStructFields(t, filepath.Join(root, filepath.FromSlash(closeIntentDecl)), closeIntentType)
	// NON-VACUITY: five fields today. A rename or a move that made this come back
	// empty would turn the guard into a no-op that passes.
	if len(want) < 4 {
		t.Fatalf("execution.%s has %d exported fields (%v) — expected at least 4. The declaration "+
			"moved or was renamed and this guard is asserting nothing", closeIntentType, len(want), want)
	}
	// NON-VACUITY, THE HALF THAT MATTERS: the two fields with no safe default must
	// still be the ones this guard requires. If either left the struct, or an
	// exemption were added for it, the guard would pass while requiring nothing
	// the defect was about.
	for _, required := range []string{"OrderID", "InstrumentID"} {
		if _, exempt := closeIntentOptional[required]; exempt {
			t.Fatalf("%q is exempted in closeIntentOptional. It has no safe default: an unnamed "+
				"instrument makes the healing watchdog's venue query unformable, which is #1036 "+
				"itself. This guard now requires nothing it was written for", required)
		}
		if !contains(want, required) {
			t.Fatalf("execution.%s no longer has a %q field — this guard is watching a struct that "+
				"changed shape underneath it", closeIntentType, required)
		}
	}

	seen := map[string]bool{}
	for _, scope := range closeIntentScopes {
		for _, gf := range goFilesUnder(t, filepath.Join(root, filepath.FromSlash(scope))) {
			if strings.HasSuffix(gf.rel, "_test.go") {
				continue
			}
			rel := scope + "/" + gf.rel
			fset := token.NewFileSet()
			// Comments are NOT parsed: a guard that reads prose matches its own
			// explanation and the field names quoted in the doc comment beside the
			// struct, and passes with the checked thing deleted.
			f, err := parser.ParseFile(fset, rel, gf.body, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isCloseIntentLit(lit) {
					return true
				}
				seen[rel] = true
				got := map[string]bool{}
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						return true // unkeyed: the compiler already requires every field
					}
					if id, ok := kv.Key.(*ast.Ident); ok {
						got[id.Name] = true
					}
				}
				var missing []string
				for _, field := range want {
					if got[field] {
						continue
					}
					if _, optional := closeIntentOptional[field]; optional {
						continue
					}
					missing = append(missing, field)
				}
				if len(missing) > 0 {
					sort.Strings(missing)
					t.Errorf("%s:%d builds an execution.%s without %v.\n"+
						"An omitted field is zero-filled with no error anywhere, and the healing "+
						"watchdog then drops the close without asking the exchange about it — while "+
						"the order is already recorded CANCELLED, which is terminal. The exchange "+
						"keeps a live, fillable order nothing will ever revisit. That is #1036 "+
						"exactly. Name the field.",
						rel, fset.Position(lit.Pos()).Line, closeIntentType, missing)
				}
				return true
			})
		}
	}

	// NON-VACUITY: both production writers must have been found — the OMS's
	// closeAtVenue and the venue adapter's CancelOrder. Fewer means the
	// construction shape moved and this guard now watches less than it thinks.
	if len(seen) < 2 {
		t.Fatalf("found execution.%s literals in %d file(s) (%v) — expected at least 2 (the OMS "+
			"close dispatcher and the venue adapter's gRPC shell). The construction shape moved "+
			"and this guard is asserting nothing", closeIntentType, len(seen), keysOf(seen))
	}

	// DEAD-ENTRY ARM: an optional-field entry naming a field that no longer exists
	// would wave through a future field that reused the name.
	have := map[string]bool{}
	for _, f := range want {
		have[f] = true
	}
	for f, reason := range closeIntentOptional {
		if !have[f] {
			t.Errorf("optional entry for %q (%s) matches no execution.%s field — delete it",
				f, reason, closeIntentType)
		}
	}
}

// isCloseIntentLit reports whether lit constructs execution.CloseIntent, named
// either bare (inside package execution, or through a package-local type alias)
// or qualified (everywhere else).
func isCloseIntentLit(lit *ast.CompositeLit) bool {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		return t.Name == closeIntentType
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && pkg.Name == "execution" && t.Sel.Name == closeIntentType
	}
	return false
}

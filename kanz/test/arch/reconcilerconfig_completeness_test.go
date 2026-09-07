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

// A CONNECTOR THAT BUILDS A RECONCILER MUST BUILD ALL OF IT (#1063).
//
// Each venue connector's Start assembles its reconciler from one keyed composite
// literal. Both literals omitted `OnUnknownBalance` — the seam that says an asset
// could NOT be compared against the exchange.
//
// NOTHING ABOUT THAT FAILED. Go zero-fills an omitted field in a composite
// literal, so there was no compile error and no log line: the callback was nil,
// r.unknownBalance did nothing, and reconcileBalances `continue`d to the next
// asset. The pass then returned nil, having compared nothing and said nothing.
//
// THE FAILURE IS INVISIBLE IN THE DIRECTION THAT MATTERS. accounting.balance
// .reconciled is emitted only on a DISCREPANCY, so an asset that was SKIPPED and
// an asset that AGREED produce the identical observable estate: no FACT, no
// counter, no line. That is AGENTS.md's rule verbatim — "nothing configured" and
// "checked, and fine" must never look the same — and it lands on the layer that
// is the last thing able to notice a mis-booked position, a fill the websocket
// missed, or an execution posted to the wrong account.
//
// WHY A GUARD AND NOT TWO EDITS. Both reconcilers' own unit tests set the field
// by hand, so the suite proved a seam nothing in production used and stayed green
// for as long as the defect existed — the same shape as #1036, where every test
// built a CloseIntent the real writer never produced. A third venue adapter would
// repeat it with nothing to stop it, exactly as the second repeated the first.
// This is the third application of the #418 mechanism
// (TestEveryWorkerDepsLiteralNamesEveryCollaborator), after
// TestEveryCloseIntentLiteralNamesTheOrderAndTheInstrument.
//
// WHAT IT CHECKS: every keyed reconciler-config literal outside test files names
// every exported field of the struct that has no documented default. The field
// set is DERIVED FROM THE STRUCT via the AST, not written here — a list in the
// guard would be a further copy of exactly the thing that broke, which is
// somebody enumerating a set by hand and missing a member.
//
// WHAT IT CANNOT CHECK: that the value is USEFUL. `OnUnknownBalance: nil`
// satisfies this guard — deliberately. The point is not to force an
// implementation into existence; it is to make the absence a VISIBLE DECISION in
// a diff, with a reason beside it, rather than a field nobody typed.

// reconcilerConfigs are the venue reconciler configs and where each is declared,
// module-relative. Two entries because there are two exchange connectors, and
// the whole argument for a guard is that the third must not be able to repeat
// what the second repeated from the first.
var reconcilerConfigs = []struct {
	typeName string
	decl     string
	// scope is the tree searched for literals of it.
	scope string
}{
	{
		typeName: "ReconcilerConfig",
		decl:     "services/venue-binance/internal/binance/binance_recon.go",
		scope:    "services/venue-binance",
	},
	{
		typeName: "OKXReconcilerConfig",
		decl:     "services/venue-okx/internal/okx/okx_recon.go",
		scope:    "services/venue-okx",
	},
}

// reconcilerConfigOptional are fields a literal need not name, with the reason.
//
// EVERY ENTRY HAS A DOCUMENTED DEFAULT WHOSE BEHAVIOUR IS IDENTICAL TO NAMING IT,
// and that is the whole rule. A SEAM IS NEVER OPTIONAL — a nil collaborator
// silently disables a capability while leaving the pass returning nil, which is
// the defect this guard exists for. `Closes` is deliberately absent from this map
// even though nil disables the healing watchdog: that is a capability, not a
// default.
var reconcilerConfigOptional = map[string]string{
	"Now":          "nil ⇒ time.Now; omitting it is the same behaviour as naming the production clock",
	"CloseTimeout": "<=0 ⇒ execution.DefaultCloseTimeout; omitting it is the same behaviour as naming the default",
}

// reconcilerConfigRequired are the fields with NO safe default that this guard
// exists to require. Named explicitly as a non-vacuity arm, in the shape #1036's
// guard uses: if one of them left the struct, or somebody exempted it, the guard
// would keep passing while requiring nothing it was written for.
var reconcilerConfigRequired = []string{"Balances", "OnUnknownBalance", "OnCloseUnhealable", "Pub"}

func TestEveryReconcilerConfigLiteralNamesEverySeam(t *testing.T) {
	root := moduleRoot(t)

	total := 0
	for _, cfg := range reconcilerConfigs {
		want := exportedStructFields(t, filepath.Join(root, filepath.FromSlash(cfg.decl)), cfg.typeName)
		// NON-VACUITY: twelve fields today. A rename or a move that made this come
		// back empty would turn the guard into a no-op that passes.
		if len(want) < 10 {
			t.Fatalf("%s has %d exported fields (%v) — expected at least 10. The declaration "+
				"moved or was renamed and this guard is asserting nothing", cfg.typeName, len(want), want)
		}
		// NON-VACUITY, THE HALF THAT MATTERS: the fields with no safe default must
		// still be the ones this guard requires.
		for _, required := range reconcilerConfigRequired {
			if _, exempt := reconcilerConfigOptional[required]; exempt {
				t.Fatalf("%q is exempted in reconcilerConfigOptional. It has no safe default: a nil "+
					"seam disables the capability while the reconciliation pass still returns nil, "+
					"which is #1063 itself. This guard now requires nothing it was written for", required)
			}
			if !contains(want, required) {
				t.Fatalf("%s no longer has a %q field — this guard is watching a struct that "+
					"changed shape underneath it", cfg.typeName, required)
			}
		}

		seen := map[string]bool{}
		for _, gf := range goFilesUnder(t, filepath.Join(root, filepath.FromSlash(cfg.scope))) {
			if strings.HasSuffix(gf.rel, "_test.go") {
				continue
			}
			rel := cfg.scope + "/" + gf.rel
			fset := token.NewFileSet()
			// Comments are NOT parsed: a guard that reads prose matches its own
			// explanation and the field names quoted in the doc comment beside the
			// struct, and three guards in this tree have passed with the checked thing
			// deleted for exactly that reason.
			f, err := parser.ParseFile(fset, rel, gf.body, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isNamedLit(lit, cfg.typeName) {
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
					if _, optional := reconcilerConfigOptional[field]; optional {
						continue
					}
					missing = append(missing, field)
				}
				if len(missing) > 0 {
					sort.Strings(missing)
					t.Errorf("%s:%d builds a %s without %v.\n"+
						"An omitted seam is zero-filled with no error anywhere, and the reconciler "+
						"then skips what it could not check IN SILENCE — no FACT, no counter, no "+
						"log, and a pass that returns nil. A skipped asset and an asset that "+
						"agreed become the same observable estate, on the last layer able to "+
						"notice a mis-booked position. That is #1063 exactly. Name the field; "+
						"assigning nil is fine if it is deliberate, because then a reviewer sees "+
						"it and can ask why.",
						rel, fset.Position(lit.Pos()).Line, cfg.typeName, missing)
				}
				return true
			})
		}

		// NON-VACUITY: this venue's connector must have been found. Zero means the
		// construction shape moved and this guard now watches less than it thinks.
		if len(seen) < 1 {
			t.Fatalf("found no %s literal under %s — the construction shape moved and this "+
				"guard is asserting nothing", cfg.typeName, cfg.scope)
		}
		total += len(seen)

		// DEAD-ENTRY ARM: an optional-field entry naming a field that no longer
		// exists would wave through a future field that reused the name.
		have := map[string]bool{}
		for _, f := range want {
			have[f] = true
		}
		for f, reason := range reconcilerConfigOptional {
			if !have[f] {
				t.Errorf("optional entry for %q (%s) matches no %s field — delete it",
					f, reason, cfg.typeName)
			}
		}
	}

	// NON-VACUITY across both venues: one reconciler wired and the other not is
	// precisely the state #1063 found, so a guard that saw only one would have
	// nothing to say about it.
	if total < 2 {
		t.Fatalf("found reconciler-config literals in %d file(s) — expected at least 2, one per "+
			"exchange connector", total)
	}
}

// isNamedLit reports whether lit constructs the named struct, either bare
// (inside the declaring package) or qualified.
func isNamedLit(lit *ast.CompositeLit, typeName string) bool {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		return t.Name == typeName
	case *ast.SelectorExpr:
		return t.Sel.Name == typeName
	}
	return false
}

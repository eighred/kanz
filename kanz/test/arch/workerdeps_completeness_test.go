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

// A COMPOSITION ROOT THAT BUILDS WorkerDeps MUST BUILD ALL OF IT (#418).
//
// execution.WorkerDeps is the whole set of collaborators a venue adapter's
// background workers need. Both venue mains construct one, and both omit
// `Balances` — the seam that compares Kanz's books against the exchange's actual
// balances.
//
// NOTHING ABOUT THAT FAILED. Go zero-fills an omitted field in a composite
// literal, so there was no compile error and no log line: reconcileBalances
// simply short-circuits on nil (binance_recon.go) and returns nil. The
// platform has therefore NEVER ONCE compared its books against an exchange —
// while infra/nats/tenancy.yaml and infra/kafka/topics-job.yaml both provision
// the subject that comparison would publish on.
//
// THE FAILURE IS INVISIBLE IN THE DIRECTION THAT MATTERS. accounting.balance
// .reconciled is emitted only on a DISCREPANCY, so a dashboard filtered on it
// shows zero events — which reads as "no discrepancies" and actually means "no
// comparison has ever run". That is CLAUDE.md's rule verbatim: "nothing
// configured" and "checked, and fine" must never look the same.
//
// WHY A GUARD AND NOT TWO EDITS. A per-root test only covers the roots somebody
// remembered; a third venue adapter would repeat the omission with nothing to
// stop it, exactly as the second one repeated the first. This is the same shape
// and the same argument as TestEveryAuthenticatorPopulatesTheWholePrincipal
// (#225), where deleting a field from the production arm failed nothing because
// that arm had no test.
//
// WHAT IT CHECKS: every keyed execution.WorkerDeps literal outside test files
// names every exported field of the struct.
//
// WHAT IT CANNOT CHECK: that the value is USEFUL. `Balances: nil` satisfies this
// guard — deliberately. The point is not to force a working implementation into
// existence; it is to make the absence a VISIBLE DECISION in a diff, with a
// reason beside it, rather than a field nobody typed.

// workerDepsType is the struct every literal below must fill.
const workerDepsType = "WorkerDeps"

// workerDepsDecl is where it is declared, module-relative.
const workerDepsDecl = "internal/execution/publisher.go"

// workerDepsScope is the tree searched for literals of it.
const workerDepsScope = "services"

// workerDepsOptional are fields a literal need not name, with the reason.
//
// EVERY ENTRY IS A DURATION WITH A DOCUMENTED DEFAULT, and that is the whole
// rule: `<=0 ⇒ default` means omitting the field and naming the default are the
// same behaviour, so the omission loses no capability and hides no decision.
// A SEAM IS NEVER OPTIONAL — a nil collaborator silently disables a capability,
// which is the defect this guard exists for.
var workerDepsOptional = map[string]string{
	"ReconcileInterval": "duration with a documented default (<=0 ⇒ default); omitting it is the same behaviour as naming the default",
	"TickerInterval":    "duration with a documented default (<=0 ⇒ default); omitting it is the same behaviour as naming the default",
	"CloseTimeout":      "duration with a documented default (<=0 ⇒ 1500ms); omitting it is the same behaviour as naming the default",
	"HealInterval":      "duration with a documented default (<=0 ⇒ 500ms); omitting it is the same behaviour as naming the default",
	"MarginInterval":    "duration with a documented default (<=0 ⇒ venuemargin.DefaultInterval); omitting it is the same behaviour as naming the default",
}

func TestEveryWorkerDepsLiteralNamesEveryCollaborator(t *testing.T) {
	root := moduleRoot(t)

	want := exportedStructFields(t, filepath.Join(root, filepath.FromSlash(workerDepsDecl)), workerDepsType)
	// NON-VACUITY: eleven fields today. A rename or a move that made this come
	// back empty would turn the guard into a no-op that passes.
	if len(want) < 8 {
		t.Fatalf("execution.%s has %d exported fields (%v) — expected at least 8. The declaration "+
			"moved or was renamed and this guard is asserting nothing", workerDepsType, len(want), want)
	}

	seen := map[string]bool{}
	for _, gf := range goFilesUnder(t, filepath.Join(root, filepath.FromSlash(workerDepsScope))) {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		rel := workerDepsScope + "/" + gf.rel
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, gf.body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isWorkerDepsLit(lit) {
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
				if _, optional := workerDepsOptional[field]; optional {
					continue
				}
				missing = append(missing, field)
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s:%d builds an execution.%s without %v.\n"+
					"An omitted collaborator is zero-filled with no error anywhere, and the worker "+
					"that needed it short-circuits on nil — the capability is simply absent, and "+
					"the absence looks identical to it being healthy. That is #418 exactly. Name "+
					"the field; assigning nil is fine if it is deliberate, because then a reviewer "+
					"sees it and can ask why.",
					rel, fset.Position(lit.Pos()).Line, workerDepsType, missing)
			}
			return true
		})
	}

	// NON-VACUITY: both venue adapters must have been found. Zero or one means
	// the construction shape moved and this guard now watches less than it thinks.
	if len(seen) < 2 {
		t.Fatalf("found execution.%s literals in %d file(s) (%v) — expected at least 2 (the "+
			"binance and okx composition roots). The construction shape moved and this guard is "+
			"asserting nothing", workerDepsType, len(seen), keysOf(seen))
	}

	// DEAD-ENTRY ARM: an optional-field entry naming a field that no longer
	// exists would wave through a future field that reused the name.
	have := map[string]bool{}
	for _, f := range want {
		have[f] = true
	}
	for f, reason := range workerDepsOptional {
		if !have[f] {
			t.Errorf("optional entry for %q (%s) matches no execution.%s field — delete it",
				f, reason, workerDepsType)
		}
	}
}

// isWorkerDepsLit reports whether lit constructs execution.WorkerDeps, named
// either bare (inside package execution) or qualified (everywhere else).
func isWorkerDepsLit(lit *ast.CompositeLit) bool {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		return t.Name == workerDepsType
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && pkg.Name == "execution" && t.Sel.Name == workerDepsType
	}
	return false
}

// exportedStructFields returns the exported field names of the named struct
// declared in path, in declaration order.
//
// Shared with the Principal completeness guard rather than copied: two guards
// asking the same question of two structs is one implementation, and a copied
// helper is how a fix stops spreading.
func exportedStructFields(t *testing.T, path, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typeName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fl := range st.Fields.List {
			for _, name := range fl.Names {
				if name.IsExported() {
					out = append(out, name.Name)
				}
			}
		}
		return false
	})
	return out
}

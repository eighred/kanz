package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY CONSUMER OF A compliance.Decision MUST NAME EVERY REFUSAL FLAG (#803).
//
// # The failure this was written for
//
// PreTradeGate refuses an order under five different "no rule was evaluated"
// flags: Ungoverned, Unpriced, Unvaluable, Unscoped and Unreadable. Each one
// names a DIFFERENT operator action — write a mandate, wire a price source, look
// at the order size, disambiguate the tenants, republish the mandate — and the
// whole family exists so that none of them is reported as a rule breach.
//
// Two consumers enumerated that family by hand, and BOTH were missing the same
// member:
//
//   - services/oms/internal/compliance/comp01.go handled four, then fell through
//     to breachFromResult. For an Unreadable decision the Result is nil, so the
//     defensive tail returned "MANDATE" / "mandate breach" — a rule breach that
//     never fired, sent to the submitting client on ORDER_REJECTED and filed in
//     the audit trail.
//   - services/optimization/internal/bridge/bridge.go handled the same four and
//     fell through to "pre-trade compliance breach".
//
// The cost is not that the order was admitted — it was refused, correctly. It is
// that the mandate stream is COMPACTED, so an undecodable mandate is the last
// message on that portfolio's subject and refuses every order for it
// indefinitely, while the desk reads "mandate breach" and reasonably concludes
// the mandate is working. An indefinite outage, mis-signposted.
//
// # Why a guard, and why THIS guard
//
// mandate_terminal_sentinels_test.go is the same idea one layer up and would not
// have caught this: it checks call sites of MandateRegistry.Mandate, and neither
// of these files is one. They consume the DECISION, not the lookup error.
//
// IT CHECKS PRESENCE, NOT CORRECT HANDLING, and that limit is deliberate — the
// same one mandate_terminal_sentinels_test.go states for itself. A file that
// names a flag and then does the wrong thing with it passes here; that half is
// covered by behaviour (comp01_unreadable_test.go, and gateReason's table in
// bridge_test.go, which asserts no refusal reaches the fall-through arm).
// Mutating a branch to `if false && d.Unreadable` leaves this guard green, and
// the behavioural tests red. What NO test can cover is the flag that does not
// exist yet and the consumer nobody remembered to open — presence is what an AST
// can see, and it fails the build at exactly that moment.
//
// The list of flags is not written here. It is derived from decide()'s own
// returns — every field set beside `Allowed: false` in a Decision literal — so a
// sixth refusal added to the gate is picked up automatically and fails the build
// at every consumer that was not opened. A hand-written list here would be a
// fourth copy of the thing that broke.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. Every paragraph
// above names these flags; the checks below run over parsed composite literals
// and selector expressions with comments not attached, so this text cannot
// satisfy or defeat them.
const (
	// complianceGateFile declares Decision and contains decide(), which is the
	// only place a refusal flag is set.
	complianceGateFile = "internal/compliance/gate.go"
	// compliancePkgImport is what a consumer outside the package imports.
	compliancePkgImport = "github.com/eighred/kanz/internal/compliance"
	// compliancePkgDir is the package itself, whose own files are consumers too.
	compliancePkgDir = "internal/compliance"
)

// decisionConsumerExempt names a file that reads SOME refusal flags and is
// permitted not to read all of them, with the issue that retires the entry.
//
// EMPTY, AND THAT IS THE POINT. A file that distinguishes "no mandate exists"
// from "the order could not be priced" and then lumps a third refusal in with
// rule breaches is not simplifying — it is telling an operator to go and find a
// rule that never ran. An entry here is a decision that some caller may report
// one class of unevaluated order as a breach, which has to be argued in writing
// before it is true in code.
var decisionConsumerExempt = map[string]string{}

func TestEveryDecisionConsumerHandlesEveryRefusal(t *testing.T) {
	root := moduleRoot(t)
	flags := refusalFlags(t, root)
	if len(flags) < 2 {
		t.Fatalf("derived %d refusal flags from %s — decide() no longer returns "+
			"Decision{Allowed: false, <Flag>: true} literals, so this guard is asserting nothing. "+
			"If the shape changed, teach refusalFlags the new one", len(flags), complianceGateFile)
	}

	files := decisionConsumerFiles(t, root)
	if len(files) == 0 {
		t.Fatalf("no file imports %s and reads a refusal flag — this guard found nothing to "+
			"check", compliancePkgImport)
	}

	checked := 0
	for _, path := range files {
		rel := relPath(root, path)
		if _, ok := decisionConsumerExempt[rel]; ok {
			continue
		}
		read := refusalFlagsRead(t, path, flags)
		if len(read) == 0 {
			continue // not a consumer of the family at all
		}
		checked++
		var missing []string
		for _, f := range flags {
			if !read[f] {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("%s distinguishes %d of the %d ways an order is refused without any rule "+
				"being evaluated, and does not name: %s.\n"+
				"Whatever this file does with the ones it knows about, the ones it does not fall "+
				"through to its generic arm — which reports an order NOTHING was checked against "+
				"as a rule breach, and sends a reviewer to look for the rule that fired. Name "+
				"them, or add this file to decisionConsumerExempt with the issue that argues why "+
				"one class of unevaluated order may be reported as a breach",
				rel, len(read), len(flags), strings.Join(missing, ", "))
		}
	}
	if checked == 0 {
		t.Fatal("no file was actually checked — every candidate read zero refusal flags, so this " +
			"guard passed without asserting anything")
	}
}

// refusalFlags reads decide()'s own returns: the field names set beside a
// literal `Allowed: false` in a Decision composite literal.
//
// A Decision returned with `Allowed: true` (Ungoverned under the permissive
// posture, Unconstrained) is NOT a refusal and is deliberately not collected — a
// consumer rendering a rejection reason has nothing to say about an admitted
// order. Neither is Unaccounted, which rides an Allowed the gate computes rather
// than a literal, because it does not change the verdict.
func refusalFlags(t *testing.T, root string) []string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(complianceGateFile))
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", complianceGateFile, err)
	}

	seen := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "Decision" {
			return true
		}
		fields := map[string]ast.Expr{}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok {
				fields[key.Name] = kv.Value
			}
		}
		allowed, ok := fields["Allowed"].(*ast.Ident)
		if !ok || allowed.Name != "false" {
			return true // admitted, or an Allowed the gate computes
		}
		for name, val := range fields {
			if name == "Allowed" || name == "Result" {
				continue
			}
			if id, ok := val.(*ast.Ident); ok && id.Name == "true" {
				seen[name] = true
			}
		}
		return true
	})

	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// decisionConsumerFiles lists non-test Go files that could hold a consumer: the
// compliance package's own, plus every file importing it.
//
// THE IMPORT IS THE FILTER, and it is what keeps this from firing on unrelated
// code. cmd/kanz-monitor aggregates counters whose FIELDS are called Ungoverned
// and Unpriced; it does not import this package and is not a Decision consumer,
// so it is excluded by construction rather than by an exemption somebody would
// have to justify.
func decisionConsumerFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", ".claude", "node_modules", "gen", ".gotmp", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel := relPath(root, path)
		if strings.HasPrefix(rel, compliancePkgDir+"/") {
			out = append(out, path)
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(src), `"`+compliancePkgImport+`"`) {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// refusalFlagsRead reports which refusal flags this file reads AS A FIELD of
// something — a selector, x.Unreadable — so a local variable or a struct field
// that merely shares the name (monitor's warnedUngoverned) is not mistaken for
// one, and neither is the prose in this guard.
func refusalFlagsRead(t *testing.T, path string, flags []string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	want := map[string]bool{}
	for _, f := range flags {
		want[f] = true
	}
	read := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && want[sel.Sel.Name] {
			read[sel.Sel.Name] = true
		}
		return true
	})
	return read
}

// relPath is the module-relative, slash-separated form used in messages and in
// the exemption map, so an entry reads the same on every platform.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

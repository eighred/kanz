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

// A RULE THAT READS THE BOOK MUST FIRST REFUSE A BOOK IT CANNOT PRICE (#760).
//
// # The failure this was written for
//
// Every COMP-01 rule that reasons about the shape of the book funnels through
// heldPositions, and heldPositions dropped any position whose MarketValue was
// absent — an unmarked holding was removed from the book before any rule saw it.
// A single filter produced three different wrong answers:
//
//   - the holding escaped limits written about it (a cap on the instrument passed,
//     because the instrument was not in the book);
//   - every other bucket was measured against a denominator that had lost it, so a
//     violation could carry an `observed` weight nobody measured;
//   - gross leverage was UNDERSTATED, the fail-open direction, because gross is a
//     sum and the sum lost a term.
//
// unmarkedHoldings is the refusal. This guard is about the NEXT rule: the hole
// was never in any one rule's logic, it was in what every rule shares, so a
// sixth rule added later inherits the bug by doing nothing wrong.
//
// # What this checks
//
// In internal/compliance/rules.go, every exported `*Rule` function that CALLS
// one of the book-reading helpers must also call unmarkedHoldings. Both halves
// are read from the AST, not grepped: this file's own prose names every one of
// those helpers, and a guard that matched raw source would match itself.
//
// It deliberately does NOT check call ORDER, which matters — the refusal has to
// precede ConcentrationRule's empty-book branch, because a book whose every
// holding is unmarked totals zero and is otherwise indistinguishable from an
// empty one. Order is a behavioural property and is pinned where behaviour
// belongs: TestConcentration_ABookOfOnlyUnmarkedPositionsIsNotAnEmptyBook in
// internal/compliance. A structural guard asserting statement order would break
// on any harmless reshuffle while proving less.
const rulesFile = "internal/compliance/rules.go"

// bookReadingHelpers are the calls that mean "this rule reasons about holdings".
var bookReadingHelpers = []string{"heldPositions", "totalGross", "grossByDimension"}

// unmarkedRefusal is the call that makes such a rule safe.
const unmarkedRefusal = "unmarkedHoldings"

// unmarkedRefusalExempt names a book-reading rule permitted not to refuse, and
// the issue that retires the entry.
//
// EMPTY, AND THAT IS THE POINT. A rule that reads holdings and does not first
// establish it can price them is answering a question it cannot answer. An entry
// here is a decision that some mandate rule may be evaluated over a book with
// unknown contents, which has to be argued in writing before it is true in code.
var unmarkedRefusalExempt = map[string]string{}

func TestEveryBookReadingRuleRefusesAnUnpricedBook(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(rulesFile))
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rulesFile, err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Base(rulesFile), src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rulesFile, err)
	}

	reading, guarded := map[string]string{}, map[string]bool{}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !strings.HasSuffix(fn.Name.Name, "Rule") || !fn.Name.IsExported() {
			continue
		}
		called := plainCallsIn(fn)
		for _, h := range bookReadingHelpers {
			if called[h] {
				reading[fn.Name.Name] = h
				break
			}
		}
		if called[unmarkedRefusal] {
			guarded[fn.Name.Name] = true
		}
	}

	// NON-VACUITY. Five rules read the book today. A parse that found none — the
	// file renamed, the helpers renamed, the walk broken — would report a clean
	// estate having checked nothing, which is this guard's own failure mode.
	if len(reading) < 5 {
		t.Fatalf("found %d book-reading rule(s) in %s (%v) — Concentration, Restriction, "+
			"IssuerExclusion, Leverage and Currency all read holdings, so the parse is broken "+
			"rather than the estate", len(reading), rulesFile, sortedNames(reading))
	}

	var problems []string
	seenExempt := map[string]bool{}
	for _, name := range sortedNames(reading) {
		if guarded[name] {
			continue
		}
		if reason, ok := unmarkedRefusalExempt[name]; ok {
			seenExempt[name] = true
			t.Logf("%s: reads the book without refusing an unpriced one, tracked — %s", name, reason)
			continue
		}
		problems = append(problems, name+" calls "+reading[name]+" but never calls "+unmarkedRefusal+
			" — it would evaluate a mandate rule over a book whose contents it could not price, and "+
			"an unmarked holding is UNKNOWN, never zero")
	}
	// DEAD-ENTRY CHECK: an exemption matching nothing has outlived its repair and
	// must not sit here implying a hole that was already closed.
	for name := range unmarkedRefusalExempt {
		if !seenExempt[name] {
			problems = append(problems, "exemption for "+name+" matches nothing — delete the entry")
		}
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// plainCallsIn returns the names of every unqualified function CALLED in fn —
// `heldPositions(c)`, not `pkg.heldPositions(c)`. These helpers are
// package-local, so a selector call would be a different function entirely.
func plainCallsIn(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			out[id.Name] = true
		}
		return true
	})
	return out
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

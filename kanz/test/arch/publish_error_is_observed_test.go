package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// A PUBLISH ERROR MAY NOT BE ASSIGNED TO THE BLANK IDENTIFIER (#673).
//
// # What went wrong without it
//
// Both crypto venue adapters' ticker feeds ended their poll with
// `_ = pub.Publish(ctx, bus.Event{…})`. Each had a *slog.Logger in scope and
// used it on every other failure path in the same file; neither used it here,
// and there was no counter. So a subject the broker refused on every tick
// produced the same observable estate as an exchange with nothing to report —
// no line, no series, no difference. That is CLAUDE.md's rule verbatim:
// "nothing configured" and "checked, and fine" must never look the same.
//
// The CONSEQUENCE was contained, and the guard is not claiming otherwise. The
// mark Source expires an entry past maxAge rather than serving a stale price,
// and OMSPriceFeedStalled fires on a fold that held marks and holds no live one.
// What was missing is DIAGNOSIS: "the venue went quiet", "the connection
// dropped" and "the broker refused the envelope" arrive as one stalled feed and
// are three different repairs, and the discarded error was the only value that
// told them apart.
//
// And it was the SAME CODE IN TWO SERVICES, which is why a guard beats two
// edits. The second connector was written by copying the first; a third would
// have copied it again, and a repair to either would have left the other alone.
//
// # What this checks
//
// Every non-test .go file in the module, for an assignment whose left-hand side
// is entirely blank identifiers and whose right-hand side is a call to something
// named Publish. Default-deny: there are no exemptions today, and an entry added
// to publishErrorDiscardExemptions must name the issue that retires it.
//
// # What it cannot check
//
// It is syntactic, by identifier. A publish reached through a differently-named
// wrapper, or an error captured into a variable and then never read, both pass —
// the first is not a Publish by name and the second is a job for the compiler's
// unused-variable rule and for `errcheck` in golangci-lint. This guard covers
// the one spelling that BOTH of those miss and that shipped twice: an
// explicitly, deliberately discarded error, which reads to a reviewer as a
// considered decision precisely because somebody typed the underscore.
//
// It also says nothing about whether the handling that replaced the discard is
// GOOD. execution.MarkTickPublisher's own tests carry that half: that the
// observer fires on every drop, that the WARN carries the broker's reason, and
// that the latch clears so a second outage is audible.
func TestNoPublishErrorIsDiscarded(t *testing.T) {
	root := moduleRoot(t)

	scanned := 0
	seenExempt := map[string]bool{}
	var problems []string
	for _, gf := range goFilesUnder(t, root) {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		scanned++
		for _, line := range discardedPublishLines(t, gf.rel, gf.body) {
			if _, exempt := publishErrorDiscardExemptions[gf.rel]; exempt {
				seenExempt[gf.rel] = true
				continue
			}
			problems = append(problems, fmt.Sprintf("%s:%d", gf.rel, line))
		}
	}

	// NON-VACUITY, ARM 1: the walk found the module. A scan that reads nothing
	// reports nothing and protects nothing.
	if scanned < 500 {
		t.Fatalf("scanned only %d non-test .go files — the walk is broken, not the module "+
			"(there were over 900 when this guard was written)", scanned)
	}
	// NON-VACUITY, ARM 2: the detector still fires. With zero real sites left,
	// nothing else in this test would notice if the AST match stopped matching.
	if !detectsDiscardedPublish(t) {
		t.Fatal("the detector no longer flags a synthetic `_ = p.Publish(ctx, e)` — this guard is " +
			"passing because it sees nothing, not because there is nothing to see")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("a publish error is discarded at:\n  %s\n\n"+
			"A discarded publish error makes a refused subject indistinguishable from a source with "+
			"nothing to say — the failure has no log line, no counter and no downstream difference "+
			"until something far away notices data that stopped arriving. Handle it where it "+
			"happens: count every occurrence and log the REASON rate-limited, the way "+
			"execution.MarkTickPublisher does for the venue mark feeds. If a site genuinely must "+
			"drop it, add the file to publishErrorDiscardExemptions with the issue that retires it.",
			strings.Join(problems, "\n  "))
	}

	// DEAD-ENTRY ARM: an exemption whose site has been repaired would wave
	// through the next discard added to the same file.
	for file, reason := range publishErrorDiscardExemptions {
		if !seenExempt[file] {
			t.Errorf("publishErrorDiscardExemptions names %q (%s) but that file discards no publish "+
				"error any more — the repair happened, delete the entry", file, reason)
		}
	}
}

// publishErrorDiscardExemptions maps a module-relative file to the reason it may
// discard a publish error. EMPTY, and that is the intended steady state: this
// estate has never had a site where losing a publish silently was the right
// answer. Any entry must name the issue that retires it.
var publishErrorDiscardExemptions = map[string]string{}

// discardedPublishLines returns the lines in src holding an assignment that
// throws away the result of a call to something named Publish.
func discardedPublishLines(t *testing.T, name, src string) []int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var lines []int
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || !allBlank(as.Lhs) || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || !callsPublish(call) {
			return true
		}
		lines = append(lines, fset.Position(as.Pos()).Line)
		return true
	})
	return lines
}

// allBlank reports whether every expression is the blank identifier. `_, _ =`
// counts: two blanks discard just as thoroughly as one.
func allBlank(exprs []ast.Expr) bool {
	if len(exprs) == 0 {
		return false
	}
	for _, e := range exprs {
		id, ok := e.(*ast.Ident)
		if !ok || id.Name != "_" {
			return false
		}
	}
	return true
}

// callsPublish reports whether call invokes something named exactly Publish,
// qualified (x.Publish) or bare (Publish). EXACTLY, not by prefix: PublishDLQ
// and PublishTrade are different functions with their own error contracts, and a
// prefix match would silently widen this rule to them.
func callsPublish(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name == "Publish"
	case *ast.Ident:
		return fn.Name == "Publish"
	}
	return false
}

// detectsDiscardedPublish runs the detector over a synthetic file containing the
// exact defect #673 repaired, plus the shapes that must NOT trip it. It is the
// proof that a clean module means a clean module.
func detectsDiscardedPublish(t *testing.T) bool {
	t.Helper()
	const src = `package fixture

type ev struct{}
type p struct{}

func (x *p) Publish(_ any, _ ev) error { return nil }
func (x *p) PublishTrade(_ any, _ ev)  {}

// The defect: the error is thrown away on purpose.
func discards(x *p) { _ = x.Publish(nil, ev{}) }
`
	if len(discardedPublishLines(t, "fixture.go", src)) != 1 {
		return false
	}

	// And the shapes that must stay clean, so a future widening of the matcher
	// cannot start failing correct code without failing here first.
	const clean = `package fixture

type ev struct{}
type p struct{}

func (x *p) Publish(_ any, _ ev) error { return nil }
func (x *p) PublishTrade(_ any, _ ev)  {}

func handles(x *p) error   { return x.Publish(nil, ev{}) }
func delegates(x *p)       { x.PublishTrade(nil, ev{}) }
func keeps(x *p) error     { err := x.Publish(nil, ev{}); return err }
`
	return len(discardedPublishLines(t, "clean.go", clean)) == 0
}

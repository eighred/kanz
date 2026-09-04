package arch

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// A CONTROL ON HOLDING TOO MUCH CASH MUST NEVER BLOCK SPENDING IT (#963).
//
// compliance.CashBufferRule is the ceiling half of the cash policy: the
// portfolio's uninvested cash must not exceed what its mandate permits. Every
// other rule in that registry bounds something an order can make WORSE, so
// refusing the order is the remedy. This one is the opposite — the remedy for
// excess cash is to BUY something — and that inversion is the whole hazard:
//
//   - A BUY out of an over-cash portfolio is the order that discharges the
//     breach. project() carries the pre-existing excess into the candidate book,
//     so a ceiling on the admission path denies the fix for the condition it is
//     complaining about.
//   - A SELL raises cash, so it breaches a ceiling BY DEFINITION. A gate
//     enforcing one would refuse every liquidation a cash-rich portfolio
//     attempts, which traps the fund in a position at the moment it most needs to
//     leave. That is the de-risking trap VenueMarginRule opens an explicit
//     exemption for; here it is not an edge case, it is the default.
//
// The rule's own tests hold the branches that exist TODAY. What they cannot hold
// is the branch somebody adds tomorrow: a new refusal written above the
// order-path exemption is armed on the pre-trade gate immediately, and its
// symptom is orders refused for a reason no unit test in the package is looking
// for. That ordering is what this guard reads.
//
// # Why the source and not the behaviour
//
// A behavioural guard would have to enumerate the refusal cases, which is the
// hand-maintained list that misses the new member — the defect #806 and #803 both
// were. The positional property is decidable from the function itself and covers
// every branch, including ones nobody has written yet.

const cashBufferRuleRel = "../../internal/compliance/cashbuffer.go"

// TestTheIdleCashCeilingLeavesTheOrderPathBeforeAnyIdleCashRefusal is the rule.
//
// paramsMismatch is the ONE refusal permitted above the exemption, and it is not
// an exception to the argument: params that do not match the declared type is a
// mandate the engine cannot read, not a statement about cash, and a portfolio
// governed by an unreadable mandate is refused everywhere.
func TestTheIdleCashCeilingLeavesTheOrderPathBeforeAnyIdleCashRefusal(t *testing.T) {
	body := commentFreeFuncBody(t, cashBufferRuleRel, "CashBufferRule")

	exempt := strings.Index(body, "c.Order != nil")
	if exempt < 0 {
		t.Fatal("CashBufferRule no longer exempts the order path. An idle-cash ceiling enforced at " +
			"the pre-trade gate refuses the BUY that discharges the excess and refuses every SELL " +
			"for raising cash — a fund that cannot liquidate because it holds too much cash (#963).")
	}
	// The exemption has to be a PASS. A `c.Order != nil` that fell through to a
	// violation would satisfy a naive index check while doing the opposite.
	tail := body[exempt:]
	if ret := strings.Index(tail, "return"); ret < 0 || !strings.HasPrefix(strings.TrimSpace(tail[ret:]), "return nil") {
		t.Fatal("the c.Order branch in CashBufferRule does not return nil. The exemption exists to " +
			"ADMIT an order untouched; a branch that refuses there is the gate enforcement this " +
			"rule must not do (#963).")
	}

	for _, refusal := range []string{"&compliancepb.Violation{", "unvouchedCeiling("} {
		if at := strings.Index(body, refusal); at >= 0 && at < exempt {
			t.Fatalf("CashBufferRule can return %s BEFORE it exempts the order path.\n\n"+
				"That branch is armed on the OMS pre-trade gate, where this rule has no verdict "+
				"to offer: it would refuse an order over a condition the order itself is the "+
				"remedy for. Every idle-cash refusal belongs below the `if c.Order != nil` "+
				"return; the only thing permitted above it is paramsMismatch, which is about an "+
				"unreadable mandate rather than about cash (#963).", refusal)
		}
	}
}

// TestTheIdleCashCeilingIsNotReachableFromThePreTradeGate is the second half:
// the exemption is only worth anything while the gate actually SETS Candidate.Order.
//
// THE DISCRIMINATOR IS ONE FIELD IN ANOTHER FILE, and nothing in cashbuffer.go
// can see it. If a future gate change built a Candidate without an Order — for an
// order with no venue, say, or on some new admission path — this rule would
// silently start refusing orders, and the failure would look like a compliance
// breach rather than like a missing field.
func TestTheIdleCashCeilingIsNotReachableFromThePreTradeGate(t *testing.T) {
	const gateRel = "../../internal/compliance/gate.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, gateRel, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", gateRel, err)
	}

	// EVERY Candidate LITERAL IN THE FILE, not the one in the function this guard
	// was written against. A second admission path added beside decide() is
	// exactly how the discriminator would come to be set on one route and not the
	// other, and naming the function would make this guard blind to it.
	found := 0
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		id, ok := lit.Type.(*ast.Ident)
		if !ok || id.Name != "Candidate" {
			return true
		}
		found++
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Order" {
				return true
			}
		}
		t.Errorf("the pre-trade gate builds a compliance.Candidate with no Order field at %s.\n\n"+
			"CashBufferRule uses Order != nil as its ONLY signal that it is looking at an "+
			"admission decision rather than a passive book, so an order-less candidate arms the "+
			"idle-cash ceiling on the order path — refusing the BUY that discharges the excess, "+
			"and refusing every SELL for raising cash (#963).", fset.Position(lit.Pos()))
		return true
	})
	if found == 0 {
		t.Fatal("found NO compliance.Candidate literal in gate.go — this guard passed vacuously. " +
			"Either the gate stopped building candidates here, or the type was renamed.")
	}
}

// commentFreeFuncBody renders one function's body as source with COMMENTS REMOVED — the
// parser is given no comment map, so the printer cannot reproduce them.
//
// THAT IS THE POINT, not a convenience. A guard that greps raw source matches its
// own prose and the doc comments of the code it reads: three guards in this
// directory once passed with the checked thing deleted, because the sentence
// describing it was still in the file. Here the whole hazard is where a phrase
// appears RELATIVE to another, and a comment mentioning `c.Order != nil` above
// the exemption would move the index and pass a rule that had been reordered.
func commentFreeFuncBody(t *testing.T, path, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, fn.Body); err != nil {
			t.Fatalf("print %s: %v", name, err)
		}
		return buf.String()
	}
	t.Fatalf("no func %s in %s — it was renamed or removed, and this guard is checking nothing", name, path)
	return ""
}

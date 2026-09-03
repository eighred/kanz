package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// THE REPLAY BACKSTOP IS A CHAIN OF PURE FUNCTIONS, AND IT MUST STAY ONE (#820).
//
// # What this is protecting
//
// webhook-ingest is the one service the internet talks to. Its replay defence is a
// nonce store, and in the vendor-free build that store is per-pod — so a
// re-delivered TradingView alert landing on a second replica is admitted twice and
// fans out a second set of orders.
//
// The second fan-out does not reach a venue, and the reason is entirely
// arithmetic. Every identifier on the path is a pure function of (strategy, nonce):
//
//	signal_id  = DeterministicID(strategy_id, nonce)      pipeline.go
//	order_id   = DeterministicID(signal_id, venue)        translate.go
//	SubmitOrder.IdempotencyKey = order_id                 translate.go
//
// so a redelivery re-derives the SAME ids. The bus deduper collapses the re-fanned
// command on the idempotency key, and anything that does land meets the OMS
// admission gate — `ON CONFLICT (tenant_id, order_id) DO NOTHING`, whose zero
// RowsAffected returns ErrExists, acks the delivery and stops it BEFORE it routes.
//
// # Why a guard
//
// Three comments in webhook-ingest asserted the OPPOSITE for long enough to be
// found by an audit: "a fresh claim mints a fresh signal_id, so the OMS's admission
// gate sees two different orders, not a duplicate." That was false against the code
// it sat beside, and its cost is a mis-sized security control — an engineer reads
// the consequence of a cross-pod miss as "duplicate orders at a live exchange",
// when it is narrower. A control kept for a wrong reason is as badly decided as one
// dropped for a wrong reason.
//
// #820 corrected those three comments. Correcting them is what CREATES the need for
// this guard: the new text tells the reader the backstop holds, so the backstop has
// to actually hold, and the day any id on the path mixes in a clock, a UUID or a
// retry counter, the perimeter is all that is left and nothing would say so. This
// guard is the difference between a corrected comment and a comment that is merely
// wrong more recently.
//
// It reads the parsed tree, never the file text, so it cannot match the very
// comments it exists to keep honest.

// backstopFile parses one non-test file under the module root.
func backstopFile(t *testing.T, rel string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(moduleRoot(t), filepath.FromSlash(rel)), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v\n\nThis guard reads the replay backstop's source directly. If the "+
			"file moved, move the guard with it rather than deleting it (#820).", rel, err)
	}
	return fset, f
}

// derivedFrom reports whether e is a call to DeterministicID (bare or through a
// package qualifier) whose arguments end in the named selectors, in order.
func derivedFrom(e ast.Expr, args ...string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if fn.Name != "DeterministicID" {
			return false
		}
	case *ast.SelectorExpr:
		if fn.Sel.Name != "DeterministicID" {
			return false
		}
	default:
		return false
	}
	if len(call.Args) != len(args) {
		return false
	}
	for i, want := range args {
		sel, ok := call.Args[i].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != want {
			return false
		}
	}
	return true
}

// DeterministicID IS PURE, so a redelivery re-derives the same ids.
//
// The whole backstop rests on this one function. A clock, a random source or a
// counter inside it would make every downstream collapse silently stop collapsing:
// each redelivery would mint a new order_id, the OMS would admit it as a genuinely
// new order, and the fund would trade twice — which is exactly the consequence the
// three corrected comments wrongly claimed was already happening.
func TestDeterministicIDIsPure(t *testing.T) {
	fset, f := backstopFile(t, "internal/signal/translate/translate.go")

	var fd *ast.FuncDecl
	for _, d := range f.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == "DeterministicID" && d.Recv == nil {
			fd = d
		}
	}
	if fd == nil {
		t.Fatal("internal/signal/translate declares no DeterministicID — the replay backstop's one " +
			"pure function is gone, and every id derived from it in this guard is now derived from " +
			"something unexamined (#820)")
	}

	// Anything that varies between two calls with equal arguments.
	impure := map[string]string{
		"time":     "a clock",
		"rand":     "a random source",
		"uuid":     "a generated id",
		"ulid":     "a generated id",
		"nanoid":   "a generated id",
		"atomic":   "a counter",
		"os":       "process state",
		"runtime":  "process state",
		"maphash":  "a per-process seed",
		"crypto":   "a random source", // crypto/rand; the hash here is stdlib sha256
		"sequence": "a counter",
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if why, bad := impure[id.Name]; bad {
			t.Errorf("%s: DeterministicID reads %s (%s.%s).\n\n"+
				"It must be a pure function of its parts. Every replay defence downstream of "+
				"webhook-ingest is arithmetic on this: signal_id = f(strategy, nonce), order_id = "+
				"f(signal_id, venue), IdempotencyKey = order_id. Introduce anything that differs "+
				"between two calls with equal arguments and a re-delivered alert mints a NEW "+
				"order_id, the OMS admits it as a new order, and the fund trades twice against a "+
				"live exchange (#820).", fset.Position(sel.Pos()), why, id.Name, sel.Sel.Name)
		}
		return true
	})
}

// THE ID CHAIN IS DERIVED, NOT MINTED, AT EVERY HOP.
func TestTheReplayIDChainIsDerived(t *testing.T) {
	// 1. signal_id comes from (strategy, nonce) — the two things a replayed alert
	//    repeats verbatim. This is the hop the corrected comments describe.
	fset, f := backstopFile(t, "services/webhook-ingest/internal/ingest/pipeline.go")
	if !fieldDerivedFrom(f, "SignalID", "StrategyID", "Nonce") {
		t.Errorf("%s: pipeline.go does not derive SignalID from (StrategyID, Nonce).\n\n"+
			"A signal id minted from anything else — a clock, a UUID, the delivery — makes every "+
			"redelivery a distinct order all the way to the venue, and the nonce store stops being "+
			"a second line of defence and becomes the only one (#820).",
			fset.Position(f.Pos()))
	}

	// 2. order_id comes from (signal_id, venue), and 3. the command's idempotency
	//    key IS that order_id — so the bus deduper and the OMS admission gate
	//    collapse on the same value.
	fset2, f2 := backstopFile(t, "internal/signal/translate/translate.go")
	if !assignDerivedFrom(f2, "orderID", "SignalID", "Venue") {
		t.Errorf("%s: translate.go does not derive orderID from (SignalID, Venue).\n\n"+
			"The OMS admission gate is keyed on (tenant_id, order_id). If the order id stops being "+
			"a function of the signal id, ON CONFLICT DO NOTHING has nothing to conflict with and "+
			"a re-delivered alert is admitted as a new order (#820).", fset2.Position(f2.Pos()))
	}
	if !fieldIsIdent(f2, "IdempotencyKey", "orderID") {
		t.Errorf("%s: SubmitOrder.IdempotencyKey is not the order id.\n\n"+
			"The bus deduper collapses a re-fanned command on this key. Setting it to anything "+
			"freshly generated moves the entire burden onto the OMS insert, and moves it silently "+
			"(#820).", fset2.Position(f2.Pos()))
	}
}

// fieldDerivedFrom reports whether some composite-literal field `name:` is
// assigned DeterministicID(...args).
func fieldDerivedFrom(f *ast.File, name string, args ...string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == name && derivedFrom(kv.Value, args...) {
			found = true
		}
		return !found
	})
	return found
}

// fieldIsIdent reports whether some composite-literal field `name:` is assigned
// the plain identifier want.
func fieldIsIdent(f *ast.File, name, want string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		k, ok := kv.Key.(*ast.Ident)
		if !ok || k.Name != name {
			return true
		}
		if v, ok := kv.Value.(*ast.Ident); ok && v.Name == want {
			found = true
		}
		return !found
	})
	return found
}

// assignDerivedFrom reports whether `name := DeterministicID(...args)` appears.
func assignDerivedFrom(f *ast.File, name string, args ...string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name == name && derivedFrom(as.Rhs[0], args...) {
			found = true
		}
		return !found
	})
	return found
}

// THE OMS ADMISSION GATE IS THE SECOND LINE, AND IT REFUSES RATHER THAN IGNORES.
//
// Two halves, and the second is the one that is easy to lose. `ON CONFLICT DO
// NOTHING` alone would make a duplicate INSERT succeed silently — the handler would
// carry on and route the order to a venue anyway. What makes it a refusal is that
// zero RowsAffected is turned into ErrExists, which the admission path treats as
// "another delivery owns this order": ack, announce nothing, stop before routing.
//
// The SQL is read from the string literal in the parsed tree, so this guard cannot
// be satisfied by a comment quoting the clause.
func TestTheOMSAdmissionGateRefusesADuplicate(t *testing.T) {
	fset, f := backstopFile(t, "services/oms/internal/order/postgres.go")

	sawClause := false
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		sql := strings.Join(strings.Fields(lit.Value), " ")
		if strings.Contains(sql, "INSERT INTO orders") &&
			strings.Contains(sql, "ON CONFLICT (tenant_id, order_id) DO NOTHING") {
			sawClause = true
		}
		return true
	})
	if !sawClause {
		t.Errorf("%s: the orders INSERT does not carry ON CONFLICT (tenant_id, order_id) DO "+
			"NOTHING.\n\nThis is the second line of defence behind webhook-ingest's nonce store, "+
			"and three comments in that service now tell the reader it is here. Without it a "+
			"re-delivered alert that got past a per-pod nonce cache is admitted as a new order "+
			"(#820).", fset.Position(f.Pos()))
	}

	// The refusal itself: RowsAffected() == 0 must produce ErrExists.
	refuses := false
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Cond == nil {
			return true
		}
		be, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok || be.Op != token.EQL {
			return true
		}
		call, ok := be.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RowsAffected" {
			return true
		}
		if lit, ok := be.Y.(*ast.BasicLit); !ok || lit.Value != "0" {
			return true
		}
		ast.Inspect(ifs.Body, func(b ast.Node) bool {
			if id, ok := b.(*ast.Ident); ok && id.Name == "ErrExists" {
				refuses = true
			}
			return true
		})
		return true
	})
	if !refuses {
		t.Errorf("%s: a conflicting INSERT is not turned into ErrExists.\n\n"+
			"DO NOTHING on its own is not a refusal — the INSERT succeeds, the handler carries on, "+
			"and the duplicate is routed to a venue. Zero RowsAffected must become ErrExists, which "+
			"is what makes the losing delivery ack, announce nothing and stop before it routes "+
			"(#820).", fset.Position(f.Pos()))
	}
}

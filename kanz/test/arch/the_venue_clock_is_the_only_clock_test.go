package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE VENUE'S CLOCK IS THE ONLY CLOCK ON THE DEPTH PATH (#957).
//
// # What went wrong without it
//
// Book.Snapshot carried a fallback for a book whose event time was unknown:
//
//	et := b.eventTime
//	if et.IsZero() {
//	    et = time.Now().UTC()
//	}
//
// It could not fire for the case it was written for. The folds assigned
// `b.eventTime = s.GetEventTime().AsTime()`, and a NIL google.protobuf.Timestamp
// reads back through AsTime() as time.Unix(0, 0) — the UNIX EPOCH — not as a zero
// time.Time. IsZero() was therefore false, the branch was skipped, and a source
// that omitted event_time stamped every book 1970-01-01.
//
// The number was not the damage. The epoch propagated to market.crypto.quote and
// internal/marketdata/mark refused a 56-year-old observation, so the fold held no
// width rather than a fabricated fresh one — safe. What was not safe was the
// diagnosis: market-ingest folded perfectly, published perfectly and reported
// ready while every consumer silently discarded its output, with no error
// anywhere naming the cause.
//
// # Why a guard rather than a paragraph
//
// Every part of this is invisible to the compiler, to `go vet` and to the test
// suite, because nothing was wrong with the code as written — the defect was that
// a guard's premise was false. Three shapes have to hold together, and each can
// be reintroduced on its own:
//
//   - a source that does not stamp event_time (the trap the issue was filed for:
//     neither shipped adapter reaches it, so no test observes the regression);
//   - a fold that accepts an update without one (which is what turns a missing
//     stamp into a book carrying a time nobody asserted);
//   - a clock substituted anywhere on the path (which converts "we do not know
//     when the venue said this" into "we know it now" — the same quiet confidence
//     the staleness bound exists to prevent).
//
// # Why these are AST guards and not greps
//
// Because `time.Now()` still appears in book.go — four times, in the comments
// explaining why it is gone. A guard that grepped the source for it would match
// its own rationale and pass with the substitution restored, which is the exact
// failure recorded against three earlier guards in this directory. Everything
// below reads the parsed tree, where a comment is not a node.

// marketedgeFile parses one non-test file under internal/marketedge.
func marketedgeFile(t *testing.T, rel string) (*token.FileSet, *ast.File) {
	t.Helper()
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(rel))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v\n\nThis guard reads the depth path's source directly. If the file "+
			"moved, move the guard with it rather than deleting it (#957).", rel, err)
	}
	return fset, f
}

// callsPkgFunc reports whether the subtree contains a call to `pkg.name(...)`.
// (test/arch already has a callsSelector; it matches on the method name alone.)
func callsPkgFunc(n ast.Node, pkg, name string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if c, ok := x.(*ast.CallExpr); ok && isSelector(c.Fun, pkg, name) {
			found = true
		}
		return !found
	})
	return found
}

// funcNamed returns the top-level func or method declaration called name.
func funcNamed(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

// EVERY DEPTH SOURCE STAMPS THE VENUE'S TIME, ON SNAPSHOTS AND ON DELTAS.
//
// This is the trap the issue was actually filed for. The three shipped sources
// all stamp it, which is precisely why nothing observes a source that does not —
// the omission is a compile-clean, vet-clean, test-clean way to take an
// instrument dark. Default-deny over the constructors themselves, so a fourth
// source is caught when it is written rather than when someone notices the marks
// stopped moving.
//
// No exemptions, and the population says why: 6 constructors across binance, okx
// and the sim source, all compliant.
func TestEveryDepthUpdateSetsAVenueTime(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "internal", "marketedge", "depth")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read depth package: %v", err)
	}

	const floor = 4 // two sources' worth; measured 6. Guards against a silent parse-nothing.
	checked := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset, f := marketedgeFile(t, "internal/marketedge/depth/"+name)

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			var kind string
			switch {
			case isSelector(lit.Type, "marketpb", "OrderBookSnapshot"):
				kind = "OrderBookSnapshot"
			case isSelector(lit.Type, "marketpb", "OrderBookDelta"):
				kind = "OrderBookDelta"
			default:
				return true
			}
			checked++

			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "EventTime" {
					return true
				}
			}
			t.Errorf("%s: %s is built without EventTime.\n\n"+
				"A depth update that does not carry the venue's own clock cannot be folded: since #957 "+
				"both folds REFUSE it, so this source publishes no quote and no book snapshot for the "+
				"instrument at all. Before #957 it was worse — a nil timestamp reads back as the UNIX "+
				"EPOCH, not as a zero time, so the book was silently stamped 1970 and every downstream "+
				"consumer discarded the output as stale with nothing anywhere naming the cause.\n\n"+
				"Stamp it from the venue's field, never from time.Now(): the value has to be when the "+
				"VENUE observed the state, not when this process received it.",
				fset.Position(lit.Pos()), kind)
			return true
		})
	}

	if checked < floor {
		t.Fatalf("found only %d depth update constructor(s), want at least %d — this guard is "+
			"vacuous.\n\nEither the constructors moved out of internal/marketedge/depth, or the "+
			"literals stopped being written as `&marketpb.OrderBook{Snapshot,Delta}{...}` and this "+
			"guard now walks past them silently (#957).", checked, floor)
	}
}

// BOTH FOLDS REFUSE AN UPDATE THEY CANNOT TIME.
//
// The check has to be on the PROTO (`GetEventTime() == nil`), not on the
// converted value, and that distinction is the whole issue: the pre-#957 code
// tested the converted value with IsZero() and could never be true, because nil
// converts to the epoch. A guard that only asserted "there is a check here" would
// pass on the broken version.
//
// ApplySnapshot must also RETURN the refusal. It previously returned nothing,
// which is what made the missing stamp unreportable at the call site.
func TestBothDepthFoldsRefuseAnUpdateWithNoVenueTime(t *testing.T) {
	fset, f := marketedgeFile(t, "internal/marketedge/book/book.go")

	for _, name := range []string{"ApplySnapshot", "ApplyDelta"} {
		fd := funcNamed(f, name)
		if fd == nil {
			t.Fatalf("book.go declares no %s — the fold this guard protects has moved or been "+
				"renamed (#957)", name)
		}

		// It must return an error, or the refusal cannot reach the caller.
		res := fd.Type.Results
		returnsError := false
		if res != nil {
			for _, fld := range res.List {
				if id, ok := fld.Type.(*ast.Ident); ok && id.Name == "error" {
					returnsError = true
				}
			}
		}
		if !returnsError {
			t.Errorf("%s: %s returns no error.\n\nA fold that cannot refuse can only accept, and an "+
				"update with no venue time then stamps the book with a value nobody asserted. "+
				"ApplySnapshot returned nothing before #957, which is why a source that omitted "+
				"event_time produced a market-ingest that folded and published perfectly while every "+
				"consumer discarded the result.", fset.Position(fd.Pos()), name)
			continue
		}

		// Somewhere in the body: `... .GetEventTime() == nil` guarding a return of
		// ErrNoVenueTime. Testing the PROTO is the point — a nil Timestamp converts
		// to the epoch, so the converted value can never be checked for absence.
		refuses := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok || ifs.Cond == nil {
				return true
			}
			checksProto := false
			ast.Inspect(ifs.Cond, func(c ast.Node) bool {
				be, ok := c.(*ast.BinaryExpr)
				if !ok || be.Op != token.EQL {
					return true
				}
				call, ok := be.X.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "GetEventTime" {
					return true
				}
				if id, ok := be.Y.(*ast.Ident); ok && id.Name == "nil" {
					checksProto = true
				}
				return true
			})
			// A CONSTANT IN THE CONDITION IS A DISABLED REFUSAL. `if false &&
			// d.GetEventTime() == nil` keeps every symbol this guard reads and
			// refuses nothing — the same shape that let `len(res.Unpinned) > 0 &&
			// false` slip past the #771 guard, found here the same way, by
			// mutation. Surfaced as a MISS on the first harness run.
			if checksProto && exprHasBoolConst(ifs.Cond) {
				t.Errorf("%s: %s's refusal is short-circuited by a constant in its condition.\n\n"+
					"Every symbol a guard looks for is still present, so the refusal reads as wired "+
					"while an update with no venue time folds anyway (#957).",
					fset.Position(ifs.Pos()), name)
				return false
			}
			if checksProto && strings.Contains(exprsIn(ifs.Body), "ErrNoVenueTime") {
				refuses = true
			}
			return !refuses
		})

		if !refuses {
			t.Errorf("%s: %s does not refuse an update whose event_time is nil.\n\n"+
				"The check must be `x.GetEventTime() == nil` on the PROTO, returning ErrNoVenueTime. "+
				"Checking the CONVERTED value instead is the original defect (#957): a nil "+
				"google.protobuf.Timestamp reads back through AsTime() as time.Unix(0, 0) — the UNIX "+
				"epoch — so `if b.eventTime.IsZero()` is false for exactly the case it was written "+
				"for, and the branch was dead from the day it was written.",
				fset.Position(fd.Pos()), name)
		}
	}
}

// exprsIn renders the identifiers appearing in a subtree, so a guard can ask
// whether a branch mentions a symbol WITHOUT reading the file's comment text.
func exprsIn(n ast.Node) string {
	var b strings.Builder
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok {
			b.WriteString(id.Name)
			b.WriteByte(' ')
		}
		return true
	})
	return b.String()
}

// NO CLOCK IS SUBSTITUTED IN THE BOOK.
//
// The removed branch read `if et.IsZero() { et = time.Now().UTC() }`. Restoring
// any call to time.Now() here re-creates the failure in its most durable form:
// with the folds now refusing a nil stamp, a substituted clock would make an
// unstamped source publish books that look perfectly FRESH, which is strictly
// worse than the epoch — the epoch was at least refused downstream.
//
// The book may still read the clock's TYPE (`eventTime time.Time`); it may not
// read its VALUE.
func TestTheBookNeverSubstitutesAClock(t *testing.T) {
	fset, f := marketedgeFile(t, "internal/marketedge/book/book.go")

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, "time", "Now") {
			return true
		}
		t.Errorf("%s: book.go calls time.Now().\n\nThe venue's clock is the only clock on this path. "+
			"Substituting the local one converts \"we do not know when the venue said this\" into "+
			"\"we know it now\" — and since #957 made both folds refuse an unstamped update, a "+
			"substituted clock is worse than the epoch it replaced: the epoch was refused by every "+
			"staleness bound downstream, whereas a fresh local timestamp is believed.",
			fset.Position(call.Pos()))
		return true
	})

	// Non-vacuity. time.Now() appears four times in this file's COMMENTS, so a
	// text search would pass with the substitution restored. Proving the parse
	// reached real code — and that the book still stamps SOMETHING — is what
	// separates this guard from that one.
	if !callsPkgFunc(f, "timestamppb", "New") {
		t.Fatal("book.go contains no timestamppb.New call, so this guard proved nothing about a file " +
			"that still emits a timestamp. Either the snapshot stopped carrying an event time — which " +
			"is its own defect — or this guard is now walking the wrong tree (#957).")
	}
}

// THE INGEST NAMES A SOURCE THAT OMITS THE VENUE TIME.
//
// A refusal alone would swap one silent failure for another: before #957 the
// instrument published books nobody would use; a bare refusal makes it publish
// nothing, which from outside looks identical to a quiet market. The issue's
// "Verified when" is a SIGNAL, not just a refusal — so the warning is part of the
// fix and gets a guard of its own.
//
// Warn-once is load-bearing rather than cosmetic. A source that omits the field
// omits it on every message, so an unthrottled line would write one entry per
// update for the life of the pod and bury itself.
func TestTheIngestNamesADepthSourceThatOmitsVenueTime(t *testing.T) {
	fset, f := marketedgeFile(t, "internal/marketedge/ingest/engine.go")

	// Both fold paths must react to the refusal, not just the snapshot one.
	refs := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok && isSelector(e, "book", "ErrNoVenueTime") {
			refs++
		}
		return true
	})
	if refs < 2 {
		t.Errorf("engine.go references book.ErrNoVenueTime %d time(s), want at least 2 — one per fold "+
			"path.\n\nBoth ApplySnapshot and ApplyDelta can refuse. A path that drops the refusal on "+
			"the floor restores the #957 failure mode exactly: the engine folds nothing, publishes "+
			"nothing, reports ready, and says nothing.", refs)
	}

	warn := funcNamed(f, "warnNoVenueTime")
	if warn == nil {
		t.Fatalf("engine.go declares no warnNoVenueTime.\n\n#957's \"Verified when\" is that a source " +
			"omitting event_time MAKES market-ingest SAY SO — a WARN naming the instrument — rather " +
			"than going quiet. A refusal with no signal is still an instrument that goes dark for a " +
			"reason nobody can find.")
	}

	if !callsPkgFunc(warn.Body, "e", "logger") && !hasWarnCall(warn.Body) {
		t.Errorf("%s: warnNoVenueTime logs nothing", fset.Position(warn.Pos()))
	}

	// Warn-once: the throttle is what keeps the signal readable.
	once := false
	ast.Inspect(warn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && (sel.Sel.Name == "Swap" || sel.Sel.Name == "CompareAndSwap") {
			once = true
		}
		return true
	})
	if !once {
		t.Errorf("%s: warnNoVenueTime is not throttled.\n\nA source that omits event_time omits it on "+
			"EVERY message, so an unthrottled warning writes one line per depth update for the life "+
			"of the pod. The one thing worse than a silent defect is a loud one nobody can read past.",
			fset.Position(warn.Pos()))
	}

	// It must name the instrument, or an operator cannot find the source. This
	// reads the call's ARGUMENTS, which are code — not the message prose.
	if !mentionsArg(warn.Body, "instrument") {
		t.Errorf("%s: warnNoVenueTime does not log an \"instrument\" field.\n\nThe estate runs one "+
			"engine per instrument per venue; a warning that does not name which one sends an "+
			"operator to grep the fleet.", fset.Position(warn.Pos()))
	}
}

// hasWarnCall reports whether the subtree calls a .Warn(...) method.
func hasWarnCall(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if c, ok := x.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Warn" {
				found = true
			}
		}
		return !found
	})
	return found
}

// mentionsArg reports whether any call in the subtree passes want as a string
// argument. Arguments are code; the guard never reads the message prose, which is
// how three earlier guards in this directory came to match their own comments.
func mentionsArg(n ast.Node, want string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		for _, a := range call.Args {
			lit, ok := a.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && strings.Trim(lit.Value, `"`) == want {
				found = true
			}
		}
		return !found
	})
	return found
}

// exprHasBoolConst reports whether an expression contains the predeclared `true`
// or `false`. In a refusal condition either one means the refusal is off, with
// the surrounding code left intact for a guard to keep finding.
func exprHasBoolConst(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "true" || id.Name == "false") {
			found = true
		}
		return !found
	})
	return found
}

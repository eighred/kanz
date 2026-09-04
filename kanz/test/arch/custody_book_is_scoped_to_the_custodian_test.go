package arch

import (
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

// THE BOOK SIDE AND THE STATEMENT SIDE MUST BE SCOPED TO THE SAME CUSTODIAN (#1006).
//
// custody.Subject is (portfolio, custodian, business date). Every part of that
// control was custodian-aware — the break id, the run id, the partition key, the
// staleness gauge, the scheduler's pair list, the wire message. ONE THING WAS
// NOT: the book side of the comparison. BookLoader was
// `func(ctx, portfolioID string)`, so there was no parameter through which the
// custodian COULD reach it, and Reconcile loaded the whole portfolio and handed
// it to one custodian's statement.
//
// For a portfolio custodied in two places — which ACCOUNTING_CUSTODY_PAIRS
// accepts and the Scheduler iterates — custodian A's run reports every position
// held at B as MISSING_AT_CUSTODIAN and B's run reports every position at A the
// same way. Every position becomes a break, twice. EXPLAINED counts as
// outstanding by design, so the noise ages, pages via CustodyBreakAgeing, and
// trains an operator to ignore the queue that exists to surface the one break
// meaning a fill never reached the ledger.
//
// WHY A GUARD AND NOT ONLY A TEST. The unit tests prove today's loader scopes
// correctly. They cannot prove that the NEXT change keeps the custodian
// reachable: narrowing BookLoader back to a portfolio id "because that is all it
// uses" compiles, passes every test that does not configure two custodians — and
// before this issue, no test in the tree configured two custodians — and silently
// restores the defect. This asserts the SHAPE that makes the omission impossible.
//
// AST, NOT GREP: a regex over this file's own prose, or over the doc comment on
// the very function it checks, matches its own words and proves nothing.

const (
	custodyReconcileFile = "services/accounting/internal/custody/reconcile.go"
	custodySubjectType   = "Subject"
	custodianField       = "CustodianID"
)

// TestCustodyBookIsScopedToTheCustodian is the whole invariant, in four arms.
func TestCustodyBookIsScopedToTheCustodian(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var (
		file            *ast.File
		loaderTakesSubj bool
		loaderReadsCust bool
		reconcilePasses bool
		found           bool
	)
	walkGoFiles(t, root, "services/accounting/internal/custody", fset, func(rel string, f *ast.File) {
		if rel != custodyReconcileFile {
			return
		}
		found, file = true, f
	})
	if !found {
		t.Fatalf("%s not found — the custody reconciler moved, and this guard is reading nothing. "+
			"Point it at the new path rather than deleting it.", custodyReconcileFile)
	}

	for _, decl := range file.Decls {
		// Arm 1: BookLoader's signature carries the Subject.
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "BookLoader" {
					continue
				}
				ft, ok := ts.Type.(*ast.FuncType)
				if !ok {
					continue
				}
				loaderTakesSubj = paramsNameType(ft, custodySubjectType)
			}
		}

		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		switch {
		// Arm 2: LedgerBookLoader actually READS the custodian off it. A loader
		// that takes a Subject and ignores it is the same defect with a wider
		// signature.
		case fd.Name.Name == "LedgerBookLoader" && fd.Recv == nil:
			loaderReadsCust = readsField(fd, custodianField)

		// Arm 3: Reconcile hands the loader the WHOLE subject. Passing
		// subject.PortfolioID is exactly the call that produced the defect, and it
		// still compiles the moment BookLoader is narrowed.
		case fd.Name.Name == "Reconcile" && receiverIs(fd, "Reconciler"):
			reconcilePasses = passesWholeSubjectToBook(fd)
		}
	}

	if !loaderTakesSubj {
		t.Errorf("custody.BookLoader does not take a %s.\n\n"+
			"There is then no parameter through which the custodian can reach the book side, so the "+
			"comparison loads the whole portfolio and every position held at another custodian breaks "+
			"as MISSING_AT_CUSTODIAN. That is #1006, restored.", custodySubjectType)
	}
	if !loaderReadsCust {
		t.Errorf("custody.LedgerBookLoader never reads %s off the subject it is handed.\n\n"+
			"A loader that accepts the custodian and ignores it produces the same whole-portfolio book "+
			"the defect produced, behind a signature that looks repaired.", custodianField)
	}
	if !reconcilePasses {
		t.Errorf("Reconciler.Reconcile does not pass the whole subject to the book loader.\n\n" +
			"Passing subject.PortfolioID is the exact call #1006 was filed on: the custodian on the " +
			"subject is dropped at the one hop that decides which holdings are compared.")
	}

	// Arm 4 — NON-VACUITY. If the declarations moved, every arm above would be
	// false for the wrong reason and this says so instead of failing three times
	// with a misleading cause.
	if !loaderTakesSubj && !loaderReadsCust && !reconcilePasses {
		t.Fatalf("none of BookLoader, LedgerBookLoader or Reconciler.Reconcile was recognised in %s — "+
			"the guard is almost certainly reading a file whose shape has changed, not a repaired one. "+
			"Check the declarations before trusting any failure above.", custodyReconcileFile)
	}
}

// paramsNameType reports whether any parameter of ft has the named type.
func paramsNameType(ft *ast.FuncType, typeName string) bool {
	if ft.Params == nil {
		return false
	}
	for _, p := range ft.Params.List {
		if id, ok := p.Type.(*ast.Ident); ok && id.Name == typeName {
			return true
		}
	}
	return false
}

// readsField reports whether fd's body contains a selector for the named field.
func readsField(fd *ast.FuncDecl, field string) bool {
	got := false
	ast.Inspect(fd, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == field {
			got = true
		}
		return !got
	})
	return got
}

func receiverIs(fd *ast.FuncDecl, typeName string) bool {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return false
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.Name == typeName
}

// passesWholeSubjectToBook reports whether Reconcile calls its book loader with a
// bare identifier rather than a field of one.
//
// It looks for a call whose Fun is a selector ending in `.book` — the Reconciler's
// loader field — and requires its last argument to be a plain identifier. The
// pre-#1006 call was `r.book(ctx, subject.PortfolioID)`, a SelectorExpr, which is
// precisely what this rejects.
func passesWholeSubjectToBook(fd *ast.FuncDecl) bool {
	ok := false
	ast.Inspect(fd, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "book" || len(call.Args) == 0 {
			return true
		}
		last := call.Args[len(call.Args)-1]
		if id, isID := last.(*ast.Ident); isID && !strings.EqualFold(id.Name, "nil") {
			ok = true
		}
		return true
	})
	return ok
}

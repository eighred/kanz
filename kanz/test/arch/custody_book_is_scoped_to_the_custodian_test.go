package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
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

// TestEveryBookReconciledAgainstACustodianIsScopedToOne is the SAME invariant
// over EVERY caller, and it exists because the guard above was narrower than its
// name (#1025).
//
// The three arms above assert the SHAPE of one file:
// services/accounting/internal/custody/reconcile.go, the scheduled path #1006
// repaired. All three were true, and all three stayed true while the OTHER caller
// of recon.Reconcile — the ad-hoc HTTP handler in
// services/accounting/internal/server — went on materializing the WHOLE portfolio
// and comparing it against one custodian's statement from the request body. For a
// portfolio custodied in two places that answered 200 with a break for every
// position held at the other, and an ad-hoc reconciliation is what an operator
// runs WHILE INVESTIGATING, so the fabricated breaks arrive exactly when somebody
// is trying to read the real one.
//
// THE CALLER SET IS DERIVED, NOT LISTED. A file list written here would be a
// further copy of the thing that broke: #1006's guard named one path, the defect
// lived in the other, and a hand-maintained second list would miss the third
// caller the same way. This finds every call to recon.Reconcile by walking the
// module, so a new one is covered the moment it is written — and it is
// DEFAULT-DENY, so that new caller has to satisfy the property rather than be
// remembered.
//
// THE PROPERTY, in two parts per call site:
//
//  1. The function that reconciles must NAME a custody.Subject — the only type on
//     this platform that carries (portfolio, custodian, business date) together.
//     A function with no subject in it has no custodian to scope by, whatever it
//     passes.
//  2. The BOOK it hands to recon.Reconcile must come from a call that was handed
//     that subject. This is the hop the defect lived on: s.materialize(ctx, id)
//     and r.book(ctx, subject.PortfolioID) both compile, both look like a book,
//     and both drop the custodian at the one call that decides which holdings are
//     compared.
//
// AST, NOT GREP, for the reason the guard above gives: a regex over this file's
// own prose matches its own words. Nothing here reads a comment.
func TestEveryBookReconciledAgainstACustodianIsScopedToOne(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	type site struct {
		file string
		fn   string
		pos  token.Position
		call *ast.CallExpr
		decl *ast.FuncDecl
	}
	var sites []site

	walkModuleGoFiles(t, root, fset, func(rel string, f *ast.File) {
		// The local name of the recon package IN THIS FILE, so an alias cannot
		// hide a call site. Inside package recon itself the calls are unqualified.
		local := reconLocalName(f)
		inRecon := f.Name.Name == reconPackageName
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if !isReconReconcileCall(call, local, inRecon) {
					return true
				}
				sites = append(sites, site{
					file: rel, fn: fd.Name.Name, pos: fset.Position(call.Pos()), call: call, decl: fd,
				})
				return true
			})
		}
	})

	// NON-VACUITY FIRST. Two callers exist today — the scheduled reconciler and
	// the HTTP handler. Finding fewer means the walk, the alias resolution or the
	// package moved, and every assertion below would then pass by not looking,
	// which is precisely how the guard above missed this.
	if len(sites) < 2 {
		names := make([]string, 0, len(sites))
		for _, s := range sites {
			names = append(names, s.file+":"+s.fn)
		}
		t.Fatalf("found %d call site(s) of %s.Reconcile in the module (%s), want at least 2.\n\n"+
			"This guard derives its caller set by walking the module for that call. Finding almost none "+
			"means it is reading nothing — repair the derivation rather than trusting the pass.",
			len(sites), reconPackageName, strings.Join(names, ", "))
	}

	for _, s := range sites {
		subjects := subjectIdents(s.decl)
		if len(subjects) == 0 {
			t.Errorf("%s: %s reconciles a book against a custodian statement but never names a "+
				"%s.%s.\n\n"+
				"There is then no custodian anywhere in the function, so the book it compares is the "+
				"WHOLE portfolio and every position held at another custodian comes back as "+
				"MISSING_AT_CUSTODIAN. That is #1006/#1025 — the defect this guard exists for.",
				s.pos, s.fn, custodyPackageName, custodySubjectType)
			continue
		}
		if len(s.call.Args) == 0 {
			t.Errorf("%s: %s.Reconcile was called with no arguments — the signature changed and this "+
				"guard cannot see the book side. Repair it rather than deleting it.", s.pos, reconPackageName)
			continue
		}
		book, ok := s.call.Args[0].(*ast.Ident)
		if !ok {
			t.Errorf("%s: the book handed to %s.Reconcile is not a plain identifier, so this guard "+
				"cannot follow it back to the call that produced it. Bind the book to a variable "+
				"first — the assertion is that the custodian reached whatever produced it.",
				s.pos, reconPackageName)
			continue
		}
		producers := definingCalls(s.decl, book.Name)
		if len(producers) == 0 {
			t.Errorf("%s: the book %q handed to %s.Reconcile is not assigned from a call in %s.\n\n"+
				"The book must be PRODUCED by something the subject was handed to; a book that arrives "+
				"any other way has no custodian on it.", s.pos, book.Name, reconPackageName, s.fn)
			continue
		}
		// EVERY assignment to the book, not just the last: a function that loads
		// it correctly and then reassigns it from a whole-portfolio materialize —
		// a retry, a fallback, a branch — is the defect with a compliant
		// assignment standing in front of it.
		if slices.ContainsFunc(producers, func(c *ast.CallExpr) bool { return !callTakesOneOf(c, subjects) }) {
			t.Errorf("%s: %s builds the book %q WITHOUT passing it the custody subject (%s).\n\n"+
				"This is the exact hop the defect lived on: a whole-portfolio materialize takes a "+
				"portfolio id, compiles, and looks like a book — and it drops the custodian at the one "+
				"call that decides which holdings are compared. The result is one custodian's statement "+
				"against every custodian's holdings: a break for every position the fund holds "+
				"elsewhere, burying the one that means a fill never reached the ledger.\n\n"+
				"Load the book through custody.LedgerBookLoader, or another loader handed the whole "+
				"subject, rather than a whole-portfolio materialize.",
				s.pos, s.fn, book.Name, strings.Join(subjects, ", "))
		}
	}
}

const (
	reconPackageName   = "recon"
	custodyPackageName = "custody"
	reconImportSuffix  = "/services/accounting/internal/recon"
)

// reconLocalName returns the name recon is imported under in this file, or "" if
// it is not imported. It reads the IMPORT rather than assuming "recon", so an
// alias cannot move a call site out of this guard's sight.
func reconLocalName(f *ast.File) string {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, "\"")
		if !strings.HasSuffix(path, reconImportSuffix) {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return reconPackageName
	}
	return ""
}

// isReconReconcileCall reports whether call is recon.Reconcile — qualified by the
// package's local name, or unqualified inside package recon itself.
func isReconReconcileCall(call *ast.CallExpr, local string, inRecon bool) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		id, ok := fn.X.(*ast.Ident)
		return ok && local != "" && id.Name == local && fn.Sel.Name == "Reconcile"
	case *ast.Ident:
		return inRecon && fn.Name == "Reconcile"
	}
	return false
}

// subjectIdents returns every identifier in fd bound to a custody.Subject — as a
// parameter, or as a composite literal assigned to a name.
//
// It is TYPE-NAME driven and never reads a comment, so a function that merely
// mentions the word cannot satisfy it.
func subjectIdents(fd *ast.FuncDecl) []string {
	var out []string
	add := func(n string) {
		if n != "" && n != "_" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if fd.Type.Params != nil {
		for _, p := range fd.Type.Params.List {
			if !isSubjectType(p.Type) {
				continue
			}
			for _, n := range p.Names {
				add(n.Name)
			}
		}
	}
	ast.Inspect(fd, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			lit, isLit := rhs.(*ast.CompositeLit)
			if !isLit || !isSubjectType(lit.Type) {
				continue
			}
			if id, isID := as.Lhs[i].(*ast.Ident); isID {
				add(id.Name)
			}
		}
		return true
	})
	return out
}

// isSubjectType reports whether expr names custody.Subject — qualified from
// another package, or bare inside custody itself.
func isSubjectType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == custodySubjectType
	case *ast.SelectorExpr:
		id, ok := t.X.(*ast.Ident)
		return ok && id.Name == custodyPackageName && t.Sel.Name == custodySubjectType
	}
	return false
}

// definingCalls returns EVERY call expression the named variable is assigned
// from within fd.
func definingCalls(fd *ast.FuncDecl, name string) []*ast.CallExpr {
	var found []*ast.CallExpr
	ast.Inspect(fd, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, isCall := as.Rhs[0].(*ast.CallExpr)
		if !isCall {
			return true
		}
		for _, lhs := range as.Lhs {
			if id, isID := lhs.(*ast.Ident); isID && id.Name == name {
				found = append(found, call)
			}
		}
		return true
	})
	return found
}

// callTakesOneOf reports whether call is passed one of the named identifiers
// WHOLE — a bare ident, never a field of one. subject.PortfolioID is precisely
// the argument #1006 was filed on, so a selector must not satisfy this.
func callTakesOneOf(call *ast.CallExpr, names []string) bool {
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok && slices.Contains(names, id.Name) {
			return true
		}
	}
	return false
}

// walkModuleGoFiles parses every non-test .go file in the module.
//
// THE SKIP SET IS NOT DECORATION. .gotmp is the in-module GOTMPDIR the Makefile
// exports and it collects generated _testmain.go files, which parse as Go and
// belong to nothing; .claude holds agent worktrees, which are FULL COPIES of this
// module and would have this guard reading — and reporting positions in — a
// different checkout of the same files.
func walkModuleGoFiles(t *testing.T, root string, fset *token.FileSet, fn func(rel string, f *ast.File)) {
	t.Helper()
	skip := map[string]bool{
		".git": true, ".gotmp": true, ".claude": true,
		"vendor": true, "testdata": true, "node_modules": true,
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		fn(filepath.ToSlash(rel), parsed)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

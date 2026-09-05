package arch

import (
	"go/ast"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// ONE ENTRY, ONE ANSWER: A CUSTODY COMPARISON BASIS HOLDS ONLY THE ENTRIES THAT
// SETTLED AGAINST AN EXCHANGE ACCOUNT (#1073).
//
// # What went wrong, and why a unit test could not hold it
//
// The custody plane had TWO rules for the same entries, and each documented itself
// as correct. ledger.MaterializeForAccounts excluded an entry whose
// venue_account_id is '' — the positive declaration that it settled against no
// exchange account — because no exchange custodian's statement can list one. The
// single-custodian branch of custody.LedgerBookLoader called
// ledger.MaterializeCurrent instead and compared the WHOLE portfolio, which
// includes exactly those entries.
//
// Both cannot be right. Running the second against a custodian statement makes
// recon.Reconcile union the currencies and report the entire un-attributed balance
// as a cash break: an investor subscription sitting in the fund's own bank,
// reported every run as a disagreement with a custodian that cannot see it — on
// the DEFAULT configuration, which is the shape most deployments are in. A break
// queue with routine false positives is one an operations team stops reading, and
// the real break then lands in a queue nobody trusts.
//
// A unit test proves today's loader excludes it. It cannot prove the NEXT change
// keeps the exclusion: reintroducing a whole-portfolio materialize in a fallback,
// a retry or a new branch compiles, still returns a *ledger.Book, and restores the
// defect behind a signature that looks repaired. This asserts the SHAPE — nothing
// in the custody package folds a whole portfolio, and the derivation that replaced
// it is still a derivation.
//
// # Why this is a separate guard
//
// custody_book_is_scoped_to_the_custodian_test.go asserts that the CUSTODIAN
// reaches the book side. That property was true throughout: the single-custodian
// branch was handed the whole Subject and ignored it, which is the shape that guard
// permits and this one does not. A guard passing proves what it resolved, not what
// its name suggests.
//
// AST, NOT GREP: a regex over this file's own prose, or over the doc comments on
// the functions it checks, matches its own words and proves nothing. Nothing below
// reads a comment.

const (
	custodyPkgRel     = "services/accounting/internal/custody"
	custodyScopeFile  = "services/accounting/internal/ledger/custodyscope.go"
	wholeBookFold     = "MaterializeCurrent"
	scopedFold        = "MaterializeForAccounts"
	derivedFold       = "MaterializeAttributed"
	accountDerivation = "AttributedAccounts"
	venueAccountField = "VenueAccountID"
)

// TestNoCustodyComparisonFoldsAWholePortfolio is the default-deny arm: the custody
// package may fold a DECLARED account scope or a DERIVED one, and nothing else.
func TestNoCustodyComparisonFoldsAWholePortfolio(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	type call struct {
		fn  string
		pos token.Position
	}
	var wholeBook []call
	scopedCalls := map[string]int{}
	files := 0

	walkGoFiles(t, root, custodyPkgRel, fset, func(_ string, f *ast.File) {
		files++
		// The local name the ledger is imported under IN THIS FILE, so an alias
		// cannot move a call out of this guard's sight.
		local := importLocalName(f, "/services/accounting/internal/ledger")
		if local == "" {
			return
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				c, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				name, ok := qualifiedCallName(c, local)
				if !ok {
					return true
				}
				switch name {
				case wholeBookFold:
					wholeBook = append(wholeBook, call{fd.Name.Name, fset.Position(c.Pos())})
				case scopedFold, derivedFold:
					scopedCalls[name]++
				}
				return true
			})
		}
	})

	if files == 0 {
		t.Fatalf("no non-test .go file was parsed under %s — the custody package moved and this guard "+
			"is reading nothing. Point it at the new path rather than deleting it.", custodyPkgRel)
	}

	// NON-VACUITY BEFORE THE ASSERTION. A custody package that calls NEITHER
	// scoped fold is one whose book side has been rewritten, and the default-deny
	// arm above would then pass by not looking — which is exactly how the earlier
	// guard missed the branch this issue is about.
	if len(scopedCalls) == 0 {
		t.Fatalf("the custody package calls neither ledger.%s nor ledger.%s.\n\n"+
			"Those are the only two folds that produce a comparison basis scoped to exchange accounts. "+
			"Finding neither means this guard is reading a package whose book side no longer works the "+
			"way it asserts — repair the derivation rather than trusting the pass.",
			scopedFold, derivedFold)
	}

	if len(wholeBook) > 0 {
		var where []string
		for _, c := range wholeBook {
			where = append(where, c.pos.String()+" in "+c.fn)
		}
		sort.Strings(where)
		t.Errorf("the custody package folds a WHOLE PORTFOLIO at %d site(s):\n\n  %s\n\n"+
			"ledger.%s carries every entry, including the ones that settled against NO exchange account "+
			"— an investor subscription into the fund's own bank, a corporate action. No exchange "+
			"custodian's statement can list those, so comparing a book that holds them reports the "+
			"whole un-attributed balance as a cash break, every run, on the default configuration. That "+
			"is #1073.\n\n"+
			"Fold ledger.%s for a declared scope or ledger.%s for a single-custodian portfolio, both of "+
			"which exclude an entry carrying no exchange account. Do not add an exemption here: the "+
			"whole-portfolio book is what NAV and every non-reconciliation reader use, and it is the one "+
			"thing a comparison against ONE custodian must never be.",
			len(wholeBook), strings.Join(where, "\n  "), wholeBookFold, scopedFold, derivedFold)
	}
}

// TestTheCustodyBasisDerivationExcludesTheEmptyAccount asserts the two halves of
// the rule that replaced the disagreement: the derived scope is derived from the
// journal, and BOTH the derivation and the fold treat the empty account id as a
// non-member.
//
// The empty account is the whole defect in one character. The empty string is the
// positive declaration that an entry settled against no exchange account, and
// ledger.NewAccountScope already refuses an operator who tries to DECLARE it for a
// custodian. A derivation that admitted it would make the same claim by a route
// nobody typed, and a fold that stopped skipping it would put the fund's own bank
// cash back into a custodian's book with every signature unchanged.
func TestTheCustodyBasisDerivationExcludesTheEmptyAccount(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	funcs := map[string]*ast.FuncDecl{}
	found := false
	walkGoFiles(t, root, "services/accounting/internal/ledger", fset, func(rel string, f *ast.File) {
		if rel != custodyScopeFile {
			return
		}
		found = true
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	})
	if !found {
		t.Fatalf("%s not found — the custodian-scoped fold moved, and this guard is reading nothing. "+
			"Point it at the new path rather than deleting it.", custodyScopeFile)
	}

	// The derived fold must DERIVE. A scope taken from anywhere else — a
	// parameter, a package variable, a configuration — is a declaration wearing
	// the derivation's name, and the single-custodian portfolio it exists for has
	// nothing to declare.
	derived := funcs[derivedFold]
	if derived == nil {
		t.Fatalf("%s declares no %s — the single-custodian comparison basis has no derivation, so the "+
			"only fold left for a portfolio with one custodian takes a declaration nobody wrote or the "+
			"whole book. Repair this guard against the replacement rather than deleting it.",
			custodyScopeFile, derivedFold)
	}
	if !callsFunction(derived, accountDerivation) {
		t.Errorf("ledger.%s does not call %s.\n\n"+
			"Its scope must come from the accounts the JOURNAL touched. A single-custodian portfolio "+
			"holds every account it touches at that one custodian and has no declaration to consult, so "+
			"a scope from any other source is either empty — folding a book of nothing, and breaking "+
			"every position the custodian holds as MISSING_IN_IBOR — or the whole book, which is #1073.",
			derivedFold, accountDerivation)
	}

	// Both the derivation and the shared fold must compare the entry's exchange
	// account against the empty string. This follows the FIELD, not a name: an
	// assignment `acct := e.VenueAccountID` followed by `acct == ""` counts, and a
	// comparison of some unrelated string to "" does not.
	for _, name := range []string{accountDerivation, "foldForAccounts"} {
		fd := funcs[name]
		if fd == nil {
			t.Errorf("%s declares no %s — this guard cannot see whether the empty account id is still "+
				"excluded. Repair it against the replacement.", custodyScopeFile, name)
			continue
		}
		if !comparesVenueAccountToEmpty(fd) {
			t.Errorf("ledger.%s no longer tests the entry's %s against the empty string.\n\n"+
				"'' is the positive declaration that an entry settled against NO exchange account. "+
				"Admitting it puts the fund's own bank cash into a custodian's comparison basis, where "+
				"it comes back as a cash break for the full amount every run — the defect #1073 was "+
				"filed on. ledger.NewAccountScope refuses an operator who declares it; this must refuse "+
				"it by derivation.", name, venueAccountField)
		}
	}
}

// importLocalName returns the name a package with the given import-path suffix is
// imported under in f, or "" when it is not imported. It reads the IMPORT rather
// than assuming the package name, so an alias cannot hide a call.
func importLocalName(f *ast.File, pathSuffix string) string {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, "\"")
		if !strings.HasSuffix(path, pathSuffix) {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		if i := strings.LastIndex(path, "/"); i >= 0 {
			return path[i+1:]
		}
		return path
	}
	return ""
}

// qualifiedCallName returns the selector name of a call qualified by local, e.g.
// "MaterializeCurrent" for ledger.MaterializeCurrent(...).
func qualifiedCallName(call *ast.CallExpr, local string) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != local {
		return "", false
	}
	return sel.Sel.Name, true
}

// callsFunction reports whether fd's body calls the named unqualified function.
func callsFunction(fd *ast.FuncDecl, name string) bool {
	got := false
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, isID := call.Fun.(*ast.Ident); isID && id.Name == name {
			got = true
		}
		return !got
	})
	return got
}

// comparesVenueAccountToEmpty reports whether fd compares an entry's
// VenueAccountID — directly, or through a local bound to it — against "".
//
// It follows the assignment because the shared fold reads the field once into a
// variable, and a guard that only matched the field selector would go green the
// moment somebody introduced that perfectly reasonable local.
func comparesVenueAccountToEmpty(fd *ast.FuncDecl) bool {
	// Locals assigned from something.VenueAccountID.
	aliases := map[string]bool{}
	ast.Inspect(fd, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			sel, isSel := rhs.(*ast.SelectorExpr)
			if !isSel || sel.Sel.Name != venueAccountField {
				continue
			}
			if id, isID := as.Lhs[i].(*ast.Ident); isID {
				aliases[id.Name] = true
			}
		}
		return true
	})

	isAccount := func(e ast.Expr) bool {
		switch v := e.(type) {
		case *ast.SelectorExpr:
			return v.Sel.Name == venueAccountField
		case *ast.Ident:
			return aliases[v.Name]
		}
		return false
	}
	isEmptyString := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
	}

	got := false
	ast.Inspect(fd, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
			return true
		}
		if (isAccount(be.X) && isEmptyString(be.Y)) || (isEmptyString(be.X) && isAccount(be.Y)) {
			got = true
		}
		return !got
	})
	return got
}

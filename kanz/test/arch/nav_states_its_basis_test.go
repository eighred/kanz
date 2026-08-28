package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A LEVERAGE DENOMINATOR MUST SAY WHAT IT IS (#780).
//
// WHAT WENT WRONG. compliance.Book.NAV is what LeverageRule divides gross
// exposure by. Every producer on this platform set it to the sum of position
// market values — the OMS pre-trade book source, the post-trade monitor, and the
// snapshot adapter both of them went through. Gross exposure is the sum of the
// ABSOLUTE values of the same positions, so for a book with no shorts the
// numerator and the denominator were THE SAME NUMBER: the ratio was 1.0 by
// construction and a max_gross_leverage cap could not bind however the fund was
// financed. A portfolio 95% in cash and one fully invested scored identically,
// because the cash a leverage limit is measured against was not in the figure.
//
// The control ran, passed, and was recorded in the audit trail as a check that
// APPROVED the order — the shape #640 already cost this repository once, where a
// book entirely in one sector was admitted under a 10% sector cap.
//
// WHY A FIELD DID NOT FIX IT ON ITS OWN. Book.NAVBasis makes the claim explicit
// and its zero value refuses, so a producer that says nothing fails closed. That
// is the safe direction, and it is not the same as being noticed: a new producer
// that sets NAV and forgets the basis ships a book whose leverage rule refuses
// EVERY order, and the first anyone hears of it is a fund unable to trade. The
// point of this guard is that the omission is a build failure rather than an
// incident.
//
// WHAT IT CHECKS, in the files that can actually construct one — internal/
// compliance itself and anything importing it:
//
//   - a Book composite literal that sets NAV also sets NAVBasis;
//   - a function that ASSIGNS a .NAV field also mentions NAVBasis, because two
//     of the three producers build the book first and fill NAV in afterwards.
//
// It parses. A grep for "NAV" over this repository matches the word in prose
// more often than in code — the paragraphs above are themselves the proof — and
// a guard that matches its own comments checks nothing.
//
// WHAT IT DOES NOT CHECK: that the basis is TRUE. NAVBasisEquity is a claim a
// producer makes, and only its own tests can falsify it — see
// services/oms/internal/compliance/equity_test.go, which pins the claim from
// both ends, and internal/compliance/leverage_equity_test.go, which pins what
// the rule does with each of the three answers. A guard cannot tell equity from
// a positions total by reading the assignment; it can only insist that somebody
// said which one it is.
//
// It shares exprName with gateway_revocation_enforced_test.go: the package already
// had that helper, and a second copy of four lines of AST plumbing is how the
// seventeen secret() variants started.
func TestEveryNAVProducerStatesItsBasis(t *testing.T) {
	const compliancePkg = "github.com/eighred/kanz/internal/compliance"

	root := moduleRoot(t)
	var problems []string
	scanned := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// gen/ is generated and testdata/ is fixtures; neither constructs a Book.
			if info.Name() == "gen" || info.Name() == "testdata" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		// TESTS ARE OUT OF SCOPE, deliberately. A fixture with a naked NAV is a
		// confusing test failure for its author; a PRODUCER with one is a fund that
		// cannot trade. The consequence is what this guard is sized to.
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}

		// Only files that can NAME the type: the package itself, or an importer.
		// Without this, internal/alternatives' own NAV field — a *big.Rat on a
		// different type entirely — is reported as a missing compliance basis.
		inPkg := strings.HasPrefix(rel, "internal/compliance/")
		alias, imports := complianceImport(file, compliancePkg)
		if !inPkg && !imports {
			return nil
		}
		scanned++

		bookType := "Book"
		if !inPkg {
			bookType = alias + ".Book"
		}
		problems = append(problems, nakedNAVLiterals(file, rel, bookType)...)
		problems = append(problems, nakedNAVAssignments(file, rel)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// NON-VACUOUS BY DESIGN. A guard that walked zero files reports PASS, and
	// would go on reporting PASS for a repository where the type had been renamed
	// out from under it.
	if scanned == 0 {
		t.Fatalf("no file in or importing %s was scanned — this guard verified NOTHING. Either the "+
			"package moved or the walk is broken; both are failures, not a clean run", compliancePkg)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("compliance Books set a NAV without saying what it measures (%d file(s) scanned):\n  - %s",
			scanned, strings.Join(problems, "\n  - "))
	}
}

// complianceImport returns the local name internal/compliance is imported under,
// and whether it is imported at all. It reads the import spec rather than
// assuming "compliance" or "comp": both spellings are in use, and a guard that
// assumed one would silently skip every file using the other.
func complianceImport(file *ast.File, pkg string) (string, bool) {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != pkg {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		return "compliance", true
	}
	return "", false
}

// nakedNAVLiterals reports Book composite literals that set NAV and not
// NAVBasis.
func nakedNAVLiterals(file *ast.File, rel, bookType string) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || exprName(lit.Type) != bookType {
			return true
		}
		var setsNAV, setsBasis bool
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			switch exprName(kv.Key) {
			case "NAV":
				setsNAV = true
			case "NAVBasis":
				setsBasis = true
			}
		}
		if setsNAV && !setsBasis {
			out = append(out, fmt.Sprintf(
				"%s: a %s literal sets NAV and not NAVBasis. LeverageRule divides gross exposure by "+
					"that number, and a positions total divided into itself is 1.0 for every long-only "+
					"book — say which it is (#780)", rel, bookType))
		}
		return true
	})
	return out
}

// nakedNAVAssignments reports functions that assign a .NAV field without
// mentioning NAVBasis anywhere in the same function.
//
// TWO OF THE THREE PRODUCERS BUILD THE BOOK AND FILL NAV IN AFTERWARDS, so a
// literal-only check would have passed the post-trade monitor — which is one of
// the two places the defect actually shipped.
func nakedNAVAssignments(file *ast.File, rel string) []string {
	var out []string
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var assignsNAV bool
		var mentionsBasis bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				if v.Name == "NAVBasis" || strings.HasPrefix(v.Name, "NAVBasis") {
					mentionsBasis = true
				}
			case *ast.SelectorExpr:
				if v.Sel.Name == "NAVBasis" {
					mentionsBasis = true
				}
			}
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "NAV" {
					assignsNAV = true
				}
			}
			return true
		})
		if assignsNAV && !mentionsBasis {
			out = append(out, fmt.Sprintf(
				"%s: %s assigns a .NAV field and never mentions NAVBasis. A book whose NAV is filled "+
					"in after construction still has to say what that number measures, or the leverage "+
					"cap it feeds cannot bind (#780)", rel, fn.Name.Name))
		}
	}
	return out
}

// AND EVERY RULE THAT READS NAV GOES THROUGH THE BASIS CHECK (#780).
//
// The guard above makes producers declare what their NAV is. This one is the
// consumer half, and it is the half that decides whether the declaration is
// worth anything: LeverageRule is the ONLY rule that divides by NAV today, and
// the equity check lives inside it. A second rule reading c.Book.NAV directly —
// a net-exposure limit, a stress-loss ratio, anything with equity in the
// denominator — would inherit the whole defect on day one, and it would inherit
// it silently, because a positions total divided into itself is a plausible
// number rather than an error.
//
// WHAT IT CHECKS: a rule — a function with the (c *Candidate, rule *Rule)
// signature every rule in the engine has — that reads a NAV field must call
// unusableEquity. The signature is the discriminator on purpose: it is what the
// engine's dispatch table requires, so a new rule cannot avoid this check
// without also failing to be a rule.
//
// WHAT IT DOES NOT CHECK: helpers and domain validators. bookInDomain reads
// b.NAV to range-check a Decimal and has no business asking what the number
// means; it does not have the rule signature, so it is out of scope by
// construction rather than by exemption.
func TestEveryRuleThatDividesByNAVChecksTheBasis(t *testing.T) {
	const pkgDir = "internal/compliance"

	root := moduleRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, pkgDir))
	if err != nil {
		t.Fatalf("read %s: %v", pkgDir, err)
	}

	var problems []string
	rules := 0
	navRules := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		rel := pkgDir + "/" + e.Name()
		file, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(root, pkgDir, e.Name()), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !isRuleFunc(fn) {
				continue
			}
			rules++
			var readsNAV, checksBasis bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "NAV" {
					readsNAV = true
				}
				if call, ok := n.(*ast.CallExpr); ok && exprName(call.Fun) == "unusableEquity" {
					checksBasis = true
				}
				return true
			})
			if !readsNAV {
				continue
			}
			navRules++
			if !checksBasis {
				problems = append(problems, fmt.Sprintf(
					"%s: %s reads Book.NAV and never calls unusableEquity. NAV is only a leverage "+
						"denominator when the book says it is equity; without the check this rule "+
						"divides by whatever the producer happened to supply, which for every "+
						"snapshot on this platform is the sum of the same positions the numerator "+
						"sums — a ratio of 1.0 that binds on nothing (#780)", rel, fn.Name.Name))
			}
		}
	}

	// NON-VACUOUS ON BOTH COUNTS. Finding no rules at all means the signature
	// changed and this guard stopped seeing the engine; finding no rule that reads
	// NAV means LeverageRule stopped reading it, which is either a rewrite this
	// guard needs to follow or the check being quietly removed.
	if rules == 0 {
		t.Fatalf("no rule functions found in %s — the (c *Candidate, rule *Rule) signature this "+
			"guard keys on has changed, and it is now checking nothing", pkgDir)
	}
	if navRules == 0 {
		t.Fatalf("%d rule(s) scanned in %s and NONE reads Book.NAV. LeverageRule is supposed to: "+
			"either the leverage denominator moved somewhere this guard cannot see it, or the "+
			"rule was removed", rules, pkgDir)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("a rule divides by NAV without establishing it is equity (%d rule(s) scanned, %d "+
			"reading NAV):\n  - %s", rules, navRules, strings.Join(problems, "\n  - "))
	}
}

// isRuleFunc reports the engine's rule signature: (c *Candidate, rule *Rule)
// returning a violation. Keying on the SHAPE rather than on a name list is what
// makes the guard cover a rule nobody has written yet — a hardcoded list of
// today's rules would pass for tomorrow's.
func isRuleFunc(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || fn.Type.Params == nil || len(fn.Type.Params.List) != 2 {
		return false
	}
	want := []string{"Candidate", "Rule"}
	for i, p := range fn.Type.Params.List {
		star, ok := p.Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		name := exprName(star.X)
		if !strings.HasSuffix(name, want[i]) {
			return false
		}
	}
	return true
}

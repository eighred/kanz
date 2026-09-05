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

// A NAV MAY NOT VALUE ONE CURRENCY BUCKET AND CALL IT THE BOOK (#1041).
//
// # What went wrong without it
//
// services/accounting/internal carried two NAV implementations. ComputeNAVInCurrency
// iterated every entry in the book's Cash and Accrued maps, converted each at an
// FX rate, and refused the whole valuation when a rate was missing.  ComputeNAV
// — the "domestic fast path" — read ONE entry from each:
//
//	cash    := b.CashBalance(currency)
//	accrued := b.AccruedBalance(currency)
//
// Every other currency the fund held was not summed, not converted, and NOT
// REPORTED AS OMITTED. A book holding USD 100 and EUR 50 answered
// NAV{Cash: 100} with a nil error. The position leg was worse: it had no
// currency check at all, so a EUR market value was added into a USD total as a
// USD number. Wrong in both directions at once, on the fund's headline figure,
// and the errors do not cancel.
//
// AND THE ESTATE COULD TAKE NO OTHER PATH. Server.computeNAV chose the rigorous
// form only with a request-supplied fx table or a live provider, and the live
// provider is appended only when ACCOUNTING_FX_PAIRS is set — which no manifest
// in this repository does. So the fast path was not an edge case; it was the
// only reachable one.
//
// # Why a guard and not just the fix
//
// This is #257's defect one plane over. #257 — risk measures silently dropping
// positions whose currency differs from the base — produced
// TestRiskMeasuresDeclareTheirCurrencyFilter, whose scope constant is
// "internal/risk/". services/accounting is outside that subtree, so the identical
// shape here was never looked at and that guard is structurally unable to report
// it. Widening it is estate-wide work that lands alone in its batch; this is the
// same rule stated at the second site, scoped to the valuation layer so it can
// land beside the repair.
//
// # What it checks
//
// Every function in the accounting valuation package whose name begins with
// ComputeNAV must EITHER delegate to ComputeNAVInCurrency, OR itself range over
// both the Cash and the Accrued maps. Keying on the name prefix rather than on a
// list of today's two functions is what makes it cover a third nobody has
// written yet.
//
// It parses. A grep for "Cash" over this package matches the word in prose more
// often than in code — the paragraphs above are themselves the proof — and a
// guard that matches its own comments checks nothing. Comments are not attached
// (parser mode 0), so nothing written here can satisfy it.
//
// # What it does NOT check
//
// That the conversion is CORRECT, or that the rate is real. A guard cannot tell
// a right rate from a wrong one; it can only insist that no currency the book
// holds is left out of the question. The arithmetic is pinned by the package's
// own tests — TestComputeNAVMatchesTheGeneralFormOnADomesticBook from one side
// and TestComputeNAVRefusesABookHoldingForeignCash from the other.
func TestEveryAccountingNAVComputationCoversEveryCurrencyBucket(t *testing.T) {
	const pkgDir = "services/accounting/internal"

	var problems []string
	computations := 0
	for _, f := range navValuationFiles(t, pkgDir) {
		for _, d := range f.file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "ComputeNAV") {
				continue
			}
			computations++

			var delegates, rangesCash, rangesAccrued bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && exprName(call.Fun) == "ComputeNAVInCurrency" {
					delegates = true
				}
				rng, ok := n.(*ast.RangeStmt)
				if !ok {
					return true
				}
				switch selectorField(rng.X) {
				case "Cash":
					rangesCash = true
				case "Accrued":
					rangesAccrued = true
				}
				return true
			})
			if delegates || (rangesCash && rangesAccrued) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: %s neither delegates to ComputeNAVInCurrency nor ranges over both the Cash and "+
					"the Accrued maps (cash=%v accrued=%v). A valuation that reads one currency "+
					"bucket returns a number understated by the whole value of every other currency "+
					"the fund holds, with no error and no flag — byte-identical to a correct NAV "+
					"(#1041)", f.rel, fn.Name.Name, rangesCash, rangesAccrued))
		}
	}

	// NON-VACUOUS BY DESIGN. A guard that found no NAV computation reports PASS,
	// and would go on reporting PASS for a package where the functions had been
	// renamed out from under it.
	if computations < 2 {
		t.Fatalf("found %d ComputeNAV* function(s) in %s — this package has the general form and at "+
			"least one entry point into it, so the scan is broken and this guard is asserting "+
			"nothing", computations, pkgDir)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("a NAV computation does not cover every currency bucket (%d scanned):\n  - %s",
			computations, strings.Join(problems, "\n  - "))
	}
}

// AND THE REFUSAL MAY NOT BE SWITCHED OFF BY A CONSTANT (#771, #957).
//
// An arch guard that asserts a refusal EXISTS keeps passing when somebody writes
// `if false && len(missing) > 0`: every symbol it reads is still there. This has
// cost this repository twice. A condition inside a NAV computation carries no
// bool literal, so the completeness gate cannot be disabled while looking armed.
func TestNoAccountingNAVRefusalIsDisabledByAConstant(t *testing.T) {
	const pkgDir = "services/accounting/internal"

	var problems []string
	conditions := 0
	for _, f := range navValuationFiles(t, pkgDir) {
		for _, d := range f.file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "ComputeNAV") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ifs, ok := n.(*ast.IfStmt)
				if !ok || ifs.Cond == nil {
					return true
				}
				conditions++
				ast.Inspect(ifs.Cond, func(c ast.Node) bool {
					id, ok := c.(*ast.Ident)
					if !ok || (id.Name != "true" && id.Name != "false") {
						return true
					}
					problems = append(problems, fmt.Sprintf(
						"%s: %s has an `if` whose condition contains the bool constant %q. A "+
							"completeness gate wired to a constant reads as armed and refuses "+
							"nothing (#1041)", f.rel, fn.Name.Name, id.Name))
					return true
				})
				return true
			})
		}
	}
	if conditions == 0 {
		t.Fatalf("no `if` condition found inside any ComputeNAV* function in %s — the completeness "+
			"gates this checks are gone, or the scan is broken", pkgDir)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("a NAV completeness gate is disabled by a constant:\n  - %s", strings.Join(problems, "\n  - "))
	}
}

// AND THE HTTP SURFACE MAY NOT REACH A VALUATION WITHOUT AN FX CONVERTER (#1041).
//
// The guard above holds the library. This one holds the ONE place the estate
// calls it from, and it is the half that decides whether the library rule is
// worth anything: Server.computeNAV had a `default:` arm that bypassed the
// completeness-gated form entirely, and — because no manifest configures FX —
// that arm was the only one a deployed pod could reach.
//
// WHAT IT CHECKS: the set of accounting NAV entry points computeNAV calls is
// exactly {ComputeNAVInCurrency}. Deriving the set from the call sites rather
// than listing the forbidden ones means a third entry point added to the package
// is covered on the day it is written.
func TestTheAccountingNAVEndpointAlwaysSuppliesAnFXConverter(t *testing.T) {
	const rel = "services/accounting/internal/server/server.go"

	root := moduleRoot(t)
	// Mode 0: comments are not attached, so the paragraphs in server.go — which
	// name both functions — cannot satisfy or defeat this.
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	var found bool
	called := map[string]bool{}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv == nil || fn.Name.Name != "computeNAV" {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name := selectorField(call.Fun); strings.HasPrefix(name, "ComputeNAV") {
				called[name] = true
			}
			return true
		})
	}
	if !found {
		t.Fatalf("no computeNAV method in %s — the NAV endpoint's FX selection moved somewhere this "+
			"guard cannot see it, and it is now checking nothing", rel)
	}
	if len(called) == 0 {
		t.Fatalf("computeNAV in %s calls no accounting NAV entry point at all — the valuation moved "+
			"and this guard is passing vacuously", rel)
	}
	var names []string
	for n := range called {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "ComputeNAVInCurrency" {
		t.Fatalf("computeNAV in %s reaches %v. Only ComputeNAVInCurrency is completeness-gated over "+
			"every currency the book holds; any other arm values a book it has not established is "+
			"domestic, and on this estate — where no manifest sets ACCOUNTING_FX_PAIRS — that arm "+
			"is the only one a deployed pod can reach (#1041). Supply an identity FX table instead "+
			"of bypassing the gate", rel, names)
	}
}

// navFile is one parsed source file of the accounting valuation package.
type navFile struct {
	rel  string
	file *ast.File
}

// navValuationFiles parses the non-test Go files sitting DIRECTLY in the
// accounting valuation package. Subdirectories are separate packages (ledger,
// server, consume) and hold no NAV computation; the directory listing is read
// rather than a file list written here, so a valuation split into a new file is
// covered without anyone remembering to add it.
func navValuationFiles(t *testing.T, pkgDir string) []navFile {
	t.Helper()
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(pkgDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", pkgDir, err)
	}
	var out []navFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		// Mode 0: comments are not attached. A guard that matches its own prose,
		// or the package's, checks nothing.
		f, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s/%s: %v", pkgDir, e.Name(), perr)
		}
		out = append(out, navFile{rel: pkgDir + "/" + e.Name(), file: f})
	}
	if len(out) == 0 {
		t.Fatalf("no non-test Go file in %s — the package moved and this guard reads nothing", pkgDir)
	}
	return out
}

// selectorField returns the field or function name an expression ends in:
// "Cash" for b.Cash, "ComputeNAVInCurrency" for accounting.ComputeNAVInCurrency,
// and the identifier itself for a bare name. Anything else is "".
func selectorField(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.Ident:
		return v.Name
	}
	return ""
}

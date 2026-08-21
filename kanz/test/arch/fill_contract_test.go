package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE FILL FACT HAS ONE CONTRACT, AND EVERY BOOK OF RECORD READS IT (#631).
//
// # What went wrong without it
//
// order.order.filled has three independent PRODUCERS and six consumers, of which
// TWO fold it into a book of record — the OMS position projector and the
// accounting ledger (the IBOR). Those two are meant to reconcile with each other
// and with the exchange. They disagreed about what a valid fill is, and the
// position package disagreed with ITSELF:
//
//	                          fill_id==""   quantity<=0   venue==""
//	position/postgres.go        refused       refused      refused
//	position/book.go            ACCEPTED      refused      refused
//	accounting ledger.FromFill  ACCEPTED      ACCEPTED     ACCEPTED
//
// The fill_id row lost money silently. The ledger's entry id is "fill:" + FillId
// and Store.Append is ON CONFLICT DO NOTHING, so with an empty id the FIRST
// unidentified fill was journalled and every later one in that tenant was
// discarded as a duplicate — forever, with no error, no DLQ and no counter.
//
// Under such a producer the two books diverged in OPPOSITE directions: the
// position book parked the message and stopped, the ledger accepted one and
// swallowed the rest. Reconciliation reports a break with no way to attribute it.
//
// # Why this guard, in this shape
//
// test/arch/cash_subject_agreement_test.go already named this exact set as the
// known-unguarded precedent, in its own doc: "the codebase's existing precedent
// — the fill subjects, which accounting takes from config and the OMS publishes
// from its own literal — is duplication WITHOUT a check. This is the same trade
// with the check added." The check was added for cash and never for fills.
//
// Note WHICH HALF was copied: accounting's decodeFill is a byte-identical copy
// of the projector's and says so. The DECODE was mirrored. The VALIDATION was
// added later, to the position store, and did not — which is how "one
// implementation per concept" fails in practice. The copy is made once, and the
// fix lands on one side afterwards.
func TestFillSubjectsHaveOneOrigin(t *testing.T) {
	root := moduleRoot(t)
	const (
		filled    = "order.order.filled"
		partially = "order.order.partially_filled"
	)

	var offenders []string
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// gen/ is generated protobuf; test/ and *_test.go may name a subject
			// in a fixture, which is a test's business.
			if info.Name() == "gen" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/fillfact/") {
			return nil // the one origin
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || (v != filled && v != partially) {
				return true
			}
			if _, exempt := fillSubjectLiteralExempt[rel]; exempt {
				return true
			}
			offenders = append(offenders, rel+": writes the fill subject "+strconv.Quote(v)+" as a literal")
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// NON-VACUITY: this module has hundreds of non-test Go files. A walk that
	// scanned none would pass however scattered the contract had become.
	if scanned < 200 {
		t.Fatalf("scanned only %d non-test Go file(s) — the walk is broken and this guard "+
			"proves nothing", scanned)
	}

	for rel := range fillSubjectLiteralExempt {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			offenders = append(offenders, "the exemption for "+rel+" is DEAD: no such file")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("%d place(s) restate the fill subject instead of reading internal/fillfact:\n\n  %s\n\n"+
			"There were NINE, across three producers and six consumers, and no place where "+
			"\"what a fill must carry\" was stated once — which is how two books of record came to "+
			"disagree about it. Use fillfact.SubjectFilled / fillfact.SubjectPartiallyFilled.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// fillSubjectLiteralExempt names non-test files allowed to write a fill subject
// literally. Empty today, and that is the point: nine sites were retired into
// internal/fillfact and none needed to stay. An entry here is permission, and
// the dead-entry arm above removes it when the file goes.
var fillSubjectLiteralExempt = map[string]string{}

// EVERY BOOK OF RECORD FOLDS A FILL THROUGH THE SAME VALIDATION (#631).
//
// Retiring the subject literals makes the two books read one SUBJECT. This
// asserts the other half — that they apply one RULE — because the subjects
// agreeing while the validity rules diverge is exactly the state this issue
// describes, and the more dangerous half: a consumer reading the right subject
// and booking a fill the other consumer refuses.
//
// It checks a CALL to fillfact.Validate, from the AST, not an import: importing
// the package for its subject constants proves nothing about validation, and
// both of these packages now do exactly that.
func TestEveryBookOfRecordValidatesTheFill(t *testing.T) {
	root := moduleRoot(t)

	// The two packages that fold order.order.filled into durable state. Named
	// rather than discovered: "is this a book of record" is a judgement about
	// what the data MEANS, and a heuristic over it would be a guard that quietly
	// changed scope when someone added a package.
	books := map[string]string{
		"services/oms/internal/position":      "the OMS position projector",
		"services/accounting/internal/ledger": "the accounting ledger (the IBOR)",
	}

	var problems []string
	for pkg, what := range books {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		if !callsFillfactValidate(t, dir) {
			problems = append(problems, pkg+" ("+what+") never calls fillfact.Validate. It folds "+
				"order.order.filled into a book of record, so it decides which fills exist — and if "+
				"it applies a different rule than the other book, the two diverge on the same FACT "+
				"and reconciliation reports a break nobody can attribute.")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d book(s) of record do not share the fill contract:\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// callsFillfactValidate reports whether any non-test file directly in dir calls
// fillfact.Validate, resolving the package by import path so an aliased import
// is judged on what it actually calls.
func callsFillfactValidate(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
		}
		alias := ""
		for _, imp := range file.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || p != modulePath+"/internal/fillfact" {
				continue
			}
			alias = "fillfact"
			if imp.Name != nil {
				alias = imp.Name.Name
			}
		}
		if alias == "" {
			continue
		}
		found := false
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "Validate" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == alias {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

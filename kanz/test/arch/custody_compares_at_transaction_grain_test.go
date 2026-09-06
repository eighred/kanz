package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A CUSTODY COMPARISON MUST CARRY THE TRANSACTION GRAIN AS WELL AS THE NET (#1049).
//
// # The defect this exists to keep repaired
//
// custody reconciliation compared NET BALANCES ONLY. recon.Statement held
// positions and cash and nothing else, and a total is blind to its own
// composition: twelve fills on one instrument, one of them booked against an
// execution the custodian never settled and one the custodian settled that the
// book never heard of, produce IDENTICAL position and cash totals and reconcile
// CLEAN. The estate's own alert conceded the inference it was left with —
// "missing_in_ibor is the direction that MOST OFTEN means a fill never reached
// the book".
//
// # Why the existing custody guards do not cover it
//
// custody_book_is_scoped_to_the_custodian_test.go is a strong, default-deny,
// AST-derived guard over every recon.Reconcile call site — and the property it
// enforces is WHICH holdings are compared, not at what GRAIN. It was true
// throughout the netted-only era and stays true if the executions argument is
// passed as nil at every call site. custody_control_is_complete_test.go states its
// own limit in-file: it proves the pieces still refer to each other.
//
// # What this asserts, and why each arm has a way of failing that compiles
//
//  1. EVERY recon.Reconcile CALL SITE PASSES REAL EXECUTIONS, bound from a call
//     that was handed the custody subject. `recon.Reconcile(book, nil, stmt, tol)`
//     compiles, passes every test that does not configure trade lines, and
//     silently returns the control to the netted grain — with the leg reporting
//     that it ran over an empty book side. This is the transaction-grain twin of
//     the hop #1006 lived on, and it is DEFAULT-DENY so a new caller has to
//     satisfy it rather than be remembered.
//
//  2. THE PASS IS ACTUALLY REACHED FROM Reconcile and its breaks join the ones the
//     netted arms produced. A transactionLeg that exists, is tested, and is called
//     by nothing is the state posttrade.Reconcile has been in since #589 — a real
//     matcher with no caller.
//
//  3. NEITHER FUNCTION SWITCHES ITSELF OFF WITH A BOOLEAN CONSTANT. `false &&` in
//     a refusal has shipped twice here (#771, #957) with every symbol a guard
//     reads still present, and golangci-lint does not flag it.
//
//  4. recon.Grains() NAMES EVERY DECLARED GRAIN. The composition root seeds one
//     posture series per grain from it, so an omitted grain exports NO series —
//     absent rather than zero, which is the state the posture exists to abolish.
//
// AST, NOT GREP, for the reason the sibling guards give: a regex over this file's
// own prose matches its own words. Only arm 4 reads text, and it reads the
// const block and the function body with comments stripped.

const (
	reconPkgRel        = "../../services/accounting/internal/recon"
	transactionLegFunc = "transactionLeg"
	reconcileFunc      = "Reconcile"
)

// TestEveryCustodyComparisonIsHandedItsExecutions is arm 1: the default-deny
// sweep over every recon.Reconcile call site in the module.
func TestEveryCustodyComparisonIsHandedItsExecutions(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	type site struct {
		fn   string
		pos  token.Position
		call *ast.CallExpr
		decl *ast.FuncDecl
	}
	var sites []site

	walkModuleGoFiles(t, root, fset, func(_ string, f *ast.File) {
		local := reconLocalName(f)
		inRecon := f.Name.Name == reconPackageName
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall || !isReconReconcileCall(call, local, inRecon) {
					return true
				}
				sites = append(sites, site{fn: fd.Name.Name, pos: fset.Position(call.Pos()), call: call, decl: fd})
				return true
			})
		}
	})

	// NON-VACUITY FIRST. Two callers exist — the scheduled reconciler and the
	// ad-hoc HTTP handler. Finding fewer means the walk or the package moved, and
	// every assertion below would then pass by not looking.
	if len(sites) < 2 {
		t.Fatalf("found %d call site(s) of %s.%s in the module, want at least 2. This guard derives its "+
			"caller set by walking the module for that call; finding almost none means it is reading "+
			"nothing — repair the derivation rather than trusting the pass.",
			len(sites), reconPackageName, reconcileFunc)
	}

	for _, s := range sites {
		if len(s.call.Args) < 2 {
			t.Errorf("%s: %s.%s was called with %d argument(s) — the signature changed and this guard "+
				"cannot see the transaction grain. Repair it rather than deleting it.",
				s.pos, reconPackageName, reconcileFunc, len(s.call.Args))
			continue
		}
		executions, ok := s.call.Args[1].(*ast.Ident)
		if !ok || strings.EqualFold(executions.Name, "nil") {
			t.Errorf("%s: %s hands %s.%s no executions (argument 2 is %s).\n\n"+
				"That is the netted grain, restored, with the transaction leg reporting that it RAN "+
				"over an empty book side: every trade line the custodian sent comes back as an "+
				"execution the book has no entry for, and every execution the book holds goes "+
				"unchecked. Pass the executions the same loader returned with the book.",
				s.pos, s.fn, reconPackageName, reconcileFunc, describeArg(s.call.Args[1]))
			continue
		}
		subjects := subjectIdents(s.decl)
		if len(subjects) == 0 {
			// The sibling guard reports this case with its own message; not
			// repeating it here, because a function with no subject fails there
			// first and for a more fundamental reason.
			continue
		}
		producers := definingCalls(s.decl, executions.Name)
		if len(producers) == 0 {
			t.Errorf("%s: the executions %q handed to %s.%s are not assigned from a call in %s.\n\n"+
				"The book side of the transaction pass must be PRODUCED by something the custody "+
				"subject was handed to — executions that arrive any other way are over a different "+
				"slice of the journal than the book they are compared beside, and an execution from "+
				"another custodian's account reports as one THIS custodian never settled.",
				s.pos, executions.Name, reconPackageName, reconcileFunc, s.fn)
			continue
		}
		for _, c := range producers {
			if !callTakesOneOf(c, subjects) {
				t.Errorf("%s: %s builds the executions %q WITHOUT passing it the custody subject (%s).\n\n"+
					"This is the hop #1006 lived on, at the transaction grain: a whole-portfolio read "+
					"compiles, looks like a list of executions, and drops the custodian at the one "+
					"call that decides which of them are compared.",
					s.pos, s.fn, executions.Name, strings.Join(subjects, ", "))
			}
		}
	}
}

// describeArg renders an argument for a failure message without pulling in a
// printer: enough to identify what was passed.
func describeArg(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return "a field selector"
	case *ast.CallExpr:
		return "a call result"
	case *ast.CompositeLit:
		return "a composite literal"
	}
	return "an expression this guard cannot name"
}

// TestTheTransactionLegIsReachedAndNotConstantFolded is arms 2 and 3.
func TestTheTransactionLegIsReachedAndNotConstantFolded(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	funcs := map[string]*ast.FuncDecl{}
	files := 0
	walkGoFiles(t, root, "services/accounting/internal/recon", fset, func(_ string, f *ast.File) {
		files++
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	})
	if files == 0 {
		t.Fatalf("no non-test .go file was parsed under the recon package — the comparison engine moved " +
			"and this guard is reading nothing. Point it at the new path rather than deleting it.")
	}

	leg := funcs[transactionLegFunc]
	if leg == nil {
		t.Fatalf("recon declares no %s. The transaction grain is the whole of #1049: without it the "+
			"comparison is netted balances only, and a wrongly-booked execution cancels against a "+
			"missing one on the same instrument. Repair this guard against the replacement rather "+
			"than deleting it.", transactionLegFunc)
	}
	reconcile := funcs[reconcileFunc]
	if reconcile == nil {
		t.Fatalf("recon declares no %s — this guard cannot see whether the transaction pass is reached", reconcileFunc)
	}

	// ARM 2: Reconcile must CALL the leg. A matcher with no caller is the state
	// posttrade.Reconcile has been in since #589 — real, tested and unreached.
	if !callsFunction(reconcile, transactionLegFunc) {
		t.Errorf("recon.%s never calls %s.\n\n"+
			"The transaction pass then exists, is tested, and runs for nobody: every custody run is "+
			"back at the netted grain and the break kinds that name an execution can never be "+
			"produced. That is a real matcher with no caller, which this platform already has one of.",
			reconcileFunc, transactionLegFunc)
	}

	// ARM 3: neither function may switch itself off with a boolean constant. A
	// `false &&` in a condition leaves every symbol a guard reads in place, and
	// golangci-lint does not flag it (confirmed in #771 and #957).
	for name, fd := range map[string]*ast.FuncDecl{reconcileFunc: reconcile, transactionLegFunc: leg} {
		if where := boolConstantsInConditions(fset, fd); len(where) > 0 {
			sort.Strings(where)
			t.Errorf("recon.%s has a boolean CONSTANT in a condition at %s.\n\n"+
				"`if false`, `false &&` and `true ||` disable a comparison while leaving every "+
				"identifier a guard reads exactly where it was. That has shipped twice in this "+
				"repository, and the fourth gate does not catch it.",
				name, strings.Join(where, ", "))
		}
	}
}

// boolConstantsInConditions returns the positions of `true`/`false` literals used
// in an if-condition or as an operand of a logical operator inside fd.
func boolConstantsInConditions(fset *token.FileSet, fd *ast.FuncDecl) []string {
	var out []string
	isBool := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && (id.Name == "true" || id.Name == "false")
	}
	ast.Inspect(fd, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			if isBool(node.Cond) {
				out = append(out, fset.Position(node.Cond.Pos()).String())
			}
		case *ast.BinaryExpr:
			if node.Op != token.LAND && node.Op != token.LOR {
				return true
			}
			if isBool(node.X) {
				out = append(out, fset.Position(node.X.Pos()).String())
			}
			if isBool(node.Y) {
				out = append(out, fset.Position(node.Y.Pos()).String())
			}
		}
		return true
	})
	return out
}

// TestGrainsNamesEveryDeclaredGrain is arm 4.
//
// The accounting composition root seeds one series of
// kanz_accounting_custody_statement_grain_produced per grain FROM recon.Grains(),
// so a grain declared on the type and omitted from that slice exports no series at
// all — absent rather than zero, which is exactly the state the posture exists to
// abolish. Same shape, and same reason, as TestOutcomesNamesEveryDeclaredOutcome.
func TestGrainsNamesEveryDeclaredGrain(t *testing.T) {
	src := readStripped(t, filepath.Join(reconPkgRel, "transaction.go"))

	block := regexp.MustCompile(`(?s)const \(\s*\n\s*GrainUnknown Grain = iota(.*?)\n\)`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("the Grain const block is no longer in the shape this guard derives from — update the " +
			"guard rather than letting it derive an empty set")
	}
	declared := regexp.MustCompile(`(?m)^\s*(Grain[A-Za-z]+)\s*$`).FindAllStringSubmatch(block[1], -1)
	if len(declared) == 0 {
		t.Fatal("derived NO declared grains — the guard would pass vacuously")
	}

	listed := regexp.MustCompile(`func Grains\(\) \[\]Grain \{ return \[\]Grain\{([^}]*)\}`).FindStringSubmatch(src)
	if listed == nil {
		t.Fatal("recon.Grains() not found, or its body is no longer a single literal slice")
	}

	var missing []string
	for _, d := range declared {
		if !strings.Contains(listed[1], d[1]) {
			missing = append(missing, d[1])
		}
	}
	// GrainUnknown is the iota head and is captured by the block regexp rather
	// than the member regexp, so it is asserted directly.
	if !strings.Contains(listed[1], "GrainUnknown") {
		missing = append(missing, "GrainUnknown")
	}
	if len(missing) > 0 {
		t.Fatalf("Grains() omits %v.\n\nThe composition root seeds the grain posture from it, so an "+
			"omitted grain exports NO series until a statement first declares it — and a deployment "+
			"whose custodian sends only balances is precisely the one with no sample. Grains() is the "+
			"source of truth for the label set; add the member there.", missing)
	}
}

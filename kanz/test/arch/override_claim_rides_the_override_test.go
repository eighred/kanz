package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// THE DUAL-CONTROL CLAIM RIDES THE OVERRIDE IT AUTHORISES (#807).
//
// # What this is protecting
//
// The pricing override's approve path claimed the proposal in one transaction
// and applied the override in another. The ERROR path between them was answered
// loudly — a log line and a 400. The CRASH path was not, and could not be: a
// process death between the two commits consumed the proposal, applied nothing,
// and left no row, FACT or log line saying an approval had been given.
//
// The cost is a spent maker-checker signature. The exception goes on reading as
// OPEN while the proposal that would have closed it reads as decided, so a
// mispriced instrument keeps valuing the book until a human notices the
// contradiction and a second approver signs again.
//
// # The two things that must both stay true
//
//  1. THE CLAIM IS A STATEMENT IN THE OVERRIDE'S OWN TRANSACTION. Not a call
//     before it, and not a compensator after it. PostgresExceptions.Override
//     opens the transaction, so the claim has to happen inside that method.
//
//  2. THE APPROVE HANDLER DOES NOT CLAIM SEPARATELY. Passing store.Claim to
//     Override is worthless if the handler also claims first — that restores the
//     window with the fix still in place, and every unit test would keep passing
//     because in a test nothing dies between two calls.
//
// The rejection path still calls ProposalStore.Claim on its own, and that is
// correct rather than exempted: removing the row IS the whole of a rejection, so
// there is no second write for it to be separable from. What guard (2) forbids
// is one function doing both.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. Every paragraph
// on this page names Claim and Override; none of it can satisfy anything below.

const (
	datamasterStoreFile     = "services/datamaster/internal/store/postgres.go"
	datamasterProposalsFile = "services/datamaster/internal/store/proposals.go"
	datamasterApprovePkg    = "services/datamaster/internal/server"
)

func TestTheOverrideClaimRidesTheOverrideTransaction(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// Mode 0: comments are not attached.
	storeFile, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(datamasterStoreFile)), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", datamasterStoreFile, err)
	}
	var overrideBody *ast.BlockStmt
	storeMethods := map[string]bool{}
	for _, decl := range storeFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv == nil {
			continue
		}
		if proposalReceiverName(fn.Recv.List[0].Type) != "PostgresExceptions" {
			continue
		}
		storeMethods[fn.Name.Name] = true
		if fn.Name.Name == "Override" {
			overrideBody = fn.Body
		}
	}

	// NON-VACUITY 1: the method was found. A rename or a move would otherwise
	// leave every assertion below reading an absent function.
	if overrideBody == nil {
		t.Fatalf("no Override method on PostgresExceptions in %s — the durable override moved, and "+
			"this guard is asserting nothing about where its claim is written", datamasterStoreFile)
	}
	// NON-VACUITY 2: this really is the durable exception store.
	for _, must := range []string{"Add", "Get", "Open"} {
		if !storeMethods[must] {
			t.Fatalf("PostgresExceptions in %s has no %s method — this is not the store this guard "+
				"means to read", datamasterStoreFile, must)
		}
	}

	calls := selectorNamesIn(overrideBody)
	sql := strings.Join(proposalSQLIn(overrideBody), "\n")

	// THE CLAIM MUST RUN ON THE TRANSACTION THIS METHOD OPENED, not on the pool.
	// Both compile and both read as "Override claims the proposal"; only one of
	// them rolls the claim back when the override fails. `claimProposal(ctx,
	// p.pool, ...)` is the whole of the original defect expressed in one
	// argument, so the argument is what this checks.
	txName := transactionVarIn(overrideBody)
	if txName == "" {
		t.Fatalf("PostgresExceptions.Override in %s no longer assigns the result of a Begin call, "+
			"so this guard cannot tell which value is the transaction", datamasterStoreFile)
	}
	switch on := claimProposalTargetIn(overrideBody); on {
	case txName:
		// The claim rides the override's transaction.
	case "":
		t.Error("PostgresExceptions.Override no longer calls claimProposal, so it does not consume " +
			"the dual-control proposal at all.\n\n" +
			"The claim has to be a statement in THIS transaction. A claim that commits separately — " +
			"before this method or after it — reopens the window #807 closed: a process death " +
			"between the two commits spends the second signature and applies nothing, leaving the " +
			"exception OPEN, the proposal gone, and no record anywhere that anyone approved it.\n\n" +
			"NO TEST WILL CATCH THIS in memory: the in-process backend's two halves die together, " +
			"so only the Postgres-gated test can see the difference.")
	default:
		t.Errorf("PostgresExceptions.Override claims the proposal on %q rather than on its own "+
			"transaction %q.\n\nThat is the #807 defect exactly: the DELETE commits on its own "+
			"connection, so the rollback that undoes the audit row, the status flip and the FACT "+
			"CANNOT undo it. A failure — or a process death — after that point leaves the second "+
			"signature spent and the override unapplied, with nothing anywhere recording either.",
			on, txName)
	}
	if !calls["Begin"] || !calls["Commit"] {
		t.Error("PostgresExceptions.Override no longer opens and commits its own transaction, so " +
			"there is nothing for the claim, the audit row, the status flip and the FACT to ride " +
			"together (#410, #807).")
	}
	if !calls["Enqueue"] {
		t.Error("PostgresExceptions.Override no longer enqueues the override FACT in its own " +
			"transaction (#410). Committing the override and publishing separately leaves a " +
			"decision that is durable here and absent from the platform's audit trail.")
	}
	// The audit row and the status flip are the state change the claim must not
	// be separable from; if they left this method the claim would be riding
	// nothing.
	if !strings.Contains(sql, "exception_overrides") || !strings.Contains(sql, "UPDATE exceptions") {
		t.Errorf("PostgresExceptions.Override no longer writes the audit row and the status flip "+
			"itself, so the claim inside it is riding a transaction that changes nothing. SQL "+
			"found: %s", sql)
	}

	// AND THE CLAIM STATEMENT IS SPELLED ONCE. Two copies is how the approval
	// path and the rejection path come to consume a proposal differently, and the
	// one that drifts is the one an auditor reads.
	proposalsFile, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(datamasterProposalsFile)), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", datamasterProposalsFile, err)
	}
	deleters := 0
	sawClaimProposal := false
	for _, decl := range proposalsFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Name.Name == "claimProposal" {
			sawClaimProposal = true
		}
		for _, lit := range proposalSQLIn(fn.Body) {
			if strings.Contains(lit, "DELETE FROM exception_override_proposals WHERE proposal_id") {
				deleters++
			}
		}
	}
	if !sawClaimProposal {
		t.Fatalf("no claimProposal function in %s — the shared claim statement is gone, and the "+
			"assertion above is matching an identifier that means something else", datamasterProposalsFile)
	}
	if deleters != 1 {
		t.Errorf("%d functions in %s spell the single-proposal DELETE, want exactly 1. The approval "+
			"path takes it on the override's transaction and the rejection path takes it on the "+
			"pool; written twice they drift, and the audit trail reads whichever drifted.",
			deleters, datamasterProposalsFile)
	}
}

// THE APPROVE HANDLER MUST NOT CLAIM AND OVERRIDE AS TWO CALLS (#807).
//
// Guard 1 puts the claim inside the transaction. This one stops the handler
// claiming first anyway, which would restore the window with the repair still in
// the store — and no unit test would notice, because nothing dies between two
// calls in a test.
func TestTheApproveHandlerDoesNotClaimSeparately(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(datamasterApprovePkg))
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	sawClaim, sawOverride := false, false
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", p, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			called := calledMethodsIn(fn.Body)
			if called["Claim"] {
				sawClaim = true
			}
			if called["Override"] {
				sawOverride = true
			}
			if called["Claim"] && called["Override"] {
				offenders = append(offenders, fn.Name.Name)
			}
		}
	}

	// NON-VACUITY: both calls still exist somewhere in this package. Without
	// this the guard passes cleanly on a package where the override surface was
	// deleted, or where the analysis stopped seeing calls at all.
	if !sawClaim {
		t.Fatal("nothing in " + datamasterApprovePkg + " calls Claim any more — the rejection path " +
			"is what should still be calling it, so either it moved or this analysis sees nothing")
	}
	if !sawOverride {
		t.Fatal("nothing in " + datamasterApprovePkg + " calls Override any more — this guard is " +
			"reading a package that no longer applies pricing overrides")
	}
	if len(offenders) > 0 {
		t.Errorf("these functions both claim a proposal and apply an override: %v.\n\n"+
			"That is the two-transaction shape #807 removed. The approval must hand its claim to "+
			"Override (store.Claim{ProposalID: ...}) so one commit covers the claim, the audit row, "+
			"the status flip and the FACT. Claiming first spends the second signature before the "+
			"override runs, and a process death in that window applies nothing and records "+
			"nothing.\n\nA rejection legitimately calls Claim ALONE — removing the row is the whole "+
			"decision. What is forbidden is one function doing both.", offenders)
	}
}

// transactionVarIn returns the name a body binds the result of a `.Begin(` call
// to — the transaction everything in that body is supposed to ride.
func transactionVarIn(body *ast.BlockStmt) string {
	name := ""
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Begin" {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && name == "" {
			name = id.Name
		}
		return true
	})
	return name
}

// claimProposalTargetIn renders the handle claimProposal is called on — its
// second argument. Empty means it is not called at all; a non-identifier
// argument renders as its type so the failure message can still name it.
func claimProposalTargetIn(body *ast.BlockStmt) string {
	target := ""
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "claimProposal" || len(call.Args) < 2 {
			return true
		}
		switch arg := call.Args[1].(type) {
		case *ast.Ident:
			target = arg.Name
		case *ast.SelectorExpr:
			target = dottedSelectorText(arg)
		default:
			target = "an expression this guard cannot name"
		}
		return true
	})
	return target
}

// dottedSelectorText renders a dotted selector like `p.pool` for an error message.
func dottedSelectorText(sel *ast.SelectorExpr) string {
	if id, ok := sel.X.(*ast.Ident); ok {
		return id.Name + "." + sel.Sel.Name
	}
	return "." + sel.Sel.Name
}

// calledMethodsIn returns the method names CALLED on some value in a body:
// `s.proposals.Claim(...)` registers as "Claim". It deliberately ignores plain
// field selection, so naming a field is not mistaken for invoking it.
func calledMethodsIn(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			out[sel.Sel.Name] = true
		}
		return true
	})
	return out
}

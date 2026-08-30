package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY REFUSED FILL FREEZES THE ORDER (#808).
//
// # What this is protecting
//
// An ApplyFill refusal reached the platform on two paths and got two different
// answers. adopt() — the RECOVERY path — quarantined, which is correct: a fill
// the aggregate refuses is a disagreement about what the order IS, and
// re-driving cannot resolve it. work() — the LIVE path — logged an ERROR and
// broke out of the loop, then returned nil so admission published an ACCEPTED
// CommandOutcome.
//
// The cost of the weaker answer, on the path that matters more: a venue
// over-fill is a position THE FUND HOLDS with no ORDER_FILLED FACT, no
// projection, no ledger entry and no alertable counter — discoverable only by
// reading logs, while the order reports ACCEPTED. Risk, compliance and the IBOR
// all measure a book missing the execution.
//
// # Why a guard and not only the tests
//
// The behavioural tests pin the two call sites that exist TODAY. What they
// cannot notice is a THIRD — a new recovery path, a replay tool, a backfill —
// answering the same refusal a third way. There were two sites and they already
// disagreed; that is the whole history of this defect.
//
// # Why it inspects the ERROR BRANCH and not the function
//
// The first draft of this guard asked whether a function calling ApplyFill also
// called quarantine ANYWHERE in its body. It passed against the pre-fix code —
// because work() already quarantines for an unrelated reason one block up (a
// conflicting venue ack), so the weaker question was satisfied by a call that
// had nothing to do with the refused fill. Mutation caught it: the defect was
// restored, the behavioural test failed, and the guard stayed green.
//
// That is this repository's most familiar failure in a guard, so the property is
// now spelled exactly: find the assignment whose right-hand side calls
// ApplyFill, take the error identifier it binds, find the `if <err> != nil`
// that follows, and require THAT BLOCK to reach quarantine.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. The paragraph you
// are reading cannot satisfy anything below.

const omsOrderDir = "services/oms/internal/order"

// applyFillQuarantineExempt names a function that may call ApplyFill without
// reaching quarantine, and why.
//
// DEFAULT-DENY. An entry is a claim that the caller is NOT deciding what the
// venue's record means — a pure validator, say — and it must say which.
//
// IT IS EMPTY, and it should stay that way. Every production caller of ApplyFill
// is folding a venue's own report into the book of record, and the only honest
// answer to a report that does not fit is to stop and ask a human.
var applyFillQuarantineExempt = map[string]string{}

func TestEveryRefusedFillReachesQuarantine(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(omsOrderDir))

	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", omsOrderDir, err)
	}

	fset := token.NewFileSet()
	// funcs that CALL ApplyFill -> whether they also reach quarantine.
	callers := map[string]bool{}
	quarantines := map[string]bool{}
	sawQuarantineDecl := false

	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		// Mode 0: comments are not attached.
		file, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", p, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name == "quarantine" {
				sawQuarantineDecl = true
			}
			if handled, found := applyFillRefusalHandled(fn.Body); found {
				callers[fn.Name.Name] = true
				if handled {
					quarantines[fn.Name.Name] = true
				}
			}
		}
	}

	// NON-VACUITY 1: the scan found the aggregate call at all.
	if len(callers) == 0 {
		t.Fatalf("no function in %s was seen calling ApplyFill — the fold moved or was renamed, "+
			"and this guard is asserting nothing", omsOrderDir)
	}
	// NON-VACUITY 2: there are at least the two the defect was about. A scan that
	// found one would pass while the other disagreed, which is the exact state
	// #808 describes.
	if len(callers) < 2 {
		t.Fatalf("only %d caller of ApplyFill was found (%v) — #808 was about TWO paths answering "+
			"one refusal differently, so a scan seeing one of them cannot compare them",
			len(callers), sortedKeys(callers))
	}
	// NON-VACUITY 3: quarantine still exists to be reached.
	if !sawQuarantineDecl {
		t.Fatalf("no quarantine function in %s — the freeze this guard requires callers to reach "+
			"no longer exists", omsOrderDir)
	}

	var unfrozen []string
	for fn := range callers {
		if quarantines[fn] {
			continue
		}
		if reason, ok := applyFillQuarantineExempt[fn]; ok {
			t.Logf("%s: exempt — %s", fn, reason)
			continue
		}
		unfrozen = append(unfrozen, fn)
	}

	if len(unfrozen) > 0 {
		sort.Strings(unfrozen)
		t.Errorf("these functions fold a venue fill through ApplyFill and never reach quarantine: "+
			"%v.\n\n"+
			"A fill the aggregate refuses is not a transient fault and re-driving cannot help — it "+
			"is a disagreement about what the order IS, and only a human comparing it against the "+
			"venue's own order history can resolve it. Answering with a log line leaves a position "+
			"the fund HOLDS with no FACT, no projection, no ledger entry and no alertable counter, "+
			"while the order reports ACCEPTED (#808).\n\n"+
			"Route the refusal to quarantine, or add the function to applyFillQuarantineExempt "+
			"with the argument that it is not deciding what the venue's record means.", unfrozen)
	}

	// DEAD-ENTRY ARM.
	for fn, reason := range applyFillQuarantineExempt {
		if !callers[fn] {
			t.Errorf("exemption for %q (%s) names no ApplyFill caller — delete it", fn, reason)
		}
	}
}

// applyFillRefusalHandled reports whether a body calls ApplyFill (found) and
// whether the branch taken when it REFUSES reaches quarantine (handled).
//
// It walks statement LISTS rather than the whole tree, because the property is
// positional: the guard has to pair one assignment with the `if err != nil` that
// immediately follows it. A whole-tree walk is what made the first draft vacuous
// — it could see a quarantine call anywhere and call that an answer.
func applyFillRefusalHandled(body *ast.BlockStmt) (handled, found bool) {
	handled, found = true, false
	var walk func(stmts []ast.Stmt)
	walk = func(stmts []ast.Stmt) {
		for i, st := range stmts {
			if errName, ok := applyFillErrName(st); ok {
				found = true
				// The refusal branch is the next statement, and it must be an
				// `if <errName> != nil` whose body reaches quarantine.
				if i+1 >= len(stmts) || !branchQuarantines(stmts[i+1], errName) {
					handled = false
				}
			}
			// Descend into nested blocks — the fold loops are inside for/if.
			switch v := st.(type) {
			case *ast.ForStmt:
				if v.Body != nil {
					walk(v.Body.List)
				}
			case *ast.RangeStmt:
				if v.Body != nil {
					walk(v.Body.List)
				}
			case *ast.IfStmt:
				if v.Body != nil {
					walk(v.Body.List)
				}
				if v.Else != nil {
					if b, ok := v.Else.(*ast.BlockStmt); ok {
						walk(b.List)
					}
				}
			case *ast.BlockStmt:
				walk(v.List)
			case *ast.SwitchStmt:
				if v.Body != nil {
					walk(v.Body.List)
				}
			case *ast.CaseClause:
				walk(v.Body)
			}
		}
	}
	walk(body.List)
	return handled, found
}

// applyFillErrName returns the error identifier an ApplyFill assignment binds.
func applyFillErrName(st ast.Stmt) (string, bool) {
	as, ok := st.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 || len(as.Lhs) != 2 {
		return "", false
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return "", false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != "ApplyFill" {
		return "", false
	}
	errID, ok := as.Lhs[1].(*ast.Ident)
	if !ok {
		return "", false
	}
	return errID.Name, true
}

// branchQuarantines reports whether st is `if <errName> != nil { … }` and its
// body reaches quarantine.
func branchQuarantines(st ast.Stmt, errName string) bool {
	ifs, ok := st.(*ast.IfStmt)
	if !ok || ifs.Body == nil {
		return false
	}
	bin, ok := ifs.Cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	lhs, ok := bin.X.(*ast.Ident)
	if !ok || lhs.Name != errName {
		return false
	}
	return selectorNamesIn(ifs.Body)["quarantine"]
}

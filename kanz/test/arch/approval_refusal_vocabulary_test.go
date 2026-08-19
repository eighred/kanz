package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// EVERY WAY AN APPROVAL CAN BE REFUSED MUST HAVE A NAME THE APPROVER IS GIVEN (#558).
//
// # What this is protecting
//
// A refused approval used to tell the approver nothing at all. It now writes a
// reason code onto the proposal, and ListPendingApprovals hands that code to the
// approver's client, which branches on it: "self_approval" means find another
// approver, "payload_changed" means the terms moved and the order must be
// re-proposed. Those are different actions.
//
// approvalRefusal maps internal/dualcontrol's sentinel errors onto that closed
// set with a DEFAULT ARM that answers "unclassified" — meaning "refused, and this
// OMS cannot say why". A fourth sentinel added to dualcontrol — for a fourth act,
// a revocation, an entitlement — would fall into that arm, and the approver would
// be shown a shrug for a refusal the platform can name perfectly well. Nothing
// else catches it: the code compiles, every existing test passes, and the wrong
// answer is a string on a screen rather than a failure anywhere.
//
// # Why the guard is here and not a test in the order package
//
// The two files it compares are in different modules of the repository's
// dependency graph — internal/dualcontrol knows nothing about the OMS, which is
// the whole reason the rule lives in one shared package. A test inside either one
// can only see its own half.
//
// # What it deliberately does NOT check
//
// That the labels are the right words, or that a client renders them. It checks
// that the vocabulary is EXHAUSTIVE over the errors that can reach it, which is
// the property whose violation is silent.
func TestEveryDualControlRefusalHasAnApproverFacingReason(t *testing.T) {
	root := moduleRoot(t)

	sentinels := dualControlSentinels(t, filepath.Join(root, "internal", "dualcontrol", "dualcontrol.go"))
	// NON-VACUITY. If the parse found nothing, every assertion below passes while
	// checking an empty set — the failure mode a guard is least able to notice
	// about itself.
	if len(sentinels) < 4 {
		t.Fatalf("found %d Err* sentinels in internal/dualcontrol (%v) — the package moved or "+
			"its errors were renamed, and this guard is now comparing an empty set", len(sentinels), sentinels)
	}

	handled := errorsNamedIn(t,
		filepath.Join(root, "services", "oms", "internal", "order", "service.go"),
		"approvalRefusal")
	if len(handled) == 0 {
		t.Fatal("approvalRefusal names no dualcontrol error at all — either it was renamed or " +
			"its switch no longer distinguishes refusals, and every approver is now told the " +
			"same thing whatever refused them")
	}

	for _, s := range sentinels {
		if !handled[s] {
			t.Errorf("internal/dualcontrol.%s is not named in approvalRefusal's switch.\n\n"+
				"It falls into the default arm, so an approver refused for this reason is told "+
				"%q — on the queue and in the log — for a refusal this platform can name. Add a "+
				"case and a Refusal* constant beside the others.", s, "unclassified")
		}
	}
}

// THE TWO DUAL-CONTROL QUEUES MUST SPELL A STATE THE SAME WAY (#558, #563).
//
// # The divergence this catches actually happened
//
// datamaster's GET /v1/exceptions/pending-overrides renders "pending"/"lapsed";
// the OMS's ListPendingApprovals renders "pending"/"refused". They are the same
// control and the same concept, and they shipped with different CASING — the OMS
// said "PENDING" — so a client reading both needed two spellings for one word.
// Nothing failed: both queues were correct on their own, and the vocabulary is a
// string nobody compares across two services.
//
// So the rule is not "the sets must match" — they must NOT, and the constants say
// why. The rule is that neither queue may spell a state ITSELF. Every state
// rendered comes from an internal/dualcontrol constant, which is the one place a
// third act will look.
//
// # What it checks
//
// In each renderer: at least one dualcontrol.State* is used, and NO string
// literal that looks like a state appears — in any casing, so re-introducing
// "PENDING" beside the constant is caught too.
func TestNeitherDualControlQueueSpellsItsOwnState(t *testing.T) {
	root := moduleRoot(t)

	for _, q := range []struct {
		what string
		path string
		fn   string
	}{
		{
			"datamaster's pending-override queue",
			filepath.Join(root, "services", "datamaster", "internal", "server", "dualcontrol.go"),
			"handlePendingOverrides",
		},
		{
			"the OMS's pending-approval queue",
			filepath.Join(root, "services", "oms", "internal", "grpcsrv", "server.go"),
			"pendingApprovalOf",
		},
		// THE THIRD QUEUE (#562, #410 act two). Added the day it landed rather
		// than after it diverged, which is the only time this list is cheap to
		// extend — and the guard's name says NEITHER, so leaving a third renderer
		// outside it would be the overclaim #573 spent an issue on one file over.
		//
		// Its set is {pending, lapsed}: a refusal on this surface goes back on the
		// same HTTP request, so there is no window in which a refused approval
		// waits on a queue to be discovered. That is datamaster's shape, and the
		// reason the three sets differ is on the constants themselves.
		{
			"compliance's pending mandate-change queue",
			filepath.Join(root, "services", "compliance", "internal", "api", "api.go"),
			"handlePendingChanges",
		},
	} {
		used := selectorsOn(t, q.path, q.fn, "dualcontrol")
		var states int
		for name := range used {
			if strings.HasPrefix(name, "State") {
				states++
			}
		}
		// NON-VACUITY: a renderer that names no state constant is either not the
		// renderer any more, or is back to spelling its own.
		if states == 0 {
			t.Errorf("%s (%s) uses no internal/dualcontrol.State* constant — the state it "+
				"renders is its own, which is how the two queues came to disagree on casing "+
				"for one concept", q.what, q.fn)
		}
		for _, lit := range stringLiteralsIn(t, q.path, q.fn) {
			switch strings.ToLower(lit) {
			case "pending", "refused", "lapsed":
				t.Errorf("%s (%s) spells the state %q as a literal.\n\n"+
					"Use the internal/dualcontrol constant. A literal here is exactly how the "+
					"OMS came to render \"PENDING\" while datamaster rendered \"pending\" — "+
					"nothing fails, and a client reading both controls needs two spellings for "+
					"one word.", q.what, q.fn, lit)
			}
		}
	}
}

// stringLiteralsIn returns the string literals inside one function.
func stringLiteralsIn(t *testing.T, path, fn string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Name.Name == fn {
			body = fd.Body
		}
	}
	if body == nil {
		t.Fatalf("no func %s in %s — the queue renderer moved and this guard reads nothing", fn, path)
	}
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out = append(out, strings.Trim(lit.Value, "`\""))
		}
		return true
	})
	return out
}

// dualControlSentinels returns the names of the exported Err* variables the
// package declares.
func dualControlSentinels(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, n := range vs.Names {
				if strings.HasPrefix(n.Name, "Err") {
					out = append(out, n.Name)
				}
			}
		}
	}
	return out
}

// errorsNamedIn returns the dualcontrol.Err* selectors that appear inside one
// function.
//
// IT PARSES THE AST RATHER THAN GREPPING THE FILE, and that is not fastidiousness.
// A grep over source matches the guard's own prose, the doc comment above the
// function, and the neighbouring log call that mentions the same identifier — this
// repository has three guards that passed with the checked thing deleted for
// exactly that reason. Only the expressions in the function body count.
func errorsNamedIn(t *testing.T, path, fn string) map[string]bool {
	return selectorsOn(t, path, fn, "dualcontrol")
}

// selectorsOn returns the pkg.X selectors used inside one function.
func selectorsOn(t *testing.T, path, fn, pkg string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		// METHODS COUNT TOO. handlePendingOverrides is a method on *Server, and an
		// earlier form of this helper required fd.Recv == nil — so it silently found
		// nothing and the queue guard reported the function as missing rather than
		// checking it.
		if ok && fd.Name.Name == fn {
			body = fd.Body
		}
	}
	if body == nil {
		t.Fatalf("no func %s in %s — the refusal vocabulary moved, and this guard is reading "+
			"a function that no longer exists", fn, path)
	}
	found := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != pkg {
			return true
		}
		found[sel.Sel.Name] = true
		return true
	})
	return found
}

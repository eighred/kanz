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
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Name.Name == fn && fd.Recv == nil {
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
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "dualcontrol" {
			return true
		}
		found[sel.Sel.Name] = true
		return true
	})
	return found
}

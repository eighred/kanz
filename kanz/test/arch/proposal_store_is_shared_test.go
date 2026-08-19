package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sharedProposalStore    = "github.com/eighred/kanz/internal/dualcontrol/proposalstore"
	sharedProposalContract = sharedProposalStore + "/proposalstoretest"
)

// EVERY DUAL-CONTROL PROPOSAL STORE IS THE SHARED ONE (#562).
//
// # What this is protecting
//
// internal/dualcontrol holds the RULE. Storage is where the acts drifted: act
// one (datamaster's pricing override) and act three (the OMS's order submission)
// each wrote their own store, and by the time #562 was filed act one's
// in-process backend accepted three things its own Postgres backend refuses — a
// duplicate id, a proposal with no proposer, and a nil price — because it had no
// test at all while act three's had a contract run against both of its backends.
//
// #562's act two (a mandate change through the gateway) would have been the
// THIRD store. This guard is what makes writing it as a third private copy fail
// rather than merely be regrettable: a package that declares a ProposalStore
// must build it on internal/dualcontrol/proposalstore, and must run the shared
// contract against it.
//
// # Why a guard and not a paragraph
//
// The copy is the cheap path, and it looks correct in review: each store is
// individually reasonable, compiles, and passes its own tests. The divergence
// only shows up as a defect much later, in whichever backend nobody exercised —
// which is exactly how #563 happened. Nothing else in the suite catches a new
// private store, because a new private store breaks nothing.
//
// # What it deliberately does NOT check
//
// Which primitives an act composes, or what its Claim and expiry do. Those
// differ per act on purpose and each difference carries a reason (see the shared
// package's doc). What must not vary is that the mechanics come from one place
// and that the contract is run.
func TestEveryDualControlProposalStoreIsTheSharedOne(t *testing.T) {
	root := moduleRoot(t)

	owners := map[string]string{} // package dir -> the file declaring ProposalStore
	walkEveryGoFile(t, filepath.Join(root, "services"), func(path string, f *ast.File) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		if declaresTypeNamed(f, "ProposalStore") {
			owners[filepath.Dir(path)] = path
		}
	})

	// NON-VACUITY. Two acts ship a proposal store today. If the parse finds fewer,
	// the type was renamed or moved and every assertion below is passing over an
	// empty set — the failure a guard is least able to notice about itself.
	if len(owners) < 2 {
		t.Fatalf("found %d package(s) declaring a ProposalStore (%v), want at least the two "+
			"shipped acts — the type was renamed or moved and this guard is checking nothing",
			len(owners), owners)
	}

	for dir, decl := range owners {
		rel, _ := filepath.Rel(root, decl)
		if !dirImports(t, dir, false, sharedProposalStore) {
			t.Errorf("%s declares a ProposalStore without importing %s.\n\n"+
				"This is a private copy of proposal persistence. Two of them already "+
				"drifted: one act's in-process backend accepted a duplicate id, a proposal "+
				"with no proposer and a nil payload that its own Postgres backend refuses. "+
				"Compose the store out of proposalstore.Memory and proposalstore.Wellformed; "+
				"this act's Claim and expiry disposition stay its own.", rel, sharedProposalStore)
		}
		if !dirRunsTheProposalContract(t, dir) {
			t.Errorf("%s declares a ProposalStore, but nothing in its package calls "+
				"proposalstoretest.Run.\n\n"+
				"The contract is what stops a backend accepting what another refuses, and "+
				"it must be run against EVERY backend this act has — the in-process one "+
				"included, because that is the seam the service-level tests certify "+
				"against.", rel)
		}
	}
}

// THE SHARED STORE MUST NOT LEARN WHAT AN ACT'S PAYLOAD IS.
//
// internal/dualcontrol draws this boundary for the rule — it carries a digest
// rather than a payload, so it never grows an opinion about what a price or an
// order looks like. The storage half has the same exposure and a stronger pull
// towards crossing it: a shared table with a decoded payload column would need
// to know about both, and the moment it imports either act it is a dependency in
// both directions and can no longer be changed for one without the other.
func TestTheSharedProposalStoreKnowsNoActsPayload(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "internal", "dualcontrol", "proposalstore")

	seen := 0
	walkEveryGoFile(t, dir, func(path string, f *ast.File) {
		seen++
		rel, _ := filepath.Rel(root, path)
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			switch {
			case strings.HasPrefix(p, "github.com/eighred/kanz/services/"):
				t.Errorf("%s imports %s — the shared store now depends on one act, so it "+
					"cannot be changed for another act without changing that one too, and "+
					"the next act inherits a dependency on a service it has nothing to do "+
					"with", rel, p)
			case strings.HasPrefix(p, "github.com/eighred/kanz/kanz-schemas-go/"):
				t.Errorf("%s imports %s — a payload schema. The act hashes its own payload "+
					"and keeps it; a store that decodes one has an opinion about what is "+
					"being approved, which is the boundary dualcontrol.Proposal draws for "+
					"exactly this reason", rel, p)
			}
		}
	})
	if seen == 0 {
		t.Fatalf("no Go files found under %s — the package moved and this guard is checking "+
			"nothing", dir)
	}
}

// walkEveryGoFile parses every .go file under dir, TEST FILES INCLUDED (the
// suite's walkGoFiles skips them, and half of what this guard checks lives in
// them) and hands each to visit.
func walkEveryGoFile(t *testing.T, dir string, visit func(path string, f *ast.File)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v — the guard cannot check what it cannot parse", path, perr)
		}
		visit(path, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// declaresTypeNamed reports whether f declares a type of that name. PARSED, NOT
// GREPPED: a guard that matches raw source matches its own comments and the
// prose around them, and three guards in this suite have already passed with the
// checked thing deleted.
func declaresTypeNamed(f *ast.File, name string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if ok && ts.Name != nil && ts.Name.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// dirImports reports whether any file in dir imports path. tests selects the
// _test.go files rather than the production ones.
func dirImports(t *testing.T, dir string, tests bool, want string) bool {
	t.Helper()
	found := false
	walkEveryGoFile(t, dir, func(path string, f *ast.File) {
		if strings.HasSuffix(path, "_test.go") != tests || filepath.Dir(path) != dir {
			return
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == want {
				found = true
			}
		}
	})
	return found
}

// dirRunsTheProposalContract reports whether any test in dir CALLS
// proposalstoretest.Run. The import alone is not enough evidence: a package can
// import it for a type and never run a case.
func dirRunsTheProposalContract(t *testing.T, dir string) bool {
	t.Helper()
	found := false
	walkEveryGoFile(t, dir, func(path string, f *ast.File) {
		if !strings.HasSuffix(path, "_test.go") || filepath.Dir(path) != dir {
			return
		}
		alias := "proposalstoretest"
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == sharedProposalContract && imp.Name != nil {
				alias = imp.Name.Name
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Run" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == alias {
				found = true
			}
			return true
		})
	})
	return found
}

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

// ONE ANSWER TO "DOES THIS DIRECTORY BELONG TO THE ESTATE?" (#583-adjacent).
//
// # Why this exists
//
// Four guards walked the repo root, each with its own hand-written skip list,
// and the four had already drifted apart:
//
//	version_stamp_test.go          .git vendor node_modules .gotmp testdata
//	comments_cite_real_documents   vendor testdata .git node_modules
//	comments_cite_real_documents   .git node_modules vendor          (second walk)
//	supplychain_test.go            .git
//
// None of them excluded `.claude/worktrees/`, which is where this harness puts a
// subagent's checkout — a FULL COPY of the repository, Dockerfiles and all.
//
// # What that cost, observed rather than theorised
//
// TestEveryGoImageStampsItsVersion failed on 2026-08-19 with
//
//	walk C:\...\eighred-kanz: open .claude\worktrees\agent-abe7bbe704544afdf:
//	the system cannot find the file specified
//
// because an agent finished and its worktree was removed between the readdir and
// the open. That is the benign symptom. The dangerous one is silent: while a
// worktree exists, these guards read ANOTHER CHECKOUT'S files as if they were
// this one's — an agent mid-edit could fail a guard on `main`, or its copies
// could pad a non-vacuity count so a guard passes on evidence that is not the
// estate's.
//
// CI never sees either, because CI has no worktrees. So this is invisible where
// it is checked and live where the work happens.
//
// # What is NOT skipped, deliberately
//
// `.github` is a dot-directory this repository's guards must read —
// supplychain_test.go and the build-tag guard both parse workflows. So the rule
// is a named set, not "skip dot-directories".
func skipWalkDir(d fs.DirEntry) bool {
	if !d.IsDir() {
		return false
	}
	switch d.Name() {
	case ".git", ".claude", ".gotmp", "node_modules", "vendor":
		return true
	}
	return false
}

// TestTheWalkSkipSetExcludesAgentWorktrees pins the entry that is easy to lose:
// the other four names have been in these lists for months and `.claude` is the
// one somebody would tidy away as harness-specific.
func TestTheWalkSkipSetExcludesAgentWorktrees(t *testing.T) {
	if !skipWalkDir(fakeDir(".claude")) {
		t.Fatal(".claude is not skipped — guards that walk the repo root will read a subagent's " +
			"full checkout as if it were this one, and will crash when that checkout is removed " +
			"mid-walk. CI never sees this because CI has no worktrees.")
	}
	// AND THE ONE THAT MUST NOT BE SKIPPED. Two guards parse .github/workflows;
	// a blanket dot-directory rule would silently stop checking supply chain and
	// build tags, and both would pass by finding nothing.
	if skipWalkDir(fakeDir(".github")) {
		t.Fatal(".github is skipped — supplychain and build-tag coverage parse workflows from " +
			"there, and would pass vacuously")
	}
	for _, n := range []string{".git", "node_modules", "vendor"} {
		if !skipWalkDir(fakeDir(n)) {
			t.Errorf("%s is not skipped", n)
		}
	}
	// A FILE named .claude is not a directory and must not be skipped by name.
	if skipWalkDir(fakeFile(".claude")) {
		t.Error("a FILE named .claude was skipped — the check must be on directories")
	}
}

type fakeDirEntry struct {
	name string
	dir  bool
}

func (f fakeDirEntry) Name() string               { return f.name }
func (f fakeDirEntry) IsDir() bool                { return f.dir }
func (f fakeDirEntry) Type() fs.FileMode          { return 0 }
func (f fakeDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

func fakeDir(n string) fs.DirEntry  { return fakeDirEntry{name: n, dir: true} }
func fakeFile(n string) fs.DirEntry { return fakeDirEntry{name: n, dir: false} }

var _ = strings.TrimSpace

// EVERY LOCAL DIRECTORY-SKIP SET IN THIS PACKAGE NAMES .claude (#848).
//
// # The failure, which happened rather than being imagined
//
// skipWalkDir above exists because a guard walking the repo root descends into
// .claude/worktrees/ and reads ANOTHER CHECKOUT'S files as if they were the
// estate's. The helper was written and then adopted by four of the guards that
// needed it. On 2026-08-30, with three subagent worktrees present,
// TestExactlyOneImageSubstitutionPoint failed on a clean main with six findings,
// every one of them a path under .claude/worktrees/ — the estate has one image
// substitution point and the guard reported two, because it was reading three
// more copies of the same manifest.
//
// baseimage_mirror had the same exposure and had not fired only because nothing
// in a worktree happened to violate it yet.
//
// # Why this checks EVERY local skip set, not just the repo-root walkers
//
// Whether a guard can see .claude depends on the value of its walk root, which
// is a variable — often a parameter threaded from a caller (dockerfileFroms is
// handed repoRoot; protoRPCs is handed protoRoot). An AST cannot tell those
// apart without type-and-value analysis this package deliberately does not do,
// and a hand-written list of "the repo-root ones" would be the same fallible
// artifact one layer up: it was wrong twice while this was being written, first
// at 71 files and then at 7, before measurement put it at 2.
//
// So the rule is uniform and needs no such list: ANY guard that maintains its
// own skip set names .claude in it. A guard whose walk root is a subdirectory
// today costs nothing by carrying the entry, and is covered on the day somebody
// re-points it at the repo root — which is exactly the change nobody would think
// to re-audit.
//
// A skip set is recognised by naming ".git", which every one of them does; that
// is what makes the set derivable rather than listed.
func TestEveryLocalSkipSetInThisPackageNamesTheAgentWorktrees(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("*_test.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) < 50 {
		t.Fatalf("found %d test files in this package — the glob is not seeing the guards, so "+
			"this check is asserting nothing", len(paths))
	}

	checked := 0
	for _, p := range paths {
		if filepath.Base(p) == "walk_skip_test.go" {
			continue // the shared set itself, pinned by the test above
		}
		fset := token.NewFileSet()
		// Mode 0: comments are not attached, so the paragraphs above — which name
		// both ".git" and ".claude" — cannot satisfy or defeat this.
		file, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", p, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			var strs []string
			switch {
			case ok:
				strs = literalStrings(lit)
			default:
				cc, isCase := n.(*ast.CaseClause)
				if !isCase {
					return true
				}
				for _, e := range cc.List {
					if bl, isLit := e.(*ast.BasicLit); isLit && bl.Kind == token.STRING {
						strs = append(strs, strings.Trim(bl.Value, `"`))
					}
				}
			}
			if !containsString(strs, ".git") {
				return true
			}
			checked++
			if !containsString(strs, ".claude") {
				t.Errorf("%s:%d keeps its own directory-skip set naming \".git\" and not "+
					"\".claude\".\n"+
					"If this walk is ever rooted at the repository root it will descend into "+
					"an agent worktree and read another checkout's files as the estate's — a "+
					"red suite with no defect in it, invisible in CI because CI has no "+
					"worktrees. Call skipWalkDir, or add \".claude\" to the set",
					filepath.Base(p), fset.Position(n.Pos()).Line)
			}
			return true
		})
	}

	if checked == 0 {
		t.Fatal("no local skip set was found in this package — either they all now call " +
			"skipWalkDir (in which case delete this test) or the recogniser has stopped " +
			"matching, and it is passing without asserting anything")
	}
}

// literalStrings returns the string-literal elements of a composite literal,
// covering both []string{…} and map[string]bool{…: true} skip sets.
func literalStrings(lit *ast.CompositeLit) []string {
	var out []string
	for _, el := range lit.Elts {
		switch e := el.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				out = append(out, strings.Trim(e.Value, `"`))
			}
		case *ast.KeyValueExpr:
			if bl, ok := e.Key.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				out = append(out, strings.Trim(bl.Value, `"`))
			}
		}
	}
	return out
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

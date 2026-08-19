package arch

import (
	"io/fs"
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

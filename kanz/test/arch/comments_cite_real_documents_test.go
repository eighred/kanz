package arch

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A COMMENT MAY NOT CITE A DOCUMENT THAT IS NOT IN THIS REPOSITORY (#513).
//
// # What went wrong without it
//
// KANZ_BRAIN.md was deleted on 2026-07-29, for naming a Go version and a module
// path the repo had moved away from while the code was right. Fourteen citations
// to it survived the deletion, and one of them was actively harmful: pkg/alpha's
// header told a reader that quantitative strategy math "does NOT live in this
// repository (house rule; see KANZ_BRAIN.md)" — a rule the owner had retired
// twelve days earlier. So a reader was told not to write the thing that had been
// asked for, on the authority of a file they could not open, and the citation is
// what made it look settled rather than stale.
//
// AGENTS.md's standard is that "a comment justifying a trade-off is dated
// evidence — verify its premise before relying on it". A citation to a missing
// file cannot be verified at all. The reader's three options are to believe it,
// ignore it, or go read the code and find out — and only the last is safe, which
// makes the citation worse than no citation.
//
// # What this checks, and what it cannot
//
// It checks that a referenced .md file EXISTS. It cannot check that the document
// still says what the comment claims — nothing can, cheaply — so this is the
// floor rather than the ceiling. The floor is worth having because the expensive
// failure above was entirely on this side of it.
//
// # Quoting a deleted document is still allowed, with the enforcement beside it
//
// test/arch/ssh_plane_test.go quotes KANZ_BRAIN.md as the dated provenance of a
// ruling, and the guard around the quote is what enforces it. That is the correct
// pattern and it is exempted below by name: the citation is history, not
// authority, and the reader can verify the rule by breaking the test.
//
// The difference is whether removing the cited document would change what a
// reader should do. For a quoted ruling with a live guard: no. For a bare
// "see <the deleted file>" as the reason a rule exists: yes, and that is the case
// this refuses.
//
// # Resolution is by BASENAME as well as by path, deliberately
//
// Comments cite documents the way people talk about them — "README.md" or a
// repo-relative path, and often a section within one. Requiring an exact path
// would make this a link checker, which is a different and much noisier job: it
// would fail on a correct citation to a document that MOVED, and the reader is
// not harmed by that. What harms the reader is the document not existing AT
// ALL, so that is what this checks.
//
// The consequence is worth stating, because it got larger on 2026-08-26. The
// docs/ trees were retired that day and their content folded into README files
// and AGENTS.md, so most citations now name one of those — and this guard
// cannot tell a live "kanz-schemas/README.md § Subject Taxonomy §4" from a
// citation to a section that has since been renamed or deleted. The basename
// resolves either way. That is the same floor-not-ceiling limit stated above,
// but it now applies to nearly every citation in the estate rather than to a
// handful, and nothing else checks the section half.

// mdRef matches a markdown filename in prose. Deliberately narrow: a path
// fragment ending in .md, no spaces.
var mdRef = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./\-]*\.md`)

// guardProvenance is the shared argument for a citation inside test/arch: the
// file doing the citing is the file doing the ENFORCING, so the reader verifies
// the rule by breaking the test rather than by opening the document. The quote
// survives to carry the ruling's date and its exact wording, which is what stops
// someone re-litigating a settled decision.
//
// IT IS NOT A BLANKET FOR test/arch, deliberately. Each site is named, so the
// dead-entry arm still deletes it when the comment goes — and a guard that cited
// a missing document for a rule it does NOT check would still fail, which is the
// case worth catching here.
const guardProvenance = "PROVENANCE FOR A RULE THIS FILE ENFORCES. The guard is the " +
	"verification; the quote is the dated record of the ruling behind it, kept so the " +
	"decision is not re-argued from scratch. The cited document was deleted on 2026-07-29 " +
	"(#513)."

// citationExempt maps "<file>:<cited>" to why the citation may name something
// absent from the tree.
var citationExempt = map[string]string{
	"test/arch/ssh_plane_test.go:KANZ_BRAIN.md": "PROVENANCE, NOT AUTHORITY. The quote is the " +
		"dated record of the ruling that cancelled the SSH plane, and the guard it sits on is " +
		"what enforces that ruling — a reader verifies it by breaking the test, not by opening " +
		"the document. Deleting the quote would lose the date and the wording of a decision " +
		"someone will otherwise re-litigate.",
	"test/arch/comments_cite_real_documents_test.go:KANZ_BRAIN.md": "this guard's own doc, " +
		"which has to name the document whose deletion motivated it.",
	"test/arch/archiver_topology_test.go:KANZ_BRAIN.md": guardProvenance,
	"test/arch/nats_identity_test.go:KANZ_BRAIN.md":     guardProvenance,
	"test/arch/supplychain_test.go:KANZ_BRAIN.md":       guardProvenance,
	"test/arch/tenant_compute_test.go:KANZ_BRAIN.md":    guardProvenance,
	"pkg/alpha/alpha.go:KANZ_BRAIN.md": "the correction record: the header quotes its own " +
		"retired text so a reader understands why the alpha package once said the strategy math " +
		"lived elsewhere (#416, #512).",
}

func TestCommentsCiteDocumentsThatExist(t *testing.T) {
	root := moduleRoot(t)
	parent := filepath.Dir(root) // the repo root; kanz/ and kanz-schemas/ are siblings
	docs := markdownIndex(t, parent)

	type violation struct{ where, cited string }
	var bad []violation
	seenExempt := map[string]bool{}
	citations := 0

	// COMMENTS ONLY, and that is a real limit rather than an oversight. This
	// guard's subject is prose telling a reader to go read a document; the
	// string-literal half of the estate is scanned by its sibling,
	// comments_cite_by_symbol_test.go (#610), which is where the exemption
	// tables' evidence lives. walkGoProse is the shared extractor for both, so
	// there is one answer to "what prose does a Go file contain".
	scanned := walkGoProse(t, root, func(s proseSite) {
		// Per COMMENT LINE, not per rejoined group: the URL skip below is a
		// per-line judgement, and reading a whole group at once would let one
		// http link in it excuse every citation beside it.
		if s.Kind != proseComment || s.Joined {
			return
		}
		text := s.Text
		for _, m := range mdRef.FindAllString(text, -1) {
			// A URL is a citation to somewhere else entirely, and this
			// guard has no opinion about the internet.
			if strings.Contains(text, "http") && strings.Contains(text, m) {
				continue
			}
			citations++
			if docs[m] || docs[strings.TrimPrefix(m, "./")] || docs[filepath.Base(m)] {
				continue
			}
			key := s.File + ":" + m
			if _, ok := citationExempt[key]; ok {
				seenExempt[key] = true
				continue
			}
			bad = append(bad, violation{where: s.File, cited: m})
		}
	})

	// NON-VACUITY, both halves: the walk must have read the tree, and it must
	// have found citations to check. Zero of either means this passes having
	// examined nothing.
	if scanned < 500 {
		t.Fatalf("scanned only %d Go files — the walk is broken, not the estate", scanned)
	}
	if citations < 20 {
		t.Fatalf("found only %d document citations — the comment extraction or the pattern is "+
			"broken", citations)
	}

	if len(bad) > 0 {
		byDoc := map[string][]string{}
		for _, v := range bad {
			byDoc[v.cited] = append(byDoc[v.cited], v.where)
		}
		docs := make([]string, 0, len(byDoc))
		for d := range byDoc {
			docs = append(docs, d)
		}
		sort.Strings(docs)
		var b strings.Builder
		for _, d := range docs {
			sort.Strings(byDoc[d])
			b.WriteString("\n  " + d + "\n    " + strings.Join(byDoc[d], "\n    "))
		}
		t.Errorf("%d comment(s) cite a document that is not in this repository:%s\n\n"+
			"A citation to a missing file cannot be verified, so a reader can only believe it, "+
			"ignore it, or go read the code — and only the last is safe. KANZ_BRAIN.md's "+
			"deletion left pkg/alpha telling readers not to write the thing the owner had just "+
			"asked for, and the citation is what made that look settled (#513).\n"+
			"Fix by stating the rule beside what enforces it, or by re-verifying the claim "+
			"against the code and saying what is true now. DO NOT repoint the citation at "+
			"AGENTS.md — that preserves an unverified claim and re-authorises it against a "+
			"document that does not make it. If the citation is dated PROVENANCE for a rule the "+
			"code enforces, add it to citationExempt with that argument.",
			len(bad), b.String())
	}

	// DEAD-ENTRY ARM: an exemption whose citation is gone, or whose document
	// came back, is a claim about the tree that is no longer true.
	for key, reason := range citationExempt {
		if !seenExempt[key] {
			t.Errorf("citation exemption %q is stale — the comment was removed, or the document "+
				"now exists. Delete the entry (%s)", key, reason)
		}
	}
}

// markdownIndex walks the whole repository once and returns every .md file,
// keyed by BOTH its repo-relative path and its bare filename. See the resolution
// note in the package doc for why the bare name counts.
func markdownIndex(t *testing.T, repoRoot string) map[string]bool {
	t.Helper()
	index := map[string]bool{}
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of the tree is not this guard's business
		}
		if d.IsDir() {
			if skipWalkDir(d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, rerr := filepath.Rel(repoRoot, path)
		if rerr == nil {
			index[filepath.ToSlash(rel)] = true
		}
		index[d.Name()] = true
		return nil
	})
	if err != nil {
		t.Fatalf("indexing markdown: %v", err)
	}
	if len(index) < 20 {
		t.Fatalf("found only %d markdown files in %s — the index walk is broken, so every "+
			"citation would look dangling", len(index), repoRoot)
	}
	return index
}

func mustRelPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

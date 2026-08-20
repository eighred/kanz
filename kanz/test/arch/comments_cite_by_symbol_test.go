package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A COMMENT MAY NOT CITE A FILE IN THIS REPOSITORY BY LINE NUMBER (#610).
//
// # The bar this sets, stated before anything else
//
// This is the STRONG bar, reached by construction rather than by inspection.
// It does not check that a cited line says what the comment claims — nothing
// can, cheaply. It removes the construct that decays instead: an in-repo
// `<file>:<line>` citation is refused outright, and the citation has to name a
// SYMBOL, which moves with the code it names.
//
// The weaker bar the issue offered — "the file exists and the line is in range"
// — was measured against this tree before it was rejected. Of the 151 line
// citations here, ZERO pointed past the end of an in-repo file. It would have
// shipped green on a tree whose citations were extensively rotted, and it would
// have passed the corpact citation that prompted #610, because the line it named
// was a real line in a real file. A guard that reports "checked, and fine" after
// checking a property nothing violates is worse than no guard, so that bar is
// not the one here.
//
// A third bar was built and measured too: require some identifier the comment
// names to appear at the cited line. On this tree it split 46 pass / 72 fail —
// and among the 72 were correct citations whose comment simply worded things
// differently (pkg/bus/redrive_integration_test.go's pointer at
// dlq_integration_test.go is exact, and the heuristic missed it because the
// anchor was a hyphenated header name). A guard that fires on correct comments
// gets exempted until it means nothing, and one that matches prose is the
// failure this repo has already had three times. Rejected as well.
//
// # What went wrong without it
//
// test/arch/no_dark_capability_test.go exempted services/accounting/internal/
// corpact and cited ledger.go by LINE as the evidence for why. That was correct
// when #583 landed on 2026-08-19. ledger.go grew 52 lines in an unrelated merge
// the NEXT DAY, and the cited line moved 52 lines down. The old number then
// landed inside the Event struct's doc, which says nothing about corporate
// actions.
//
// The failure mode is not a broken link. An exemption's whole job is to carry
// its own justification. A reader checking why a dark package is allowed to stay
// dark followed the citation, found nothing about corpact, and would reasonably
// conclude the exemption was stale and the package had been wired — the exact
// opposite of the truth. The evidence had a one-day half-life, and nothing went
// red.
//
// PR #609 repaired it by citing ledger.Action by symbol. This is that repair
// made general.
//
// # Why STRING LITERALS are scanned, not just comments
//
// The corpact citation was not in a comment. It was in a string literal — the
// reason field of an arch guard's exemption table. A comments-only guard, which
// is what #513 is, would not have caught the one defect that prompted this
// issue. Every exemption table in test/arch/ carries its evidence in a string,
// so a guard for this class that reads only comments checks the half of the
// estate where the failure did not happen.
//
// The string arm is load-bearing rather than incidental, so the non-vacuity
// check below asserts it found string-literal sites specifically. Dropping it in
// a refactor cannot pass quietly.
//
// # Relationship to #513, which is this guard's sibling
//
// comments_cite_real_documents_test.go checks that a cited .md file EXISTS. It
// is explicitly the floor for documents, because a document's content cannot be
// machine-checked against a prose claim. The two share their extraction —
// walkGoProse below is the single answer to "what prose does a Go file contain",
// and #513 calls it for comments — and they divide the subject: #513 owns "does
// the cited thing exist", this owns "is the citation of a kind that can rot".
//
// # What this does NOT catch, stated so the green is not read as more
//
//   - A SYMBOL citation to a symbol that no longer exists, or that never said
//     what the comment claims. Renaming ledger.Action leaves every comment
//     naming it dangling and this stays green. That is the #513 floor problem in
//     a different costume; it is not resolved here.
//   - A citation to a file OUTSIDE this repository (x/crypto/ssh/session.go, a
//     dependency's source). Out of scope deliberately: the line belongs to a
//     version-pinned artifact this tree does not control, and flagging it would
//     put noise on the one citation a reader can still resolve exactly.
//   - A wrong FILE name with no line number. `see producer.go` naming a file
//     that never held the thing passes. Only the rotting construct is refused.
//   - Prose in any language but Go. YAML and shell comments in this repo cite
//     line numbers too; this walks .go files only.
//
// # Its own text cannot satisfy it
//
// A guard that greps raw source matches its own comments — three guards in this
// directory have passed with the checked thing deleted for exactly that reason,
// and this one is unusually exposed because its subject matter is citations.
//
// Two things keep that from happening here. Every example in this file writes a
// line number as a `:NNN` placeholder, which the pattern cannot match, so the
// prose carries no citations to trip over. And the positive control below never
// reads a file at all: it drives the pattern and the resolver directly, with a
// fixture ASSEMBLED AT RUNTIME (badCitationFixture) precisely so the thing being
// detected is not sitting in this file waiting to be found by accident.
//
// The exemption table is the one exception, and it is exempted by name — a table
// cannot excuse a citation without quoting it. See the note on
// lineCitationExempt for what was tried before that, and why it was abandoned.

// lineCitationPattern matches a path ending in a source extension this repo
// uses, followed by a line number.
//
// The extension list is closed on purpose. An open pattern ("anything dotted
// followed by a colon and digits") matches version strings, durations, ratios
// and struct tags, and the exemption list that follows would be noise about the
// pattern rather than about citations.
var lineCitationPattern = regexp.MustCompile(
	`([A-Za-z0-9_][A-Za-z0-9_./\-]*\.(?:go|proto|yaml|yml|sql|sh|json|py|ts|tsx|toml|tf|md)):(\d+)`)

// lineCitationExempt maps "<citing file> => <citation>" to why that citation may
// keep a line number.
//
// Default-deny, one entry per site, with a dead-entry arm below: an exemption
// whose citation has been repaired is a claim about the tree that is no longer
// true, and it must not outlive the thing it excused.
//
// A key has to spell out the citation it excuses, so this table is itself the
// one place in this file that carries a citation — hence the second entry,
// exactly as #513's table exempts its own doc for naming the deleted document
// that motivated it.
//
// Writing the keys as split literals to dodge that was tried first, and the
// wrapped-run reader in walkGoProse rejoined them and reported this file. That
// is the feature working, so the dodge is gone and the exemption is named
// instead: a guard that has to be clever to stay green is a guard whose next
// editor will not know why.
var lineCitationExempt = map[string]string{
	"test/arch/no_dark_capability_test.go => ledger.go:109": "THE ROTTED CITATION ITSELF, kept " +
		"as the dated record of what went wrong. The corpact entry quotes its own retired text " +
		"to tell the next editor why the evidence is a symbol and not a line — deleting the " +
		"quote loses the one concrete case that makes the rule obviously worth following. It is " +
		"history, not a pointer: the entry says in the same breath that the line moved (#609, " +
		"#610).",

	"test/arch/comments_cite_by_symbol_test.go => ledger.go:109": "this guard's own exemption " +
		"table, which cannot excuse a citation without quoting it. The entry above is the only " +
		"citation in this file; everything else here writes a line number as a :NNN placeholder, " +
		"which the pattern cannot match.",
}

// TestCommentsDoNotCiteRepoFilesByLineNumber is the guard. Its name is the
// property: no line-number citation to a file in this repository. It is NOT
// "citations are correct" — see the bar note in this file's doc.
func TestCommentsDoNotCiteRepoFilesByLineNumber(t *testing.T) {
	root := moduleRoot(t)
	index := repoFileIndex(t, filepath.Dir(root))

	type violation struct {
		where    string
		citation string
	}
	// Keyed by "<file> => <citation>", the same key the exemption map uses, so a
	// citation that a wrapped run reports a second time is one finding rather than
	// two. The repair is per citation TEXT, not per occurrence.
	bad := map[string]violation{}
	seenExempt := map[string]bool{}
	var comments, strLits, joinedComments, joinedStrings int

	files := walkGoProse(t, root, func(s proseSite) {
		switch {
		case s.Kind == proseComment && s.Joined:
			joinedComments++
		case s.Kind == proseComment:
			comments++
		case s.Kind == proseString && s.Joined:
			joinedStrings++
		case s.Kind == proseString:
			strLits++
		}
		for _, raw := range lineCitations(s.Text) {
			cited := raw[:strings.LastIndex(raw, ":")]
			if !index.holds(cited) {
				continue // a dependency's source; out of scope, see the doc
			}
			key := s.File + " => " + raw
			if _, ok := lineCitationExempt[key]; ok {
				seenExempt[key] = true
				continue
			}
			if _, dup := bad[key]; dup {
				continue
			}
			where := fmt.Sprintf("%s:%d (%s)", s.File, s.Line, s.Kind)
			if s.Joined {
				where += " — wrapped across lines"
			}
			bad[key] = violation{where: where, citation: raw}
		}
	})

	// NON-VACUITY. A ban cannot prove itself by counting the instances it finds —
	// after the #610 sweep there are none, and "found nothing" and "looked
	// nowhere" read identically. So instead every STAGE of the pipeline is pinned
	// separately, each with a floor around a third of what it reads today: high
	// enough that an arm returning nothing is a failure, low enough that deleting
	// a service is not.
	//
	// The DECISION stage — pattern, resolver — is not a count and cannot be
	// covered here. TestTheLineCitationDetectorFiresOnAKnownBadCitation covers it.
	for _, arm := range []struct {
		name  string
		got   int
		floor int
		why   string
	}{
		{"Go files walked", files, 1000,
			"the walk is broken, not the estate"},
		{"files indexed", index.size(), 1500,
			"every citation would resolve to \"not in this repo\" and be skipped"},
		{"comments read", comments, 40000,
			"the comment arm of emitGoProse is broken"},
		{"string literals read", strLits, 30000,
			"the STRING arm is broken — and that is the arm the corpact citation lived in, " +
				"so a comments-only reading of this estate would have missed #610's own defect"},
		{"wrapped comment groups rejoined", joinedComments, 4000,
			"unwrapComment is broken, so a citation split across two comment lines is invisible"},
		{"concatenation chains rejoined", joinedStrings, 1000,
			"unwrapConcat is broken, so a citation split across an exemption table's line " +
				"breaks is invisible — the shape the corpact entry itself is written in"},
	} {
		if arm.got < arm.floor {
			t.Fatalf("%s: %d, want at least %d — %s", arm.name, arm.got, arm.floor, arm.why)
		}
	}

	if len(bad) > 0 {
		keys := make([]string, 0, len(bad))
		for k := range bad {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString("\n  " + bad[k].where + "\n    cites " + bad[k].citation)
		}
		t.Errorf("%d citation(s) point at a file in this repository BY LINE NUMBER:%s\n\n"+
			"A line number is evidence with a half-life. The corpact exemption in "+
			"no_dark_capability_test.go cited a line that moved 52 rows in an unrelated merge "+
			"the DAY AFTER it was written, and the old number then landed in an unrelated "+
			"struct's doc — so a reader checking why a dark package stays dark found no "+
			"evidence and would conclude the package had been wired (#609, #610).\n"+
			"Fix by citing the SYMBOL: `ledger.Action`, `reconcileOrders`, "+
			"`outcome_announced_at` — a name moves with the code it names. Where the comment "+
			"already names the symbol, the line number is redundant and deleting it is the "+
			"whole repair. DO NOT re-point the number at the current line: that restores the "+
			"same one-day half-life. If the citation is dated PROVENANCE for a rot that "+
			"already happened, add it to lineCitationExempt with that argument.",
			len(bad), b.String())
	}

	// DEAD-ENTRY ARM: an exemption whose citation has been repaired or deleted no
	// longer describes the tree.
	//
	// It doubles as a PERMANENT LIVE-FIRE CONTROL, which is worth keeping in mind
	// before anyone "tidies" the exemption away. The entry names a citation that
	// really is in this estate, so the guard cannot go green without the pattern,
	// the walk, the string arm and the resolver ALL having worked on a real file.
	// Breaking any one of them was measured to fire here: the exemption reads as
	// stale, because from inside the guard a citation it cannot see and a citation
	// that is not there look the same.
	for key, reason := range lineCitationExempt {
		if !seenExempt[key] {
			t.Errorf("line-citation exemption %q is stale — the citation was repaired or "+
				"removed. Delete the entry (%s)", key, reason)
		}
	}
}

// badCitationFixture returns a citation that this guard must flag, assembled
// from pieces so that no string literal in this file matches
// lineCitationPattern. The guard scans its own source; a fixture written out in
// full would make this file violate the rule it enforces, and a reader would
// have to decide whether the resulting failure was real.
func badCitationFixture() string {
	return "the fold is at " + "ledger" + ".go" + ":" + "109" + " today"
}

// TestTheLineCitationDetectorFiresOnAKnownBadCitation is the positive control,
// and it is the arm that matters most.
//
// A ban guard goes green by finding nothing, and after the #610 sweep it finds
// nothing on a clean tree — which is indistinguishable from a broken pattern, a
// broken resolver, or a walk that read no files. The count-based arms above
// cover the walk and the extractor. This covers the DECISION: the exact
// pattern-and-resolve path the guard uses is run against input known to be bad,
// and against input known to be out of scope, and both answers are asserted.
func TestTheLineCitationDetectorFiresOnAKnownBadCitation(t *testing.T) {
	index := repoFileIndex(t, filepath.Dir(moduleRoot(t)))

	// 1. THE PATTERN. The fixture reproduces the citation #610 was filed for.
	got := lineCitations(badCitationFixture())
	if len(got) != 1 {
		t.Fatalf("lineCitations(%q) = %v, want exactly one citation — the pattern no longer "+
			"matches the citation this guard exists for", badCitationFixture(), got)
	}
	cited := got[0][:strings.LastIndex(got[0], ":")]

	// 2. THE RESOLVER, positive. The cited file is in this repository, so the
	//    citation is in scope and the guard must refuse it.
	if !index.holds(cited) {
		t.Fatalf("repoFileIndex does not hold %q, which is a file in this repository — every "+
			"citation would be classed as third-party and skipped, and this guard would pass "+
			"having checked nothing", cited)
	}

	// 3. THE RESOLVER, negative. A dependency's source must NOT be in scope, or
	//    the guard becomes noise on the one citation a reader can resolve
	//    exactly. The path is assembled for the same reason as the fixture.
	external := "x/crypto/ssh/" + "session" + ".go"
	if index.holds(external) {
		t.Errorf("repoFileIndex claims %q is in this repository — a dependency's source would "+
			"be reported as a violation. The basename matches a file here, so resolution must "+
			"require the DIRECTORY prefix to match too", external)
	}

	// 4. A citation with no line number is not this guard's business — that is
	//    the #513 sibling's subject, and overlapping them would give a reader
	//    two answers to one question.
	if n := lineCitations("see " + "ledger" + ".go, and ledger.Action"); n != nil {
		t.Errorf("lineCitations flagged a symbol citation %v — this guard refuses LINE numbers "+
			"only, and firing on a correct symbol citation is how a guard gets exempted until "+
			"it means nothing", n)
	}

	// 5. THE WRAPPED READER, end to end through the real extraction. Both halves
	//    of it: a citation broken across two comment lines, and one broken across
	//    a concatenation chain's line breaks — which is the shape every exemption
	//    table in this directory is written in, and therefore the shape the
	//    corpact citation had.
	//
	//    Without this, unwrapComment and unwrapConcat could be deleted and only
	//    the two count arms above would notice, which they would do by reporting
	//    a number rather than a missed citation.
	for _, tc := range wrappedCitationFixtures() {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "fixture.go", tc.src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: parsing the fixture: %v", tc.name, err)
		}
		found := false
		emitGoProse(fset, f, "fixture.go", func(site proseSite) {
			if len(lineCitations(site.Text)) > 0 {
				found = true
			}
		})
		if !found {
			t.Errorf("%s: the extraction found no citation in a fixture that contains one, split "+
				"the way this estate's line limit splits them. A citation that survives a line "+
				"break is a citation this guard cannot see, and the densest evidence comments — "+
				"the ones most worth checking — are exactly the ones that wrap.", tc.name)
		}
	}
}

// wrappedCitationFixtures returns Go sources that each contain one citation
// broken across a line, in the two ways this estate breaks them.
//
// The sources are assembled from pieces for the same reason as
// badCitationFixture: written out whole, a fixture would put a citation in this
// file, and the guard would then be reporting itself.
func wrappedCitationFixtures() []struct {
	name string
	src  string
} {
	num := "109"
	return []struct {
		name string
		src  string
	}{
		{
			name: "citation split across two comment lines",
			src: "package p\n\n// the fold is named in ledger.\n" +
				"// go" + ":" + num + " today\nvar X = 1\n",
		},
		{
			name: "citation split across a concatenation chain",
			src: "package p\n\nvar Y = \"the fold is named in ledger.go\" +\n" +
				"\t\"" + ":" + num + " today\"\n",
		},
	}
}

// lineCitations returns every "<file>:<line>" citation in text, as raw matches.
func lineCitations(text string) []string {
	return lineCitationPattern.FindAllString(text, -1)
}

// proseKind distinguishes the two places a Go file carries prose.
type proseKind string

const (
	proseComment proseKind = "comment"
	proseString  proseKind = "string"
)

// proseSite is one piece of prose in a Go file: a comment, or a string literal.
//
// Joined marks a site that is the whole of a wrapped run — every line of one
// comment group, or every operand of one concatenation chain — rejoined with the
// wrapping removed. See walkGoProse for why that second reading exists.
type proseSite struct {
	File   string // module-relative, slash-separated
	Line   int
	Kind   proseKind
	Joined bool
	Text   string
}

// walkGoProse is the single answer to "what prose does a Go file contain", used
// by this guard and by #513's. It parses with go/ast rather than grepping raw
// source, because a raw grep matches the guard's own doc comment and the
// identifiers next to it — the failure that has let three guards in this
// directory pass with the checked thing deleted.
//
// It returns the number of files parsed so callers can assert the walk was not
// vacuous.
func walkGoProse(t *testing.T, root string, fn func(proseSite)) int {
	t.Helper()
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// skipWalkDir is the shared skip set (walk_skip_test.go): it keeps a
			// live agent worktree's FULL COPY of this repository out of the walk,
			// while keeping .github in, which two other guards parse.
			if skipWalkDir(d) || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		files++
		emitGoProse(fset, f, filepath.ToSlash(mustRelPath(root, path)), fn)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

// emitGoProse yields every prose site in one parsed file. It is separate from
// the walk so the positive control can drive the whole extraction over a source
// snippet it builds itself, without a filesystem and without depending on this
// estate happening to contain an example of what is being detected.
func emitGoProse(fset *token.FileSet, f *ast.File, rel string, fn func(proseSite)) {
	for _, group := range f.Comments {
		for _, c := range group.List {
			fn(proseSite{File: rel, Line: fset.Position(c.Pos()).Line, Kind: proseComment, Text: c.Text})
		}
		if len(group.List) > 1 {
			fn(proseSite{
				File: rel, Line: fset.Position(group.Pos()).Line, Kind: proseComment,
				Joined: true, Text: unwrapComment(group),
			})
		}
	}
	consumed := map[ast.Node]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if be, ok := n.(*ast.BinaryExpr); ok && be.Op == token.ADD && !consumed[be] {
			if text, pure := unwrapConcat(be, consumed); pure {
				fn(proseSite{
					File: rel, Line: fset.Position(be.Pos()).Line, Kind: proseString,
					Joined: true, Text: text,
				})
			}
		}
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		fn(proseSite{File: rel, Line: fset.Position(bl.Pos()).Line, Kind: proseString, Text: bl.Value})
		return true
	})
}

// unwrapComment rejoins a comment group with the `//` markers and the leading
// indentation removed, and NO separator inserted.
//
// A citation that wraps across two lines is invisible to a per-line read, and
// this estate wraps constantly: golangci-lint's line limit is what splits a long
// path, so the densest evidence comments — the ones most worth checking — are
// exactly the ones a per-line pattern cannot see. Two live examples were found
// the moment this was added, both in stored_series_has_a_producer_test.go, where
// a path had been broken after the dot and after the colon.
//
// Joining with no separator is deliberate: a wrapped path resumes with no space,
// so anything else would put a gap in the middle of the citation this exists to
// reassemble.
func unwrapComment(g *ast.CommentGroup) string {
	var b strings.Builder
	for _, c := range g.List {
		t := strings.TrimPrefix(c.Text, "//")
		t = strings.TrimPrefix(t, "/*")
		t = strings.TrimSuffix(t, "*/")
		b.WriteString(strings.TrimSpace(t))
	}
	return b.String()
}

// unwrapConcat rejoins a string-concatenation chain into the string it builds,
// for the same reason as unwrapComment: an exemption table's reason field is one
// long sentence broken at the line limit, and a path can be broken with it. It
// marks every node it consumed so the outer walk does not report one chain once
// per nesting level.
//
// It reports false when the expression is not a pure string concatenation —
// anything with a variable in it is not prose this guard can read.
func unwrapConcat(be *ast.BinaryExpr, consumed map[ast.Node]bool) (string, bool) {
	var b strings.Builder
	pure := true
	var walk func(e ast.Expr)
	walk = func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.BinaryExpr:
			if v.Op != token.ADD {
				pure = false
				return
			}
			consumed[v] = true
			walk(v.X)
			walk(v.Y)
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				pure = false
				return
			}
			b.WriteString(strings.Trim(v.Value, "`\""))
		default:
			pure = false
		}
	}
	walk(be)
	return b.String(), pure
}

// fileIndex answers "is this cited path a file in this repository?".
type fileIndex struct {
	byRel  map[string]bool
	byBase map[string][]string
	all    int
}

func (ix fileIndex) size() int { return ix.all }

// holds reports whether cited names a file in this repository.
//
// Resolution mirrors how people actually write citations — "ledger.go", not
// "kanz/services/accounting/internal/ledger/ledger.go" — but a bare basename is
// only accepted when the citation HAS no directory part. A citation that names
// directories must have those directories match, or "x/crypto/ssh/session.go"
// resolves to this repo's web-bff session.go and a dependency's source is
// reported as ours.
func (ix fileIndex) holds(cited string) bool {
	cited = strings.TrimPrefix(filepath.ToSlash(cited), "./")
	if ix.byRel[cited] {
		return true
	}
	if strings.Contains(cited, "/") {
		for _, p := range ix.byBase[path.Base(cited)] {
			if strings.HasSuffix(p, "/"+cited) {
				return true
			}
		}
		return false
	}
	return len(ix.byBase[cited]) > 0
}

// repoFileIndex indexes every file in the repository — both modules, since
// comments in kanz/ cite kanz-schemas/ constantly — keyed by repo-relative
// path, by module-relative path, and by basename.
func repoFileIndex(t *testing.T, repoRoot string) fileIndex {
	t.Helper()
	ix := fileIndex{byRel: map[string]bool{}, byBase: map[string][]string{}}
	module := filepath.Join(repoRoot, "kanz")
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of the tree is not this guard's business
		}
		if d.IsDir() {
			if skipWalkDir(d) {
				return filepath.SkipDir
			}
			return nil
		}
		ix.all++
		rel, rerr := filepath.Rel(repoRoot, p)
		if rerr == nil {
			ix.byRel[filepath.ToSlash(rel)] = true
		}
		if mrel, merr := filepath.Rel(module, p); merr == nil && !strings.HasPrefix(mrel, "..") {
			ix.byRel[filepath.ToSlash(mrel)] = true
		}
		slashed := filepath.ToSlash(p)
		ix.byBase[d.Name()] = append(ix.byBase[d.Name()], slashed)
		return nil
	})
	if err != nil {
		t.Fatalf("indexing %s: %v", repoRoot, err)
	}
	return ix
}

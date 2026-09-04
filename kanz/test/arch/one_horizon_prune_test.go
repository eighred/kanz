package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THERE IS ONE HORIZON PRUNE IN THIS MODULE, AND IT IS internal/pit (#871).
//
// # What went wrong, twice
//
// "An ascending list, pruned to a horizon measured from the NEWEST RETAINED
// element, releasing what it drops" is one concept. #811 established it and
// #862/#863 found the defect inside it: the horizon bounded the version COUNT
// and not the heap, because `vs[drop:]` reslices the same backing array and Go
// keeps an entire array alive while any slice references any part of it. The fix
// — `clear(vs[:drop])` — landed in one place.
//
// It then had to land in a second. #867's internal/marketedge/volprofile kept
// the same container, could not import pit because pit lived under
// internal/risk/pricing and test/arch/risk_boundary_test.go admits only
// internal/risk/api/v* from outside the risk module, and so was written with its
// own prune carrying its own copy of the same clear(). Nothing was wrong with
// either copy; the cost is that the NEXT defect of that class would have had to
// be found twice, which is the failure mode CLAUDE.md prices at 17 services with
// their own secret(), 15 of them wrong.
//
// #871 promoted pit to internal/pit, where anything may import it, and rewrote
// volprofile onto it. This guard is what keeps the third copy from appearing: a
// package that cannot see pit is a boundary problem to solve, never a reason to
// write the prune again.
//
// # THE RULE
//
// A function that BOTH computes a cutoff by subtracting a duration from a
// non-wall-clock instant AND drops a prefix of a slice is a horizon prune. Only
// internal/pit may contain one.
//
// # WHAT IS DELIBERATELY NOT THIS RULE'S BUSINESS
//
//   - A WALL-CLOCK expiry. `now().Add(-ttl)` is a different concept with a
//     different failure mode, and pit's package doc argues at length that the
//     wall clock is the WRONG anchor for retention: it empties a store whose
//     feed has stalled, turning "stale" into "missing". internal/risk/state's
//     dedupWindow.gc is the estate's example — a broker-redelivery window, where
//     wall clock is exactly right — and a guard that swept it in here would be
//     recommending a repair that breaks it. Fixture arm 2 pins that.
//   - A READ that clamps to a window without dropping anything.
//     trades.Tape.Volumes computes the same kind of cutoff and mutates nothing;
//     retention and aggregation are not the same act. Fixture arm 3 pins that.
//   - HOW LONG the horizon is. That is a financial decision argued per store.
//
// # SCOPE AND LIMITS, so nobody reads more into a green run than it carries
//
//   - Syntactic, and it fails OPEN. A prune split across two functions, or one
//     whose anchor is passed in from a caller under a name this analyser cannot
//     see, is not detected. The arms below are what stop that direction from
//     going unnoticed: the positive control is the REAL internal/pit source, not
//     a fixture, so a detector that has stopped recognising the shape fails here
//     rather than reporting a clean estate.
//   - It proves there is ONE implementation, not that it is CORRECT.
//     internal/pit's own release tests are what prove the prune releases.

// horizonPruneOwner is the one package that may implement the concept. It is a
// path prefix, so pit's own files are the allowed set and nothing else is.
const horizonPruneOwner = "internal/pit/"

// horizonPrune is one detected implementation of the concept.
type horizonPrune struct {
	file   string // path relative to the module root
	site   string // file:line of the function declaration, for the failure message
	fn     string // Type.Method or Func
	cutoff string // file:line of the cutoff expression
}

// key identifies an implementation for the exemption list. It names the FILE and
// the SYMBOL and never a line: a line number is evidence with a half-life — this
// repository has already had an exemption's citation move 52 rows in an
// unrelated merge the day after it was written — while a function name moves
// with the function. Renaming or deleting the function un-certifies the
// exemption through the dead-entry check below, which is the property the key
// has to carry.
func (p horizonPrune) key() string { return p.file + ":" + p.fn }

// isWallClockAnchor reports whether e reads the wall clock — `time.Now()`,
// `s.now()`, or a value plainly named `now`. Such a cutoff is an expiry, not a
// retention horizon, and is outside this rule (see the doc above).
func isWallClockAnchor(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch v := n.(type) {
		case *ast.Ident:
			if strings.EqualFold(v.Name, "now") {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if strings.EqualFold(v.Sel.Name, "now") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// horizonCutoff returns the position of a `X.Add(-d)` whose X is not wall-clock
// derived, or token.NoPos. That is the "measured back from an instant the data
// itself supplies" half of the concept.
func horizonCutoff(body *ast.BlockStmt) token.Pos {
	pos := token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		if pos != token.NoPos {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Add" {
			return true
		}
		un, ok := call.Args[0].(*ast.UnaryExpr)
		if !ok || un.Op != token.SUB {
			return true
		}
		if isWallClockAnchor(sel.X) {
			return true
		}
		pos = call.Pos()
		return false
	})
	return pos
}

// dropsAPrefix reports whether the body contains a tail reslice `x[lo:]` — the
// "and the front of the list goes away" half. It matches the reslice whatever is
// done with it (returned, assigned, or copied down through append), because all
// three are the same act on the same container.
func dropsAPrefix(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		se, ok := n.(*ast.SliceExpr)
		if !ok || se.Low == nil || se.High != nil || se.Max != nil {
			return true
		}
		// x[0:] drops nothing.
		if lit, ok := se.Low.(*ast.BasicLit); ok && lit.Value == "0" {
			return true
		}
		found = true
		return false
	})
	return found
}

// scanHorizonPrunes reports every function in f that implements the concept.
func scanHorizonPrunes(rel string, fset *token.FileSet, f *ast.File) []horizonPrune {
	var out []horizonPrune
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		cut := horizonCutoff(fd.Body)
		if cut == token.NoPos || !dropsAPrefix(fd.Body) {
			continue
		}
		name := fd.Name.Name
		if typeName, _ := receiverType(fd); typeName != "" {
			name = typeName + "." + name
		}
		out = append(out, horizonPrune{
			file:   rel,
			site:   fmt.Sprintf("%s:%d", rel, fset.Position(fd.Pos()).Line),
			fn:     name,
			cutoff: fmt.Sprintf("%s:%d", rel, fset.Position(cut).Line),
		})
	}
	return out
}

// scanHorizonPruneText runs the same analysis over a source literal, which is
// what makes the fixture arms possible without leaving a second prune in the
// estate for the guard to find.
func scanHorizonPruneText(t *testing.T, name, source string) []horizonPrune {
	t.Helper()
	fset := token.NewFileSet()
	// mode 0: comments are NOT attached, so nothing here can match this guard's
	// own prose (a guard that greps raw source matches its own comments, and this
	// repository has already shipped three that did).
	f, err := parser.ParseFile(fset, name, source, 0)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return scanHorizonPrunes(name, fset, f)
}

// goSourcesUnder parses every non-test .go file under the module root, skipping
// the directories skipWalkDir names — .claude above all, where this harness puts
// an agent's full checkout of this same repository.
func goSourcesUnder(t *testing.T, root string, fn func(rel string, fset *token.FileSet, f *ast.File)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipWalkDir(d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		fn(filepath.ToSlash(rel), fset, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// pre871VolprofilePrune is volprofile.Store.pruneLocked as it stood before #871:
// the second implementation, correct in every respect and duplicated all the
// same. It is the arm that proves this guard would have caught the copy on the
// day it was written.
const pre871VolprofilePrune = `package fixture

import (
	"sort"
	"time"
)

type session struct{ start time.Time }

type history struct {
	done  []*session
	dirty bool
}

type Store struct{ horizon time.Duration }

func (s *Store) pruneLocked(h *history) {
	if len(h.done) == 0 {
		return
	}
	cutoff := h.done[len(h.done)-1].start.Add(-s.horizon)
	drop := sort.Search(len(h.done), func(i int) bool { return !h.done[i].start.Before(cutoff) })
	if drop == 0 {
		return
	}
	clear(h.done[:drop])
	h.done = h.done[drop:]
	h.dirty = true
}`

// wallClockExpiry is internal/risk/state's dedupWindow.gc: a broker-redelivery
// window anchored on the wall clock, which is a DIFFERENT concept. It must not
// be flagged — pit's whole argument is that the wall clock is the wrong anchor
// for retention, so "route this through pit" would be a repair that breaks a
// window for which wall clock is right.
const wallClockExpiry = `package fixture

import "time"

type dedupWindow struct {
	seen  map[string]time.Time
	order []string
	ttl   time.Duration
	now   func() time.Time
}

func (w *dedupWindow) gc() {
	cutoff := w.now().Add(-w.ttl)
	i := 0
	for i < len(w.order) {
		ts, ok := w.seen[w.order[i]]
		if !ok || ts.Before(cutoff) {
			delete(w.seen, w.order[i])
			i++
			continue
		}
		break
	}
	w.order = w.order[i:]
}`

// windowedRead is trades.Tape.Volumes: the same kind of cutoff, computed from
// the newest retained element, over a container it never shortens. Aggregating
// within a window is not retaining, and a guard that conflated them would demand
// a horizon of every reader.
const windowedRead = `package fixture

import "time"

type trade struct{ eventTime time.Time }

type Tape struct {
	trades    []trade
	retention time.Duration
}

func (t *Tape) Volumes(window time.Duration) int {
	if len(t.trades) == 0 {
		return 0
	}
	if window <= 0 || window > t.retention {
		window = t.retention
	}
	cutoff := t.trades[len(t.trades)-1].eventTime.Add(-window)
	n := 0
	for i := len(t.trades) - 1; i >= 0; i-- {
		if t.trades[i].eventTime.Before(cutoff) {
			break
		}
		n++
	}
	return n
}`

// horizonPruneExempt is the default-deny allow-list: a second implementation
// that has been reviewed and argued for, keyed on the file and SYMBOL that
// implements it (see horizonPrune.key). The value names the issue that retires
// the entry — an exemption without one is a decision nobody has to revisit.
// EMPTY, AND THAT IS THE POINT (#880 retired the only entry).
//
// It held trades.Tape.pruneLocked, on the grounds that the tape could not be
// pit.Put: pit.Version is keyed uniquely by AsOf so a second Put at the same
// instant REPLACES — right for a calibration, silently lossy for a tape where two
// prints routinely share a timestamp — and pit.Put sorted-inserts, which would
// splice a late print into history a reader has already aggregated.
//
// BOTH OF THOSE ARE STILL TRUE. The tape was not folded into pit.Put and must not
// be. What changed is which part of the concept is shared: the RELEASE — clear
// the dropped prefix, reslice past it — moved into pit.DropOldest, and the tape
// calls it. The cutoff and the scan for the drop index stay in the tape, because
// they are where a trade tape genuinely differs from a calibration store: it
// scans linearly, tolerating the out-of-order print that arrival ordering admits.
//
// So the guard's rule is satisfied in the way it was meant to be — one
// implementation of "drop a prefix and release it" — rather than by an exemption.
// The tape also stopped compacting with append(t.trades[:0], t.trades[i:]...),
// which cost an O(n) copy per print on the ingest path and left the vacated tail
// slots reachable through the backing array.
//
// A new entry here needs the same standard: a reason that is a property of the
// data rather than of the code, and an issue that retires it.
var horizonPruneExempt = map[string]string{}

func TestOneHorizonPrune(t *testing.T) {
	// FIXTURE ARM 1 (positive control). The analyser must flag the copy #871
	// removed. Without it every arm below is vacuous.
	if pre := scanHorizonPruneText(t, "pre871.go", pre871VolprofilePrune); len(pre) == 0 {
		t.Fatal("the analyser did NOT flag volprofile.Store.pruneLocked as it stood before #871 — " +
			"a second implementation of the horizon prune, carrying its own copy of #862's " +
			"clear(). The scanner is broken, and every arm below passes on nothing.")
	}

	// FIXTURE ARM 2 (negative control). A wall-clock expiry is a different
	// concept and must not be swept in. pit's package doc argues the wall clock
	// is the wrong anchor for RETENTION; dedupWindow.gc is a redelivery window,
	// where it is the right one.
	if wc := scanHorizonPruneText(t, "dedup.go", wallClockExpiry); len(wc) != 0 {
		t.Fatalf("a wall-clock expiry was flagged as a horizon prune: %+v. internal/risk/state's "+
			"dedupWindow.gc has this shape, and 'route it through pit' would replace an anchor "+
			"that is correct for a broker-redelivery window with one that is not.", wc)
	}

	// FIXTURE ARM 3 (negative control). A read that clamps to a window mutates
	// nothing. trades.Tape.Volumes has this shape.
	if rd := scanHorizonPruneText(t, "volumes.go", windowedRead); len(rd) != 0 {
		t.Fatalf("a windowed READ was flagged as a horizon prune: %+v. Aggregating within a "+
			"window is not retaining, and this rule is about what a container KEEPS.", rd)
	}

	root := moduleRoot(t)
	var found []horizonPrune
	goSourcesUnder(t, root, func(rel string, fset *token.FileSet, f *ast.File) {
		found = append(found, scanHorizonPrunes(rel, fset, f)...)
	})
	sort.Slice(found, func(i, j int) bool { return found[i].site < found[j].site })

	// NON-VACUITY, AND IT IS DERIVED FROM THE SOURCE OF TRUTH RATHER THAN
	// ASSERTED HERE: the one implementation this rule exists to protect must be
	// among what the walk found. pit.Put IS the concept, so a detector that no
	// longer recognises it recognises nothing — and would report a clean estate
	// forever while a third copy sat beside it.
	var owner []horizonPrune
	for _, p := range found {
		if strings.HasPrefix(p.file, horizonPruneOwner) {
			owner = append(owner, p)
		}
	}
	if len(owner) == 0 {
		t.Fatalf("the walk did not find a horizon prune in %s at all, over %d file(s) of module "+
			"source. pit.Put is the implementation this rule is written around — if the analyser "+
			"cannot see it, it cannot see a copy of it either, and a green run here means "+
			"nothing. Found: %+v", horizonPruneOwner, len(found), found)
	}

	// DEAD-ENTRY CHECK: an exemption naming a function that is no longer a second
	// implementation reads as a reviewed decision while protecting nothing.
	live := map[string]bool{}
	for _, p := range found {
		live[p.key()] = true
	}
	var dead []string
	for site := range horizonPruneExempt {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("horizonPruneExempt names %d entr(y/ies) that are no longer a horizon prune: %s\n\n"+
			"Either the function was renamed or moved to another file (update the key) or it "+
			"was unified onto internal/pit (delete the entry). A stale exemption cannot outlive "+
			"the thing it excused.",
			len(dead), strings.Join(dead, ", "))
	}

	var offending []horizonPrune
	for _, p := range found {
		if strings.HasPrefix(p.file, horizonPruneOwner) {
			continue
		}
		if _, ok := horizonPruneExempt[p.key()]; ok {
			continue
		}
		offending = append(offending, p)
	}
	if len(offending) == 0 {
		return
	}
	var lines []string
	for _, p := range offending {
		lines = append(lines, fmt.Sprintf("%s: %s (cutoff at %s)", p.site, p.fn, p.cutoff))
	}
	t.Fatalf("%d function(s) outside internal/pit implement the horizon prune a second time:\n  %s\n\n"+
		"An ascending list pruned to a horizon measured from its own newest element, releasing "+
		"what it drops, is ONE concept with ONE implementation — internal/pit. #862 is what a "+
		"copy costs: the horizon bounded the retained COUNT and not the heap, because a reslice "+
		"keeps the whole backing array alive, and every retention assertion in the estate was on "+
		"len and blind to it. That fix had to be written twice already (#871). Import "+
		"github.com/eighred/kanz/internal/pit and use pit.Put. If a boundary stops you importing "+
		"it — the risk-module boundary is what caused the first copy — the boundary is the thing "+
		"to solve, in an issue, and not a reason to write the prune again.",
		len(offending), strings.Join(lines, "\n  "))
}

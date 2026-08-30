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

// THE RISK STATE STORE MUST NOT HAND OUT THE LIVE PORTFOLIO (#821).
//
// # What went wrong
//
// internal/risk/state.Store.Lookup returned the engine's live
// *domain.Portfolio and told the caller, in its own doc comment, to "read it
// while holding the appropriate lock". The lock is s.locks[id] — unexported,
// with no accessor — so the instruction named a lock the caller could not
// reach. Every other read on the Store (Snapshot, SnapshotWithKeys,
// SnapshotOwned) already returned port.Clone() taken under that lock; Lookup
// was the one door that did not.
//
// The second half is what made it worse than an ordinary escaped pointer.
// domain.Portfolio.Positions() is not a pure read: on the first call after a
// mutation it WRITES p.sortedCache, the memoised sorted view that LATENCY-01c
// added. So the natural thing to do with a Lookup result — range its
// positions — is a write, on a value the ingest goroutine is concurrently
// applying events to. Two goroutines, one of them writing a slice header the
// other reads, on the path where the number that comes out is what a limit is
// checked against.
//
// Nothing had followed the instruction: Lookup had zero non-test callers, so
// this was a loaded footgun rather than a live bug. It is guarded rather than
// merely deleted because the next person to want a cheap read will write it
// again, and will be reading neighbours that all look correct.
//
// # Why the memoisation stayed and the accessor went
//
// The obvious alternative — make Positions() non-memoising, so the accessor is
// genuinely a read — was measured and rejected. Dropping the cache takes
// BenchmarkComputeMeasures from 356–444µs to 2.25–2.53ms at n=1024 (5.7×) and
// from 23 to 59 allocs/op, 331KB/op to 1.14MB/op (12th-gen i5, 2026-08-30).
// compute/regression_test.go pins the warm call at zero allocations for that
// reason. The memoisation is load-bearing; the shared pointer was not.
//
// # THE RULE
//
// In internal/risk/state, no exported method on the Store may return a value
// derived from the live portfolio map unless that value passed through
// Clone(). A zero value, a nil, and anything unrelated to the map are all
// fine. This covers the pointer itself (Snapshot, SnapshotOwned) and values
// BUILT from it (Release returns a persist.PortfolioRecord whose Positions
// field would otherwise alias the live memoised slice — and building it would
// perform that memoising write on the shared copy).
//
// SCOPE AND LIMITS, so a green run is not read for more than it carries:
//
//   - Syntactic taint, in the shape of pricing_store_read_holds_its_lock_test.go.
//     A portfolio reached through a helper, a closure stored in a variable, or
//     an interface will not be seen. That direction fails OPEN, which is why
//     the fixture arms below pin the analyser against the exact pre-#821
//     Lookup rather than trusting the tree to keep a specimen of it.
//   - This is NOT a race detector. -race needs cgo and does not run on the
//     Windows box; CI is the only detector. What this proves is that the shape
//     which made the race reachable cannot come back unnoticed.
//   - It says nothing about what a caller does with a Clone. A clone is owned
//     by one goroutine and the memoising write on it is safe; that is the
//     property this guard preserves, not one it checks.

// portfolioSource is one parsed non-test file under internal/risk/state.
type portfolioSource struct {
	rel  string
	fset *token.FileSet
	file *ast.File
}

// liveStateEscape is one returned value derived from the live portfolio map
// without passing through Clone().
type liveStateEscape struct {
	site   string // file:line of the return
	method string
	what   string
}

// stateScope is the package whose reads must leave by clone.
const stateScope = "internal/risk/state"

// liveStateEscapeExempt maps "<file>:<line>" of a RETURN to the reason it may
// hand out state derived from the live portfolio map. DEFAULT-DENY and empty:
// every read out of this package clones today, and an entry would be somebody
// deciding on purpose that a caller outside internal/risk/state may hold a
// pointer the ingest goroutine is writing. Keyed on the return's position, so
// moving it un-certifies it. An entry must name the issue that retires it.
var liveStateEscapeExempt = map[string]string{}

// stateFiles parses every non-test .go file directly under internal/risk/state.
// parser mode 0: comments are NOT attached, so nothing below can match this
// guard's own prose, a doc comment naming `portfolios`, or the package doc that
// discusses Clone at length — the failure mode that has already let three
// guards in this tree pass with the checked thing deleted.
func stateFiles(t *testing.T, root string) []*portfolioSource {
	t.Helper()
	base := filepath.Join(root, filepath.FromSlash(stateScope))
	var out []*portfolioSource
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// skipWalkDir carries ".claude" — see walk_skip_test.go. A walk that
			// descends into an agent worktree reads another checkout as the estate.
			if path != base && (skipWalkDir(d) || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			if path != base {
				return filepath.SkipDir // sub-packages (persist) own their own rules
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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
		out = append(out, &portfolioSource{rel: filepath.ToSlash(rel), fset: fset, file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", stateScope, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// liveStateField is the struct field holding the live portfolios, DERIVED from
// the declaration rather than named here: the field of a struct in this package
// whose type mentions domain.Portfolio. isMap records whether it is a map, which
// decides whether a `for k := range` key is portfolio-derived (it is not).
type liveStateField struct {
	structName string
	field      string
	isMap      bool
}

// liveStateFields finds the struct fields that hold live portfolios. Deriving
// them from the type declaration is what keeps this guard honest when the Store
// grows a second container or renames the one it has — a hand-written
// "portfolios" would silently stop matching and the scan below would find
// nothing to distrust.
func liveStateFields(f *ast.File) []liveStateField {
	var out []liveStateField
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, fld := range st.Fields.List {
			if !mentionsPortfolioType(fld.Type) {
				continue
			}
			_, isMap := fld.Type.(*ast.MapType)
			for _, nm := range fld.Names {
				out = append(out, liveStateField{structName: ts.Name.Name, field: nm.Name, isMap: isMap})
			}
		}
		return true
	})
	return out
}

// mentionsPortfolioType reports whether a type expression names
// domain.Portfolio anywhere inside it (a pointer, a map value, a slice element).
func mentionsPortfolioType(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Portfolio" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "domain" {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// containsClone reports whether e contains a call to a method named Clone. That
// is the cleansing operation: a value that went through Clone() is a deep copy
// the caller owns outright, which is exactly what makes the memoising
// Positions() write on it safe.
func containsClone(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Clone" {
			found = true
			return false
		}
		return true
	})
	return found
}

// liveAnalysis is what one scanned source contributes: the escapes it holds,
// plus the counters the non-vacuity arms assert on.
type liveAnalysis struct {
	escapes  []liveStateEscape
	fields   int // live-portfolio fields derived from struct declarations
	methods  int // exported methods on the struct that holds them
	derived  int // returned expressions that are derived from the live map
	cleansed int // ...of which passed through Clone()
}

// analyseLiveState walks one file and reports every exported method that
// returns a value derived from the live portfolio map without cloning it.
func (src *portfolioSource) analyseLiveState() liveAnalysis {
	var a liveAnalysis
	fields := liveStateFields(src.file)
	a.fields = len(fields)

	byStruct := map[string][]liveStateField{}
	for _, fld := range fields {
		byStruct[fld.structName] = append(byStruct[fld.structName], fld)
	}
	// The holder is whichever struct in this package declares them; a file that
	// declares none still has to be scanned for methods on the holder declared
	// elsewhere, so the set is unioned across the package by the caller.
	for _, d := range src.file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		_, recv := receiverType(fd)
		if recv == "" || !fd.Name.IsExported() {
			continue
		}
		a.methods++
		a.merge(src, fd, recv, packageLiveFields)
	}
	return a
}

// packageLiveFields is the union of live-portfolio fields across the package,
// set by the test before analysis. A method lives in ownership.go while the
// struct that carries the map is declared in store.go, so a per-file view would
// miss every method that matters.
var packageLiveFields []liveStateField

func (a *liveAnalysis) merge(src *portfolioSource, fd *ast.FuncDecl, recv string, fields []liveStateField) {
	if len(fields) == 0 {
		return
	}
	names := map[string]bool{}
	mapField := map[string]bool{}
	for _, f := range fields {
		names[f.field] = true
		mapField[f.field] = f.isMap
	}

	mentionsLive := func(e ast.Expr, tainted map[string]bool) bool {
		found := false
		ast.Inspect(e, func(n ast.Node) bool {
			if found {
				return false
			}
			switch v := n.(type) {
			case *ast.CallExpr:
				// len(s.portfolios) and cap(...) yield an int. Counting them as a
				// mention taints Store.IDs()'s `out := make([]ID, 0, len(s.portfolios))`
				// and the guard then flags a slice of map keys as an escaped
				// portfolio — a false positive whose fix is an exemption on
				// correct code.
				if id, ok := v.Fun.(*ast.Ident); ok && (id.Name == "len" || id.Name == "cap") {
					return false
				}
				return true
			case *ast.SelectorExpr:
				if id, ok := v.X.(*ast.Ident); ok && id.Name == recv && names[v.Sel.Name] {
					found = true
					return false
				}
			case *ast.Ident:
				if tainted[v.Name] {
					found = true
					return false
				}
			}
			return true
		})
		return found
	}

	// Taint to a fixpoint. A binding whose right-hand side goes through Clone()
	// does NOT taint its target: that is the whole point of the cleansing rule,
	// and without it `cp := port.Clone(); return cp` would be flagged and the
	// guard would reject the repair.
	tainted := map[string]bool{}
	bind := func(lhs, rhs []ast.Expr) bool {
		changed := false
		for _, r := range rhs {
			if !mentionsLive(r, tainted) || containsClone(r) {
				continue
			}
			for _, l := range lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name != "_" && !tainted[id.Name] {
					tainted[id.Name] = true
					changed = true
				}
			}
		}
		return changed
	}
	for pass := 0; pass < 8; pass++ {
		changed := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.AssignStmt:
				lhs := v.Lhs
				// COMMA-OK BINDS A BOOL, NOT A PORTFOLIO. `port, ok := s.portfolios[id]`
				// taints port; ok is presence, and tainting it makes every
				// `return nil, ok` read as a second escape at the same site.
				if len(v.Rhs) == 1 && len(v.Lhs) == 2 {
					lhs = v.Lhs[:1]
				}
				if bind(lhs, v.Rhs) {
					changed = true
				}
			case *ast.ValueSpec:
				var lhs []ast.Expr
				for _, nm := range v.Names {
					lhs = append(lhs, nm)
				}
				if bind(lhs, v.Values) {
					changed = true
				}
			case *ast.RangeStmt:
				// RANGING A MAP OF PORTFOLIOS TAINTS THE VALUE, NOT THE KEY.
				// Store.IDs() does `for id := range s.portfolios` and returns the
				// ids; tainting the key would flag it, and the fix a reader would
				// reach for is an exemption on correct code.
				var lhs []ast.Expr
				isMapRange := false
				if sel, ok := v.X.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
						isMapRange = mapField[sel.Sel.Name]
					}
				}
				if v.Key != nil && !isMapRange {
					lhs = append(lhs, v.Key)
				}
				if v.Value != nil {
					lhs = append(lhs, v.Value)
				}
				if bind(lhs, []ast.Expr{v.X}) {
					changed = true
				}
			}
			return true
		})
		if !changed {
			break
		}
	}

	// Returns inside a nested function literal belong to that literal, not to
	// this method's result list, so their index would be meaningless.
	inLiteral := map[*ast.ReturnStmt]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			return true
		}
		ast.Inspect(lit.Body, func(m ast.Node) bool {
			if r, ok := m.(*ast.ReturnStmt); ok {
				inLiteral[r] = true
			}
			return true
		})
		return true
	})

	ast.Inspect(fd.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || inLiteral[ret] {
			return true
		}
		if len(ret.Results) == 0 {
			// A NAKED RETURN IS NOT ANALYSABLE HERE and is therefore refused
			// rather than waved through: the named result could hold the live
			// pointer and this guard would report nothing. There are none today.
			if fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
				a.escapes = append(a.escapes, liveStateEscape{
					site:   src.where(ret.Pos()),
					method: fd.Name.Name,
					what:   "a naked return, which this guard cannot trace to a cloned value",
				})
			}
			return true
		}
		for i, res := range ret.Results {
			if !mentionsLive(res, tainted) {
				continue
			}
			a.derived++
			if containsClone(res) {
				a.cleansed++
				continue
			}
			a.escapes = append(a.escapes, liveStateEscape{
				site:   src.where(res.Pos()),
				method: fd.Name.Name,
				what:   fmt.Sprintf("result %d is derived from the live portfolio map and never passes through Clone()", i),
			})
		}
		return true
	})
}

func (src *portfolioSource) where(pos token.Pos) string {
	p := src.fset.Position(pos)
	return fmt.Sprintf("%s:%d", src.rel, p.Line)
}

// analyseLiveText runs the same analysis over a source literal. It is what makes
// the fixture arms possible: the pre-#821 Lookup can be pinned here forever
// without leaving a live-pointer accessor in the tree for the guard to find.
func analyseLiveText(t *testing.T, name, source string) liveAnalysis {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, source, 0)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	src := &portfolioSource{rel: name, fset: fset, file: f}
	fields := liveStateFields(f)
	if len(fields) == 0 {
		t.Fatalf("fixture %s declares no live-portfolio field — the fixture, not the tree, is broken", name)
	}
	saved := packageLiveFields
	packageLiveFields = fields
	defer func() { packageLiveFields = saved }()
	return src.analyseLiveState()
}

// liveStoreFixture wraps method bodies in the smallest Store that carries the
// live map, so the fixtures differ ONLY in how they return from it.
func liveStoreFixture(methods string) string {
	return `package fixture

import (
	"sync"

	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

type PortfolioID string

type Store struct {
	mu         sync.Mutex
	portfolios map[PortfolioID]*domain.Portfolio
	locks      map[PortfolioID]*sync.Mutex
}

var _ = persist.PortfolioRecord{}

` + methods
}

// pre821Lookup is state.Store.Lookup as it stood before #821, transcribed. The
// guard has no list of method names, so flagging this proves it catches a member
// it has never seen.
const pre821Lookup = `func (s *Store) Lookup(id PortfolioID) (*domain.Portfolio, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.portfolios[id]
	return p, ok
}`

// borrowDirect is the shape somebody writes next, under a name nobody has used
// before, skipping the local entirely.
const borrowDirect = `func (s *Store) Borrow(id PortfolioID) *domain.Portfolio {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.portfolios[id]
}`

// recordWithoutClone is the value-shaped escape: no pointer leaves, but the
// record's Positions field aliases the live memoised slice, and building it
// performs the memoising write on the shared copy.
const recordWithoutClone = `func (s *Store) Drop(id PortfolioID) (persist.PortfolioRecord, bool) {
	s.mu.Lock()
	port, ok := s.portfolios[id]
	delete(s.portfolios, id)
	s.mu.Unlock()
	if !ok {
		return persist.PortfolioRecord{}, false
	}
	return persist.FromPortfolio(port, nil), true
}`

// clonedSnapshot is the shape the tree runs: the clone is taken under the
// per-aggregate lock and the pointer never escapes.
const clonedSnapshot = `func (s *Store) Snapshot(id PortfolioID) (*domain.Portfolio, bool) {
	s.mu.Lock()
	lock := s.locks[id]
	port, ok := s.portfolios[id]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	return port.Clone(), true
}`

// clonedViaLocal is the same thing spelled with an intermediate variable. It
// must NOT be flagged, or the cleansing rule is really "the word Clone must
// appear in the return statement" and the guard would reject a correct repair.
const clonedViaLocal = `func (s *Store) Copy(id PortfolioID) (*domain.Portfolio, bool) {
	s.mu.Lock()
	port, ok := s.portfolios[id]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	cp := port.Clone()
	return cp, true
}`

// idsFromKeys is Store.IDs(): ranging the map for its KEYS is not a portfolio
// escape, and flagging it would push a reader towards exempting correct code.
const idsFromKeys = `func (s *Store) IDs() []PortfolioID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PortfolioID, 0, len(s.portfolios))
	for id := range s.portfolios {
		out = append(out, id)
	}
	return out
}`

func TestRiskStateHandsOutNoLivePortfolio(t *testing.T) {
	// FIXTURE ARM 1 (positive control). The analyser must flag the exact code
	// #821 removed. Without this arm, a scanner that silently stopped matching
	// would report a clean estate forever.
	if a := analyseLiveText(t, "pre821.go", liveStoreFixture(pre821Lookup)); len(a.escapes) == 0 {
		t.Fatal("the analyser did NOT flag the pre-#821 Store.Lookup, which returned the live " +
			"*domain.Portfolio from the map. The scanner is broken — every arm below is vacuous " +
			"until it flags this fixture again.")
	}

	// FIXTURE ARM 2 (positive control, a shape and a name it has never seen).
	if a := analyseLiveText(t, "borrow.go", liveStoreFixture(borrowDirect)); len(a.escapes) == 0 {
		t.Fatal("the analyser did NOT flag a direct `return s.portfolios[id]` under a method name " +
			"it has no list of. The rule must be derived from the field, not from known method names.")
	}

	// FIXTURE ARM 3 (positive control, the value-shaped escape). No pointer
	// leaves, and it is still an escape: the record aliases the live memoised
	// slice and building it writes that cache on the shared copy.
	if a := analyseLiveText(t, "record.go", liveStoreFixture(recordWithoutClone)); len(a.escapes) == 0 {
		t.Fatal("the analyser did NOT flag persist.FromPortfolio(port, nil) on the LIVE portfolio. " +
			"A guard that only looks at *domain.Portfolio results misses Release's shape entirely.")
	}

	// FIXTURE ARM 4 (negative control). The fix must be clean, or the guard is a
	// ban on reading the map and would have failed the change that resolved #821.
	if a := analyseLiveText(t, "snapshot.go", liveStoreFixture(clonedSnapshot)); len(a.escapes) != 0 {
		t.Fatalf("the analyser flagged the CLONING shape: %+v. A guard that rejects the repair "+
			"teaches people to work around it.", a.escapes)
	}

	// FIXTURE ARM 5 (negative control). Cleansing must survive an intermediate
	// variable, or the rule degenerates into a text match on the return line.
	if a := analyseLiveText(t, "local.go", liveStoreFixture(clonedViaLocal)); len(a.escapes) != 0 {
		t.Fatalf("the analyser flagged a clone bound to a local first: %+v", a.escapes)
	}

	// FIXTURE ARM 6 (negative control). Map KEYS are not portfolios.
	if a := analyseLiveText(t, "ids.go", liveStoreFixture(idsFromKeys)); len(a.escapes) != 0 {
		t.Fatalf("the analyser flagged Store.IDs(), which returns map keys: %+v. Ranging a map of "+
			"portfolios taints the value, never the key.", a.escapes)
	}

	root := moduleRoot(t)
	sources := stateFiles(t, root)

	// The struct that holds the live map is declared in one file and has methods
	// in another, so the field set is unioned across the package before any
	// method is analysed.
	var fields []liveStateField
	for _, src := range sources {
		fields = append(fields, liveStateFields(src.file)...)
	}
	saved := packageLiveFields
	packageLiveFields = fields
	defer func() { packageLiveFields = saved }()

	var (
		all     liveAnalysis
		escapes []liveStateEscape
	)
	for _, src := range sources {
		a := src.analyseLiveState()
		all.methods += a.methods
		all.derived += a.derived
		all.cleansed += a.cleansed
		escapes = append(escapes, a.escapes...)
	}
	all.fields = len(fields)

	// NON-VACUITY 1: the live map must have been found. Zero means the struct
	// moved, the field was renamed, or domain.Portfolio stopped appearing in its
	// type — and every arm below then scans for a taint source that does not
	// exist and finds nothing wrong with anything.
	if all.fields < 1 {
		t.Fatalf("found no struct field in %s whose type mentions domain.Portfolio — the live "+
			"state map has moved or been renamed, and this guard is asserting nothing", stateScope)
	}

	// NON-VACUITY 2: the Store's exported surface. Snapshot, SnapshotWithKeys,
	// SnapshotOwned, IDs, Restore, Release, Acquire, Owns, OwnershipManaged and
	// the three Apply methods are all there.
	if all.methods < 10 {
		t.Fatalf("found only %d exported method(s) with a receiver in %s — the walk or the "+
			"receiver match has stopped working, so the scan runs over almost nothing",
			all.methods, stateScope)
	}

	// NON-VACUITY 3: the taint must actually reach a return. Snapshot,
	// SnapshotWithKeys, SnapshotOwned and Release each return something derived
	// from the map; if this is zero the analysis has stopped propagating and a
	// live pointer would no longer be recognised on its way out.
	if all.derived < 4 {
		t.Fatalf("the taint analysis reached only %d returned expression(s) derived from the live "+
			"portfolio map, across %d exported methods. Snapshot, SnapshotWithKeys, SnapshotOwned "+
			"and Release all return one, so this should be at least four — the analysis has "+
			"stopped propagating", all.derived, all.methods)
	}
	// DEAD-ENTRY CHECK: an exemption naming a return that no longer escapes reads
	// as a reviewed decision while protecting nothing.
	live := map[string]bool{}
	for _, e := range escapes {
		live[e.site] = true
	}
	var dead []string
	for site := range liveStateEscapeExempt {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("liveStateEscapeExempt names %d return(s) that no longer hand out live state: %s\n\n"+
			"Either the code moved (update the file:line key) or it was fixed (delete the entry). "+
			"A stale exemption cannot outlive the thing it excused.", len(dead), strings.Join(dead, ", "))
	}

	var offending []liveStateEscape
	for _, e := range escapes {
		if _, ok := liveStateEscapeExempt[e.site]; ok {
			continue
		}
		offending = append(offending, e)
	}
	// NON-VACUITY 4, and it runs AFTER the escape report on purpose. It is
	// reachable only when nothing is being flagged, which is exactly when a
	// count of zero would otherwise read as a clean estate: either every
	// cloning read has been exempted away, or the reads have stopped cloning
	// AND stopped being recognised. Ordering it before the report would have
	// answered a live escape with "the cleansing detector has stopped
	// matching", which is the wrong diagnosis pointed at the wrong file — it
	// did, while this guard was being mutation-tested.
	if len(offending) == 0 && all.cleansed < 4 {
		t.Fatalf("only %d of %d portfolio-derived returns pass through Clone(), and nothing was "+
			"flagged. Snapshot, SnapshotWithKeys, SnapshotOwned and Release each clone, so "+
			"four is the floor — either the exemption map has swallowed them or the analysis "+
			"has stopped seeing them", all.cleansed, all.derived)
	}

	if len(offending) == 0 {
		return
	}
	sort.Slice(offending, func(i, j int) bool { return offending[i].site < offending[j].site })
	var lines []string
	for _, e := range offending {
		lines = append(lines, fmt.Sprintf("%s: Store.%s — %s", e.site, e.method, e.what))
	}
	t.Fatalf("%d exported read(s) in %s hand out state derived from the live portfolio map:\n  %s\n\n"+
		"The caller cannot make that safe. The per-aggregate lock that serializes applies is "+
		"s.locks[id] — unexported, with no accessor — so a doc telling the caller to hold \"the "+
		"appropriate lock\" names a lock it cannot reach. That is what #821 was.\n\n"+
		"And the natural use of the result is a WRITE: domain.Portfolio.Positions() memoises its "+
		"sorted view on the receiver, so ranging the positions of a shared portfolio races the "+
		"ingest goroutine's applies. The number that comes out of that race is what a limit is "+
		"checked against.\n\n"+
		"Return port.Clone(), taken under the per-aggregate lock, the way Snapshot, "+
		"SnapshotWithKeys and SnapshotOwned already do. The clone is cheap (tens to hundreds of "+
		"value-copied positions) and it is what makes the memoisation safe rather than merely "+
		"unobserved. -race would catch the resulting race too, but -race needs cgo and does not "+
		"run on the Windows box, so CI is the only detector and this guard is what stands in "+
		"front of it.",
		len(offending), stateScope, strings.Join(lines, "\n  "))
}

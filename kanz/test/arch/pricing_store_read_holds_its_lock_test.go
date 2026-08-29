package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A PRICING STORE MUST NOT READ ITS GUARDED STATE AFTER RELEASING THE LOCK.
//
// internal/risk/pricing holds a family of point-in-time stores that are one
// concept written four times on purpose — curve.Store (rates), volsurface.Store
// (implied vol), credit.Store (hazard curves) and livequote.LiveQuotes (last
// value). Each is a mutex plus a container, each is published to by the
// internal/schedule scheduler (one goroutine per Job) and read from the pricing
// path, and credit/store.go says in its own package doc that it mirrors the
// other two "down to the method names". The fourth was written by someone
// reading the first three, and the fifth will be too.
//
// volsurface.Store.Vol was the one that diverged (#800). It took RLock, ran the
// map lookup and the sort.Search, released, and THEN dereferenced vs[i-1]. The
// slice header it held was a copy; the backing array was not — Put's
// out-of-order branch does copy(vs[i+1:], vs[i:]) in place whenever cap exceeds
// len, so an index resolved under the lock addressed a different version by the
// time it was read. Two goroutines against that shape returned the
// second-newest surface 114 times in 4.19M reads over 20s. Nothing crashed:
// what came back was a plausible implied vol from the wrong as-of, feeding
// compute.Greeks and revaluation, and a wrong risk number is what a limit is
// checked against.
//
// THE RULE. Inside internal/risk/pricing, once a method releases the mutex it
// may not touch the guarded state again — not through the receiver's fields,
// and not through a local that was assigned from them. A deferred unlock
// satisfies this by construction (it runs after the return value is computed),
// which is why all four stores now spell it that way; a manual unlock is still
// allowed, but only when nothing guarded is read afterwards
// (livequote.Update ends on one).
//
// SCOPE AND LIMITS, so nobody reads more into a green run than it carries:
//
//   - Syntactic. "Guarded state" is every field of a struct that also has a
//     sync.Mutex/sync.RWMutex field, and the taint is over local identifiers
//     assigned from expressions mentioning those fields, transitively. A read
//     that reaches the container through a helper method, a closure captured
//     into another function, or an interface will not be seen. That direction
//     fails OPEN — hence the fixture arm below, which pins the analyser against
//     the exact pre-#800 code rather than trusting the tree to keep a specimen.
//   - This is NOT a race detector and does not replace one. -race needs cgo and
//     does not run on the Windows box; CI is the only detector. What this proves
//     is that the shape which produced the #800 defect cannot come back
//     unnoticed.
//   - Only internal/risk/pricing/** is scanned. The rule is a property of this
//     store family, not a module-wide ban on manual unlocking.

// lockedStruct is one struct that carries its own mutex: the name, the mutex
// field names, and every other field, which is what "guarded state" means here.
type lockedStruct struct {
	name   string
	mutex  map[string]bool
	fields map[string]bool
}

// pricingSource is one parsed non-test file under internal/risk/pricing.
type pricingSource struct {
	rel     string
	fset    *token.FileSet
	file    *ast.File
	structs map[string]lockedStruct
}

// lockViolation is one read of guarded state that happens after the lock is
// gone.
type lockViolation struct {
	site   string // file:line of the offending read
	method string
	unlock string // file:line of the unlock that preceded it
	what   string
}

// pricingFiles parses every non-test .go file under internal/risk/pricing.
// parser mode 0: comments are NOT attached, so nothing below can match this
// guard's own prose or a doc comment that happens to name a field — the failure
// mode that has already let three guards in this tree pass with the checked
// thing deleted.
func pricingFiles(t *testing.T, root string) []*pricingSource {
	t.Helper()
	base := filepath.Join(root, "internal", "risk", "pricing")
	var out []*pricingSource
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
		out = append(out, &pricingSource{
			rel:     filepath.ToSlash(rel),
			fset:    fset,
			file:    f,
			structs: lockedStructs(f),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", base, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// lockedStructs returns the file's structs that embed their own mutex.
func lockedStructs(f *ast.File) map[string]lockedStruct {
	out := map[string]lockedStruct{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		ls := lockedStruct{name: ts.Name.Name, mutex: map[string]bool{}, fields: map[string]bool{}}
		for _, field := range st.Fields.List {
			isMutex := isSelectorType(field.Type, "sync", "Mutex") || isSelectorType(field.Type, "sync", "RWMutex")
			for _, nm := range field.Names {
				if isMutex {
					ls.mutex[nm.Name] = true
				} else {
					ls.fields[nm.Name] = true
				}
			}
		}
		if len(ls.mutex) > 0 && len(ls.fields) > 0 {
			out[ts.Name.Name] = ls
		}
		return true
	})
	return out
}

// receiverType names the (possibly pointer) receiver's type and its identifier.
func receiverType(fd *ast.FuncDecl) (typeName, recvName string) {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return "", ""
	}
	field := fd.Recv.List[0]
	expr := field.Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return "", ""
	}
	if len(field.Names) == 1 {
		recvName = field.Names[0].Name
	}
	return id.Name, recvName
}

// readsGuarded reports whether n contains a selector `recv.<guardedField>`.
func readsGuarded(n ast.Node, recv string, ls lockedStruct) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && id.Name == recv && ls.fields[sel.Sel.Name] {
			found = true
			return false
		}
		return true
	})
	return found
}

// taintedLocals returns the local identifiers whose value is derived from the
// guarded state, to a fixpoint: `vs := st.byUnderlying[id]` taints vs, and
// anything later assigned from vs is tainted too. Assignments and range clauses
// both count; the analysis is flow-insensitive on purpose, because a local that
// is EVER bound to the container is a local this guard has to distrust after an
// unlock.
func taintedLocals(body *ast.BlockStmt, recv string, ls lockedStruct) map[string]bool {
	tainted := map[string]bool{}
	mentionsTainted := func(e ast.Expr) bool {
		if readsGuarded(e, recv, ls) {
			return true
		}
		hit := false
		ast.Inspect(e, func(node ast.Node) bool {
			if hit {
				return false
			}
			if id, ok := node.(*ast.Ident); ok && tainted[id.Name] {
				hit = true
				return false
			}
			return true
		})
		return hit
	}
	bind := func(lhs []ast.Expr, rhs []ast.Expr) bool {
		changed := false
		for _, r := range rhs {
			if !mentionsTainted(r) {
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
	// Repeat to a fixpoint: a taint introduced late in the body still has to
	// reach an assignment that appeared earlier in source order (a loop).
	for pass := 0; pass < 8; pass++ {
		changed := false
		ast.Inspect(body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.AssignStmt:
				if bind(v.Lhs, v.Rhs) {
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
				var lhs []ast.Expr
				if v.Key != nil {
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
	return tainted
}

// manualUnlocks returns the non-deferred Unlock/RUnlock calls on the receiver's
// own mutex. A deferred unlock is excluded because it runs after the return
// value is computed, which is exactly the property being enforced.
func manualUnlocks(fd *ast.FuncDecl, recv string, ls lockedStruct) []*ast.CallExpr {
	deferred := map[*ast.CallExpr]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if d, ok := n.(*ast.DeferStmt); ok {
			deferred[d.Call] = true
		}
		return true
	})
	var out []*ast.CallExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || deferred[call] {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Unlock" && sel.Sel.Name != "RUnlock") {
			return true
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || !ls.mutex[inner.Sel.Name] {
			return true
		}
		if id, ok := inner.X.(*ast.Ident); ok && id.Name == recv {
			out = append(out, call)
		}
		return true
	})
	return out
}

// analysis is what one scanned source contributes: the violations it holds, and
// the counters the non-vacuity arms assert on.
type analysis struct {
	violations []lockViolation
	stores     int // mutex-bearing structs seen
	methods    int // methods on them with a lock acquisition
	unlocks    int // manual (non-deferred) unlocks seen
	tainted    int // methods where the taint analysis bound at least one local
}

// analyse walks one file and reports every guarded read that happens after a
// manual unlock.
func (src *pricingSource) analyse() analysis {
	var a analysis
	a.stores = len(src.structs)
	for _, d := range src.file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		typeName, recv := receiverType(fd)
		if recv == "" {
			continue
		}
		ls, ok := src.structs[typeName]
		if !ok {
			continue
		}
		a.methods++
		tainted := taintedLocals(fd.Body, recv, ls)
		if len(tainted) > 0 {
			a.tainted++
		}
		for _, unlock := range manualUnlocks(fd, recv, ls) {
			a.unlocks++
			after := unlock.End()
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if n == nil || n.Pos() < after {
					return true
				}
				var what string
				switch v := n.(type) {
				case *ast.SelectorExpr:
					if id, ok := v.X.(*ast.Ident); ok && id.Name == recv && ls.fields[v.Sel.Name] {
						what = fmt.Sprintf("%s.%s", recv, v.Sel.Name)
					}
				case *ast.Ident:
					if tainted[v.Name] {
						what = fmt.Sprintf("%s (bound to %s's guarded state under the lock)", v.Name, recv)
					}
				}
				if what == "" {
					return true
				}
				a.violations = append(a.violations, lockViolation{
					site:   src.where(n.Pos()),
					method: fmt.Sprintf("%s.%s", typeName, fd.Name.Name),
					unlock: src.where(unlock.Pos()),
					what:   what,
				})
				return false
			})
		}
	}
	return a
}

func (src *pricingSource) where(pos token.Pos) string {
	p := src.fset.Position(pos)
	return fmt.Sprintf("%s:%d", src.rel, p.Line)
}

// analyseText runs the same analysis over a source literal. It is what makes
// the fixture arms below possible: the pre-#800 shape can be pinned here
// forever without leaving a broken store in the tree for the guard to find.
func analyseText(t *testing.T, name, source string) analysis {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, source, 0)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	src := &pricingSource{rel: name, fset: fset, file: f, structs: lockedStructs(f)}
	if len(src.structs) == 0 {
		t.Fatalf("fixture %s declares no mutex-bearing struct — the fixture, not the tree, is broken", name)
	}
	return src.analyse()
}

// storeFixture wraps a method body in the smallest store that carries a mutex
// and a versioned container, so the two fixtures differ ONLY in where the
// unlock sits.
func storeFixture(method string) string {
	return `package fixture

import (
	"sort"
	"sync"
	"time"
)

type Store struct {
	mu           sync.RWMutex
	byUnderlying map[string][]version
}

type version struct {
	asOf time.Time
	s    *surface
}

type surface struct{}

func (x *surface) Vol(strike, ttm float64) (float64, bool) { return 0, true }

` + method
}

// pre800Vol is volsurface.Store.Vol as it stood before #800, transcribed.
const pre800Vol = `func (st *Store) Vol(underlyingID string, strike, ttmYears float64, asOf time.Time) (float64, bool) {
	st.mu.RLock()
	vs := st.byUnderlying[underlyingID]
	i := sort.Search(len(vs), func(i int) bool { return vs[i].asOf.After(asOf) })
	st.mu.RUnlock()
	if i == 0 {
		return 0, false
	}
	return vs[i-1].s.Vol(strike, ttmYears)
}`

// fixedVol is the shape #800 landed: the unlock is deferred, so the element
// read is inside the critical section.
const fixedVol = `func (st *Store) Vol(underlyingID string, strike, ttmYears float64, asOf time.Time) (float64, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	vs := st.byUnderlying[underlyingID]
	i := sort.Search(len(vs), func(i int) bool { return vs[i].asOf.After(asOf) })
	if i == 0 {
		return 0, false
	}
	return vs[i-1].s.Vol(strike, ttmYears)
}`

// terminalUnlock is the legitimate manual unlock: the lock is released and
// nothing guarded is touched afterwards (livequote.Update's shape). It must NOT
// be flagged, or the rule degenerates into "always defer" and the exemption
// list becomes the guard.
const terminalUnlock = `func (st *Store) Put(underlyingID string, asOf time.Time, s *surface) {
	st.mu.Lock()
	st.byUnderlying[underlyingID] = append(st.byUnderlying[underlyingID], version{asOf: asOf, s: s})
	st.mu.Unlock()
}`

// pricingLateReadExemptions is the default-deny allow-list: a guarded read
// after a manual unlock that has been reviewed and argued for. Keyed on the
// "file:line" of the READ, so moving it un-certifies it. Empty today — #800
// removed the only one, and the bar for adding an entry is a structural
// argument that the value read cannot be mutated by any writer, not "this
// store is not on the capital path."
var pricingLateReadExemptions = map[string]string{}

func TestPricingStoreReadHoldsItsLock(t *testing.T) {
	// FIXTURE ARM 1 (positive control). The analyser must flag the exact code
	// #800 removed. This is the arm that survives someone deleting every real
	// violation from the tree: without it, a scanner that silently stopped
	// matching would report a clean estate forever.
	pre := analyseText(t, "pre800.go", storeFixture(pre800Vol))
	if len(pre.violations) == 0 {
		t.Fatal("the analyser did NOT flag the pre-#800 volsurface.Store.Vol, which released the " +
			"read lock and then dereferenced vs[i-1] out of the array Put shifts in place. The " +
			"scanner is broken — every arm below is vacuous until it flags this fixture again.")
	}

	// FIXTURE ARM 2 (negative control). The fix must be clean, or the guard is
	// just a ban on the word RUnlock and would have failed the very change that
	// resolved #800.
	if fixed := analyseText(t, "fixed.go", storeFixture(fixedVol)); len(fixed.violations) != 0 {
		t.Fatalf("the analyser flagged the FIXED shape (deferred RUnlock): %+v. A guard that "+
			"rejects the repair teaches people to work around it.", fixed.violations)
	}

	// FIXTURE ARM 3 (negative control). A manual unlock with nothing guarded
	// read after it is legitimate and must stay legitimate.
	if term := analyseText(t, "terminal.go", storeFixture(terminalUnlock)); len(term.violations) != 0 {
		t.Fatalf("the analyser flagged a manual unlock that reads nothing afterwards: %+v. "+
			"livequote.Update has this shape and is correct.", term.violations)
	}

	root := moduleRoot(t)
	sources := pricingFiles(t, root)

	var (
		all        analysis
		violations []lockViolation
	)
	for _, src := range sources {
		a := src.analyse()
		all.stores += a.stores
		all.methods += a.methods
		all.unlocks += a.unlocks
		all.tainted += a.tainted
		violations = append(violations, a.violations...)
	}

	// NON-VACUITY 1: the family is four stores — curve, volsurface, credit,
	// livequote. Fewer means the walk or the mutex-field match broke, and the
	// scan below then runs over almost nothing.
	if all.stores < 4 {
		t.Fatalf("found only %d mutex-bearing struct(s) under internal/risk/pricing — curve.Store, "+
			"volsurface.Store, credit.Store and livequote.LiveQuotes are all there, so the walk or "+
			"the sync.Mutex/sync.RWMutex field match has stopped working", all.stores)
	}

	// NON-VACUITY 2: the taint analysis must actually bind something. If it
	// returns empty sets the guard can only ever catch a direct recv.field read
	// after an unlock, which is not the shape #800 had.
	if all.tainted < 4 {
		t.Fatalf("the taint analysis bound a local to guarded state in only %d method(s) across "+
			"%d methods on those stores. Every Curve/Vol/Latest/Instruments does `vs := s.byX[k]` "+
			"or ranges the map, so this should be at least four — the analysis has stopped "+
			"propagating and a late read would no longer be recognised", all.tainted, all.methods)
	}

	// DEAD-ENTRY CHECK: an exemption naming a read that is no longer a late read
	// reads as a reviewed decision while protecting nothing.
	live := map[string]bool{}
	for _, v := range violations {
		live[v.site] = true
	}
	var dead []string
	for site := range pricingLateReadExemptions {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("pricingLateReadExemptions names %d read(s) that are no longer guarded reads after "+
			"a manual unlock: %s\n\nEither the code moved (update the file:line key) or it was "+
			"fixed (delete the entry). A stale exemption cannot outlive the thing it excused.",
			len(dead), strings.Join(dead, ", "))
	}

	var offending []lockViolation
	for _, v := range violations {
		if _, ok := pricingLateReadExemptions[v.site]; ok {
			continue
		}
		offending = append(offending, v)
	}
	if len(offending) == 0 {
		return
	}
	sort.Slice(offending, func(i, j int) bool { return offending[i].site < offending[j].site })
	var lines []string
	for _, v := range offending {
		lines = append(lines, fmt.Sprintf("%s: %s reads %s after the unlock at %s",
			v.site, v.method, v.what, v.unlock))
	}
	t.Fatalf("%d read(s) of a pricing store's guarded state happen after its lock is released:\n  %s\n\n"+
		"The slice header a reader copies out is private; the BACKING ARRAY is not. Put's "+
		"out-of-order branch does copy(vs[i+1:], vs[i:]) in place whenever cap exceeds len, so an "+
		"index resolved under the lock addresses a different version by the time it is read. That "+
		"is what #800 was: volsurface.Store.Vol returned the second-newest surface 114 times in "+
		"4.19M reads against two goroutines — a plausible implied vol from the wrong as_of, "+
		"reaching compute.Greeks and revaluation, with no panic and no log line.\n\n"+
		"Fix it by deferring the unlock, the way curve.Store.Curve and credit.Store.Curve already "+
		"do — the read then happens inside the critical section and the shape is impossible "+
		"rather than unlikely. -race would catch this too, but -race needs "+
		"cgo and does not run on the Windows box, so CI is the only detector and this guard is "+
		"what stands in front of it.",
		len(offending), strings.Join(lines, "\n  "))
}

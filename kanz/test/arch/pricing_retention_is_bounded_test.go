package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// A POINT-IN-TIME PRICING STORE MAY NOT GROW WITHOUT A HORIZON (#811).
//
// # What went wrong
//
// internal/risk/pricing holds three stores that are the same concept written
// three times on purpose — curve.Store (rates), credit.Store (hazard curves)
// and volsurface.Store (implied vol). Each keeps `map[key][]version` ascending
// by as-of, each is published to by the internal/schedule scheduler and read
// from the pricing path, and credit/store.go says in its own package doc that it
// mirrors the other two "down to the method names".
//
// All three argued for point-in-time RETENTION correctly and none of them said
// for how long. `grep -rn "delete(" internal/risk/pricing` returned nothing: a
// calibration refresh appended a version and nothing ever removed one, so the
// retained set was a function of UPTIME rather than of anything financial. The
// risk-engine composition root registers an intraday and a nightly job per
// currency, and at the one-minute cadence RISK_ENGINE_CALIBRATION_INTERVAL
// accepts that is ~525k retained curves per currency per year, in a process that
// drops none of them.
//
// The failure mode is not a crash. Calibration latency rises monotonically
// because every Refresh inserts into a longer list, until refreshes overlap or
// the pod is OOM-killed — and a stale or missing curve on the control plane is a
// wrong risk number, in the direction that makes a limit pass.
//
// # THE RULE
//
// Inside internal/risk/pricing, a versioned container — a field of type
// map[K][]V on a struct that also carries its own mutex — may only be written
// from a value the shared retention package produced. internal/pit
// is that package: pit.Put inserts, prunes past the horizon and reports what it
// dropped, and it is one implementation because a horizon copied into three
// stores is the copied-helper failure mode this repository has already paid for
// (17 services with their own secret(), 15 of them wrong).
//
// The container list is DERIVED from the tree, never enumerated here: a fourth
// store added tomorrow is scanned the day it is written, which is the whole
// point — the fourth member of this family will be written by someone reading
// the first three, exactly as the third was.
//
// # SCOPE AND LIMITS, so nobody reads more into a green run than it carries
//
//   - Syntactic. A write that reaches the container through a helper method, a
//     closure, or an interface is not seen. That direction fails OPEN, which is
//     why the fixture arms below pin the analyser against both the pre-#811
//     shape and a store this repository has never contained.
//   - It proves the write is BOUNDED, not that the bound is RIGHT. The horizon
//     itself is a financial decision argued in pit's package doc and asserted by
//     internal/pit's tests and by the per-store retention tests.
//   - Only internal/risk/pricing/** is scanned. The rule is a property of this
//     store family, not a module-wide ban on append.
//   - It does not cover the CONCEPT module-wide, and widening this walk would not
//     make it: the match is a map-of-slice field on a mutex-carrying struct,
//     which is this family's shape and not volprofile's (a plain slice on an
//     inner history struct, behind the Store's mutex rather than its own).
//     test/arch/one_horizon_prune_test.go is the guard that follows the concept
//     instead of the directory — default-deny over every non-test file in the
//     module — and #871 is why it exists.

// retentionPkg is the one implementation a versioned container may be written
// from.
const retentionPkg = "pit"

// versionedContainer is one guarded map-of-slice field: the struct that holds
// it, and the field name.
type versionedContainer struct {
	structName string
	field      string
}

// unboundedWrite is one write to a versioned container that did not come from
// the shared retention package.
type unboundedWrite struct {
	site   string // file:line of the assignment
	method string
	target string // recv.field
}

// versionedContainers returns the map-of-slice fields of every struct in f that
// also carries a sync.Mutex/sync.RWMutex, keyed by struct name. A container
// without a mutex is not a shared store and is not this rule's business.
func versionedContainers(f *ast.File) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		hasMutex := false
		fields := map[string]bool{}
		for _, field := range st.Fields.List {
			if isSelectorType(field.Type, "sync", "Mutex") || isSelectorType(field.Type, "sync", "RWMutex") {
				hasMutex = true
				continue
			}
			mt, ok := field.Type.(*ast.MapType)
			if !ok {
				continue
			}
			if _, ok := mt.Value.(*ast.ArrayType); !ok {
				continue // map to a single value is a last-value cache, bounded by cardinality
			}
			for _, nm := range field.Names {
				fields[nm.Name] = true
			}
		}
		if hasMutex && len(fields) > 0 {
			out[ts.Name.Name] = fields
		}
		return true
	})
	return out
}

// callsRetention reports whether e contains a call whose function is a selector
// on the retention package (pit.Put, pit.At, ...).
func callsRetention(e ast.Node) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == retentionPkg {
			found = true
			return false
		}
		return true
	})
	return found
}

// boundedLocals returns the locals whose value came from the retention package,
// to a fixpoint: `vs, _ := pit.Put(...)` binds vs, and anything later assigned
// from vs is bounded too.
func boundedLocals(body *ast.BlockStmt) map[string]bool {
	bounded := map[string]bool{}
	mentionsBounded := func(e ast.Expr) bool {
		if callsRetention(e) {
			return true
		}
		hit := false
		ast.Inspect(e, func(n ast.Node) bool {
			if hit {
				return false
			}
			if id, ok := n.(*ast.Ident); ok && bounded[id.Name] {
				hit = true
				return false
			}
			return true
		})
		return hit
	}
	for pass := 0; pass < 8; pass++ {
		changed := false
		ast.Inspect(body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, r := range as.Rhs {
				if !mentionsBounded(r) {
					continue
				}
				for _, l := range as.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name != "_" && !bounded[id.Name] {
						bounded[id.Name] = true
						changed = true
					}
				}
			}
			return true
		})
		if !changed {
			break
		}
	}
	return bounded
}

// containerTarget reports the container field an assignment LHS writes, if any:
// `recv.field = x` or `recv.field[k] = x`.
func containerTarget(lhs ast.Expr, recv string, fields map[string]bool) string {
	if idx, ok := lhs.(*ast.IndexExpr); ok {
		lhs = idx.X
	}
	sel, ok := lhs.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != recv || !fields[sel.Sel.Name] {
		return ""
	}
	return fmt.Sprintf("%s.%s", recv, sel.Sel.Name)
}

// retentionScan is what one scanned source contributes.
type retentionScan struct {
	writes     []unboundedWrite
	containers []versionedContainer
	written    int // container writes seen, bounded or not
}

// scanRetention walks one parsed file and reports every write to a versioned
// container that did not come from the retention package.
func scanRetention(rel string, fset *token.FileSet, f *ast.File) retentionScan {
	var out retentionScan
	byStruct := versionedContainers(f)
	names := make([]string, 0, len(byStruct))
	for s := range byStruct {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		fields := make([]string, 0, len(byStruct[s]))
		for fld := range byStruct[s] {
			fields = append(fields, fld)
		}
		sort.Strings(fields)
		for _, fld := range fields {
			out.containers = append(out.containers, versionedContainer{structName: s, field: fld})
		}
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		typeName, recv := receiverType(fd)
		if recv == "" {
			continue
		}
		fields, ok := byStruct[typeName]
		if !ok {
			continue
		}
		bounded := boundedLocals(fd.Body)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, l := range as.Lhs {
				target := containerTarget(l, recv, fields)
				if target == "" {
					continue
				}
				out.written++
				var rhs ast.Expr
				if i < len(as.Rhs) {
					rhs = as.Rhs[i]
				} else if len(as.Rhs) == 1 {
					rhs = as.Rhs[0]
				}
				if rhs != nil && callsRetention(rhs) {
					continue
				}
				isBounded := false
				if rhs != nil {
					ast.Inspect(rhs, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok && bounded[id.Name] {
							isBounded = true
							return false
						}
						return true
					})
				}
				if isBounded {
					continue
				}
				out.writes = append(out.writes, unboundedWrite{
					site:   fmt.Sprintf("%s:%d", rel, fset.Position(as.Pos()).Line),
					method: fmt.Sprintf("%s.%s", typeName, fd.Name.Name),
					target: target,
				})
			}
			return true
		})
	}
	return out
}

// scanRetentionText runs the same analysis over a source literal, which is what
// makes the fixture arms possible: the pre-#811 shape and a store this tree has
// never held can both be pinned here without leaving an unbounded store in the
// estate for the guard to find.
func scanRetentionText(t *testing.T, name, source string) retentionScan {
	t.Helper()
	scan := scanRetentionTextRaw(t, name, source)
	if len(scan.containers) == 0 {
		t.Fatalf("fixture %s declares no guarded versioned container — the fixture, not the tree, "+
			"is broken", name)
	}
	return scan
}

// scanRetentionTextRaw is scanRetentionText without the "this fixture must
// declare a container" assertion, for the one arm whose whole claim is that a
// shape declares none.
func scanRetentionTextRaw(t *testing.T, name, source string) retentionScan {
	t.Helper()
	fset := token.NewFileSet()
	// mode 0: comments are NOT attached, so nothing here can match this guard's
	// own prose or a doc comment that happens to name a field.
	f, err := parser.ParseFile(fset, name, source, 0)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return scanRetention(name, fset, f)
}

// pre811Put is curve.Store.Put as it stood before #811: a sorted insert that
// appends and never removes.
const pre811Put = `package fixture

import (
	"sort"
	"sync"
	"time"
)

type curveVersion struct {
	asOf time.Time
	c    *Curve
}

type Curve struct{}

type Store struct {
	mu         sync.RWMutex
	byCurrency map[string][]curveVersion
}

func (s *Store) Put(currency string, asOf time.Time, c *Curve) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.byCurrency[currency]
	i := sort.Search(len(vs), func(i int) bool { return !vs[i].asOf.Before(asOf) })
	if i < len(vs) && vs[i].asOf.Equal(asOf) {
		vs[i].c = c
	} else {
		vs = append(vs, curveVersion{})
		copy(vs[i+1:], vs[i:])
		vs[i] = curveVersion{asOf: asOf, c: c}
	}
	s.byCurrency[currency] = vs
}`

// bounded811Put is the shape #811 landed.
const bounded811Put = `package fixture

import (
	"sync"
	"time"

	"github.com/eighred/kanz/internal/pit"
)

type Curve struct{}

type Store struct {
	mu         sync.RWMutex
	byCurrency map[string][]pit.Version[*Curve]
	horizon    time.Duration
}

func (s *Store) Put(currency string, asOf time.Time, c *Curve) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, _ := pit.Put(s.byCurrency[currency], asOf, c, s.horizon)
	s.byCurrency[currency] = vs
}`

// unseenFourthStore is a member of this family that has never existed in this
// repository — an FX-forward point store, written the way the first three were.
// It is the arm that proves the guard covers what it has not been shown, rather
// than recognising three known field names.
const unseenFourthStore = `package fixture

import (
	"sync"
	"time"
)

type fwdVersion struct {
	asOf time.Time
	pts  float64
}

type FwdStore struct {
	guard  sync.Mutex
	byPair map[string][]fwdVersion
}

func (f *FwdStore) Publish(pair string, asOf time.Time, pts float64) {
	f.guard.Lock()
	defer f.guard.Unlock()
	f.byPair[pair] = append(f.byPair[pair], fwdVersion{asOf: asOf, pts: pts})
}`

// lastValueCache is livequote.LiveQuotes' shape: a mutex over a map to a SINGLE
// value rather than to a version list. THIS RULE IS ABOUT RETENTION PER KEY — a
// last-value write keeps one event per key however often it fires, so there is
// no horizon for it to violate. It must NOT be flagged, or the rule degenerates
// into "every map behind a mutex needs a horizon" and the exemption list becomes
// the guard.
//
// The KEY SPACE is a different question and this guard has never answered it.
// The premise here used to be "bounded by instrument cardinality"; #894 measured
// that against the wiring and it was false — the handler is subscribed to the
// `market.>` wildcard, so the real cardinality was the whole spine's instrument
// universe. LiveQuotes now admits only the configured calibration strip and
// TestEveryLongLivedMapHasAnEvictor is where that key-space claim is held.
const lastValueCache = `package fixture

import "sync"

type Event struct{}

type LiveQuotes struct {
	mu     sync.RWMutex
	latest map[string]*Event
}

func (q *LiveQuotes) Update(id string, ev *Event) {
	q.mu.Lock()
	q.latest[id] = ev
	q.mu.Unlock()
}`

// unboundedRetentionExempt is the default-deny allow-list: a versioned-container
// write that has been reviewed and argued for. Keyed on the "file:line" of the
// WRITE, so moving it un-certifies it. Empty today, and the bar for an entry is
// a structural argument that the container cannot grow — not "this store is
// small" and not "this store is not on the capital path", both of which were
// true of the three stores #811 found.
var unboundedRetentionExempt = map[string]string{}

func TestPricingRetentionIsBounded(t *testing.T) {
	// FIXTURE ARM 1 (positive control). The analyser must flag the exact code
	// #811 removed. Without it, a scanner that silently stopped matching would
	// report a bounded estate forever.
	if pre := scanRetentionText(t, "pre811.go", pre811Put); len(pre.writes) == 0 {
		t.Fatal("the analyser did NOT flag the pre-#811 curve.Store.Put, which appended a version " +
			"per calibration refresh and removed none. The scanner is broken — every arm below is " +
			"vacuous until it flags this fixture again.")
	}

	// FIXTURE ARM 2 (negative control). The repair must be clean, or the guard is
	// a ban on the word append and would have rejected the change that resolved
	// #811.
	if fixed := scanRetentionText(t, "bounded811.go", bounded811Put); len(fixed.writes) != 0 {
		t.Fatalf("the analyser flagged the BOUNDED shape (pit.Put): %+v. A guard that rejects the "+
			"repair teaches people to work around it.", fixed.writes)
	}

	// FIXTURE ARM 3 (a member the guard has never seen). The container list is
	// derived, so a fourth store nobody has written yet is covered on the day it
	// appears — which is the only way this rule outlives the three stores that
	// motivated it.
	unseen := scanRetentionText(t, "fwdstore.go", unseenFourthStore)
	if len(unseen.writes) == 0 {
		t.Fatal("the analyser did NOT flag an FX-forward point store this repository has never " +
			"contained, whose Publish appends to a mutex-guarded map[string][]fwdVersion and " +
			"never prunes. The guard is recognising the three known field names rather than the " +
			"shape, so it protects nothing a new store could do wrong.")
	}

	// FIXTURE ARM 4 (negative control). A last-value cache behind a mutex retains
	// one event per key and is not this rule's business — its KEY space is
	// TestEveryLongLivedMapHasAnEvictor's.
	if lv := scanRetentionTextRaw(t, "livequote.go", lastValueCache); len(lv.containers) != 0 {
		t.Fatalf("a mutex-guarded map to a SINGLE value was treated as a versioned container: %+v. "+
			"livequote.LiveQuotes has this shape, keeps one event per instrument, and needs no "+
			"horizon.", lv.containers)
	}

	root := moduleRoot(t)
	sources := pricingFiles(t, root)

	var (
		writes     []unboundedWrite
		containers []versionedContainer
		written    int
	)
	for _, src := range sources {
		scan := scanRetention(src.rel, src.fset, src.file)
		writes = append(writes, scan.writes...)
		containers = append(containers, scan.containers...)
		written += scan.written
	}

	// NON-VACUITY 1: the family is three versioned stores — curve, credit,
	// volsurface. Fewer means the walk or the map-of-slice match broke, and every
	// arm below then runs over almost nothing.
	if len(containers) < 3 {
		t.Fatalf("found %d versioned container(s) under internal/risk/pricing — curve.Store's "+
			"byCurrency, credit.Store's byReference and volsurface.Store's byUnderlying are all "+
			"there, so the walk or the map-of-slice field match has stopped working: %+v",
			len(containers), containers)
	}

	// NON-VACUITY 2: something must actually be WRITTEN. A scan that found the
	// containers but no assignment into them would report a clean estate while
	// checking nothing at all.
	if written < 3 {
		t.Fatalf("found %d write(s) into those %d container(s) — each store's Put assigns its map "+
			"entry, so this should be at least three. The assignment shape moved and this guard is "+
			"asserting nothing", written, len(containers))
	}

	// DEAD-ENTRY CHECK: an exemption naming a write that is no longer unbounded
	// reads as a reviewed decision while protecting nothing.
	live := map[string]bool{}
	for _, w := range writes {
		live[w.site] = true
	}
	var dead []string
	for site := range unboundedRetentionExempt {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("unboundedRetentionExempt names %d write(s) that are no longer unbounded: %s\n\n"+
			"Either the code moved (update the file:line key) or it was fixed (delete the entry). "+
			"A stale exemption cannot outlive the thing it excused.", len(dead), strings.Join(dead, ", "))
	}

	var offending []unboundedWrite
	for _, w := range writes {
		if _, ok := unboundedRetentionExempt[w.site]; ok {
			continue
		}
		offending = append(offending, w)
	}
	if len(offending) == 0 {
		return
	}
	sort.Slice(offending, func(i, j int) bool { return offending[i].site < offending[j].site })
	var lines []string
	for _, w := range offending {
		lines = append(lines, fmt.Sprintf("%s: %s writes %s from a value pit did not produce",
			w.site, w.method, w.target))
	}
	t.Fatalf("%d write(s) to a point-in-time pricing store's version list are not bounded by a "+
		"horizon:\n  %s\n\nA version list that only ever grows makes the pod's pricing inputs a "+
		"function of UPTIME rather than of the book: at the one-minute intraday cadence the "+
		"risk-engine accepts, that is ~525k retained curves per currency per year, with every "+
		"Refresh inserting into a longer list until refreshes overlap or the pod is OOM-killed. "+
		"Route the write through internal/pit — pit.Put inserts, prunes past the "+
		"store's horizon and reports what it dropped — rather than pruning in a fourth place. The "+
		"horizon is a financial decision, argued once in pit's package doc, and three copies of it "+
		"is how a fix stops spreading.",
		len(offending), strings.Join(lines, "\n  "))
}

package arch

// AN OUTBOX RELAY'S BACKGROUND DRAIN IS JOINED BY THE FRAME THAT STARTED IT
// (#815), FOR EVERY ADOPTER.
//
// # Why this file exists rather than one more line in oms_outbox_test.go
//
// oms_outbox_test.go asserted exactly this, for exactly one service, by looking
// at the 400 bytes of source before the literal `relay.Run(ctx)` in
// services/oms/cmd/oms/main.go. The assertion was right and the scope was the
// defect: datamaster became the second composition root to run a relay, did not
// join it, and no guard in this directory looked. Its neighbours could not
// either —
//
//   - consumer_goroutine_join_test.go classifies a goroutine by whether it
//     reaches bus.NewConsumer, and a relay is a PRODUCER;
//   - run_loop_joins_its_goroutines_test.go is scoped to functions named Run*
//     taking a context.Context, and a composition root's frame is `run() int`
//     (#266), which is neither.
//
// So the seam between those two guards is precisely "a goroutine started in a
// composition root that is not a bus consumer", and the outbox relay is the
// thing that lives in it. The join assertion moved here and was generalised;
// oms_outbox_test.go keeps its OMS-specific arms and points at this file.
//
// # What is at stake, measured rather than argued
//
// The relay publishes a FACT to the broker and THEN marks the record published —
// the correct order, since a mark that preceded the publish would lose the FACT
// outright on a crash. Every composition root here is `os.Exit(run())`, so an
// unjoined relay does not wind down when run() returns: it stops existing, and
// if it stops between those two steps the estate holds an announcement the
// outbox table still calls pending. The next process republishes it. It also
// holds a cluster-wide per-key advisory lock across both steps.
// internal/outbox's TestPostgresOutboxARelayLeftRunningIsStillHoldingTheDrainLock
// forces that state against Postgres and asks an INDEPENDENT connection what the
// lock says, rather than taking this paragraph's word for it.
//
// One inherited premise does NOT hold, and is corrected here rather than
// repeated: the OMS comment said that lock "would be released by a connection
// teardown rather than by its own unlock", naming pool.Close(). Measured on pgx
// v5, pool.Close() BLOCKS while a connection is checked out and the held
// connection keeps working, so a relay inside a locked drain does get to run its
// own unlock. What Close refuses is a NEW acquisition ("closed pool", even on an
// uncancelled context). The conclusion survives and the mechanism is worse:
// nothing blocks os.Exit, and Relay's unlock runs on context.Background exactly
// so a cancelled shutdown still releases the lock.
//
// # SCOPE AND LIMITS, so a green run is not read for more than it carries
//
//   - The relay handles are DERIVED per file: an identifier bound to
//     outbox.NewRelay(...) or to a call of the form x.Outbox() — the two ways
//     this module hands a relay to a composition root (#292 keeps construction
//     inside the service so a composition cannot be missing its drain). A relay
//     reached only through a struct field or an interface value is not
//     recognised; that direction fails OPEN, which is why the floors below assert
//     the scan still finds the relays it is supposed to find.
//   - "Joined" is frameJoins() in run_loop_joins_its_goroutines_test.go — one
//     implementation, shared with that guard. It proves the starting frame blocks
//     on the goroutine, NOT that the wait precedes the store teardown. The
//     ordering of those two is asserted where it can be executed rather than
//     read: services/datamaster/cmd/datamaster/loops_test.go.
//   - Comments are not parsed (mode 0), so this file's prose can neither satisfy
//     nor defeat it. Three guards in this directory have passed with the checked
//     thing deleted by matching their own comments.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strings"
	"testing"
)

// relayJoinScanDirs are the module subtrees walked. Composition roots are the
// only place a relay's background Run is started today; the walk is wider than
// that so a service that grows an internal runner is covered on the commit that
// adds it rather than on the day somebody remembers.
var relayJoinScanDirs = []string{"internal", "pkg", "services", "cmd", "tools"}

// unjoinedOutboxRelays is the default-deny allow-list of relay goroutines a
// frame may leave unjoined, keyed "file:line" so a moved call site
// un-certifies itself.
//
// The bar for an entry is a structural argument that the goroutine cannot be
// holding the store when the frame's deferred closers run. There is no such
// argument today, which is why it is empty — and an empty allow-list is not the
// same thing as no allow-list.
var unjoinedOutboxRelays = map[string]string{}

// relayGoroutine is one `go` statement whose body runs an outbox relay.
type relayGoroutine struct {
	site    string // module-relative "file:line"
	frame   string // the function that started it
	handle  string // the relay identifier it calls Run on
	joined  bool
	signals []joinPoint
}

// relayHandles returns the identifiers in f that hold an *outbox.Relay.
//
// DERIVED, NOT LISTED. The two bindings below are the two ways a relay reaches a
// frame in this module: built in place (datamaster) or taken off the thing that
// owns it (the OMS's svc.Outbox(), accounting's folder.Outbox()). A third
// spelling would have to appear here — which is the point, because a hand list
// of SERVICES is the artifact that let datamaster escape oms_outbox_test.go.
func relayHandles(f *ast.File) map[string]bool {
	out := map[string]bool{}
	bind := func(lhs []ast.Expr, rhs []ast.Expr) {
		for i, r := range rhs {
			if i >= len(lhs) {
				break
			}
			call, ok := r.(*ast.CallExpr)
			if !ok {
				continue
			}
			relayish := isSelector(call.Fun, "outbox", "NewRelay")
			if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && sel.Sel.Name == "Outbox" {
				relayish = true
			}
			if !relayish {
				continue
			}
			if id, isID := lhs[i].(*ast.Ident); isID && id.Name != "_" {
				out[id.Name] = true
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			bind(v.Lhs, v.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, 0, len(v.Names))
			for _, name := range v.Names {
				lhs = append(lhs, name)
			}
			bind(lhs, v.Values)
		}
		return true
	})
	return out
}

// runsRelay reports which relay handle g calls Run on, "" if none.
func runsRelay(g *ast.GoStmt, handles map[string]bool) string {
	found := ""
	ast.Inspect(g, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Run" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && handles[id.Name] {
			found = id.Name
			return false
		}
		return true
	})
	return found
}

// relayPkg is one package's non-test files. The unit is the PACKAGE and not the
// file because a composition root's join may be named: datamaster's run() defers
// joinThenClose(cancelLoops, &loops, closeStores), which lives in loops.go
// beside it. A file-scoped analysis would report that as unjoined — a false
// finding whose only repair is to inline the wait, which is the opposite of what
// this guard should encourage.
type relayPkg struct {
	dir   string
	files map[string]*ast.File // module-relative path -> file
}

// namedWaits credits fd with the waits performed by same-package helpers it
// calls, one level deep.
//
// THE SAME RULE consumer_goroutine_join_test.go ALREADY USES, and for the same
// stated reason: `awaitConsumers(&consumers, logger)` blocks the caller exactly
// as an inline `consumers.Wait()` would, and a guard that only understood the
// inline spelling pushes composition roots toward copying the wait rather than
// naming it. A wait is credited only when the identifier waited on is one of the
// helper's PARAMETERS, remapped to the argument passed — a helper waiting on its
// own local is waiting on its own goroutines, not this frame's.
//
// Deferred calls count, and are returned separately: a deferred call runs on
// every return path, so like `defer wg.Wait()` its source position relative to
// the `go` statement carries no meaning. `defer joinThenClose(cancel, &loops,
// closeStores)` is precisely that shape.
//
// One level deep, deliberately. A helper that hands the WaitGroup to a second
// helper is no longer something this can read honestly, and it should fail
// rather than guess.
func (p *relayPkg) namedWaits(fd *ast.FuncDecl, a joinAnalysis, funcs map[string]*ast.FuncDecl) (direct, deferred []joinPoint) {
	inGoStmt := func(pos token.Pos) bool {
		for _, g := range a.goStmts {
			if pos > g.Pos() && pos < g.End() {
				return true
			}
		}
		return false
	}
	credit := func(call *ast.CallExpr, isDefer bool) {
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name == fd.Name.Name {
			return
		}
		helper := funcs[id.Name]
		if helper == nil || helper.Body == nil || helper.Type.Params == nil {
			return
		}
		ha := analyseFunc(helper.Body)
		i := 0
		for _, field := range helper.Type.Params.List {
			for _, name := range field.Names {
				if i >= len(call.Args) {
					return
				}
				arg := argIdent(call.Args[i])
				i++
				if arg == "" || name.Name == "_" {
					continue
				}
				for _, kind := range []string{"waitgroup", "channel"} {
					if !ha.waitsOnAfter(joinPoint{name: name.Name, kind: kind}, token.NoPos) {
						continue
					}
					jp := joinPoint{name: arg, kind: kind, pos: call.Pos()}
					if isDefer {
						deferred = append(deferred, jp)
					} else {
						direct = append(direct, jp)
					}
				}
			}
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if d, ok := n.(*ast.DeferStmt); ok {
			if !inGoStmt(d.Pos()) {
				credit(d.Call, true)
			}
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && !inGoStmt(call.Pos()) {
			credit(call, false)
		}
		return true
	})
	return direct, deferred
}

// scanRelayGoroutines reports every `go` statement in the package that runs an
// outbox relay, marked joined or not by the frame that started it. It is the one
// implementation: the estate scan and the fixture self-check below both go
// through it.
func (p *relayPkg) scanRelayGoroutines(fset *token.FileSet) (handles int, out []relayGoroutine) {
	h := map[string]bool{}
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range p.files {
		for name := range relayHandles(f) {
			h[name] = true
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	handles = len(h)
	if handles == 0 {
		return 0, nil
	}
	rels := make([]string, 0, len(p.files))
	for rel := range p.files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		for _, decl := range p.files[rel].Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			a := analyseFunc(fd.Body)
			deferred := deferredWaits(fd.Body)
			named, namedDeferred := p.namedWaits(fd, a, funcs)
			a.direct = append(a.direct, named...)
			deferred = append(deferred, namedDeferred...)
			for _, g := range a.goStmts {
				handle := runsRelay(g, h)
				if handle == "" {
					continue
				}
				joined, signals := frameJoins(a, deferred, g)
				out = append(out, relayGoroutine{
					site:    fmt.Sprintf("%s:%d", rel, fset.Position(g.Pos()).Line),
					frame:   fd.Name.Name,
					handle:  handle,
					joined:  joined,
					signals: signals,
				})
			}
		}
	}
	return handles, out
}

// THE MACHINERY DISCRIMINATES — checked against a fixture, not assumed.
//
// A detector that answered "joined" to everything would make the estate scan
// below pass over any code at all, and a count floor cannot tell that apart from
// a healthy estate. The fixture holds one of each shape, INCLUDING the one this
// guard has never seen in the estate (a relay taken off an owner and left
// unjoined, which is what datamaster looked like), so a green here is evidence
// the scan would catch a new member rather than only the members it was written
// against.
func TestTheRelayJoinAnalysisSeparatesJoinedFromUnjoined(t *testing.T) {
	const fixtureMain = `package fixture

import (
	"context"
	"sync"

	"github.com/eighred/kanz/internal/outbox"
)

type svc struct{}

func (s svc) Outbox() *outbox.Relay { return nil }

func runBuiltInPlaceJoined(ctx context.Context) {
	builtJoined, _ := outbox.NewRelay(nil, nil, nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = builtJoined.Run(ctx) }()
	wg.Wait()
}

func runTakenOffAnOwnerJoined(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	takenJoined := svc{}.Outbox()
	wg.Add(1)
	go func() { defer wg.Done(); _ = takenJoined.Run(ctx) }()
}

func runJoinedByANamedHelperInAnotherFile(ctx context.Context) {
	namedJoined, _ := outbox.NewRelay(nil, nil, nil)
	var loops sync.WaitGroup
	defer joinThenClose(&loops)
	loops.Add(1)
	go func() { defer loops.Done(); _ = namedJoined.Run(ctx) }()
}

func runBuiltInPlaceUnjoined(ctx context.Context) {
	builtUnjoined, _ := outbox.NewRelay(nil, nil, nil)
	go func() { _ = builtUnjoined.Run(ctx) }()
}

func runTakenOffAnOwnerUnjoined(ctx context.Context) {
	takenUnjoined := svc{}.Outbox()
	go func() { _ = takenUnjoined.Run(ctx) }()
}

func runSomethingElseUnjoined(ctx context.Context) {
	projector := newProjector()
	go projector.Run(ctx)
}

func newProjector() interface{ Run(context.Context) } { return nil }
`
	// A SECOND FILE, because the named-join shape this must read spans two:
	// datamaster's run() is in main.go and joinThenClose is in loops.go. A
	// fixture in one file would prove the package-scoped analysis works without
	// ever exercising the reason it is package-scoped.
	const fixtureHelper = `package fixture

import "sync"

func joinThenClose(loops *sync.WaitGroup) { loops.Wait() }
`
	fset := token.NewFileSet()
	pkg := &relayPkg{dir: "fixture", files: map[string]*ast.File{}}
	for name, src := range map[string]string{"fixture/main.go": fixtureMain, "fixture/loops.go": fixtureHelper} {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		pkg.files[name] = f
	}
	handles, found := pkg.scanRelayGoroutines(fset)
	if handles < 5 {
		t.Fatalf("relayHandles found %d relay identifier(s) in the fixture, want 5 — the binding "+
			"recogniser is broken, and with it the estate scan sees no relays at all", handles)
	}
	if len(found) != 5 {
		var got []string
		for _, g := range found {
			got = append(got, g.frame)
		}
		sort.Strings(got)
		t.Fatalf("scanRelayGoroutines found %d relay goroutine(s) in the fixture (%v), want 5. "+
			"runSomethingElseUnjoined starts a goroutine that is NOT a relay and must not be "+
			"counted; the other five must all be", len(found), got)
	}
	var unjoined []string
	for _, g := range found {
		if !g.joined {
			unjoined = append(unjoined, g.frame)
		}
	}
	sort.Strings(unjoined)
	want := []string{"runBuiltInPlaceUnjoined", "runTakenOffAnOwnerUnjoined"}
	if len(unjoined) != len(want) || unjoined[0] != want[0] || unjoined[1] != want[1] {
		t.Fatalf("the analysis reports %v as unjoined, want exactly %v. The other three are real "+
			"joins (inline wg.Wait, defer wg.Wait, and a deferred same-package helper that waits on "+
			"the WaitGroup it is handed), so this is the detector answering the same way for "+
			"everything — with which the estate scan below proves nothing", unjoined, want)
	}
}

// TestEveryOutboxRelayGoroutineIsJoinedByItsFrame is the estate scan.
func TestEveryOutboxRelayGoroutineIsJoinedByItsFrame(t *testing.T) {
	root := moduleRoot(t)

	var (
		packages   int
		goroutines []relayGoroutine
	)
	for _, sub := range relayJoinScanDirs {
		fset := token.NewFileSet()
		pkgs := map[string]*relayPkg{}
		var order []string
		walkGoFiles(t, root, sub, fset, func(rel string, f *ast.File) {
			dir := path.Dir(rel)
			p := pkgs[dir]
			if p == nil {
				p = &relayPkg{dir: dir, files: map[string]*ast.File{}}
				pkgs[dir] = p
				order = append(order, dir)
			}
			p.files[rel] = f
		})
		sort.Strings(order)
		for _, dir := range order {
			n, found := pkgs[dir].scanRelayGoroutines(fset)
			if n > 0 {
				packages++
			}
			goroutines = append(goroutines, found...)
		}
	}

	// NON-VACUITY 1: the module has relay handles in several packages — the three
	// composition roots that run one, plus order.NewService, the position
	// projector and accounting's folder, which build one. A scan finding none
	// means relayHandles stopped recognising the bindings, and every arm below
	// would run over nothing.
	if packages < 4 {
		t.Fatalf("found relay handles in only %d package(s) across %v — this module has more, so "+
			"the join check below would run over an unrepresentative sample. The binding "+
			"recogniser in relayHandles is the usual suspect", packages, relayJoinScanDirs)
	}

	// NON-VACUITY 2: three composition roots run a relay in a goroutine — the
	// OMS, accounting and datamaster. Zero would mean the `go` statement walk or
	// the Run match stopped resolving them, and the guard would report a clean
	// estate having examined nothing.
	if len(goroutines) < 3 {
		t.Fatalf("found only %d relay goroutine(s) in the module. Three composition roots run "+
			"one — services/oms, services/accounting and services/datamaster — so the `go` "+
			"statement walk or the .Run() match has stopped working, and with it broken this "+
			"guard reports every relay as joined", len(goroutines))
	}

	// live is the set of sites that are STILL UNJOINED, not merely still present:
	// keying on presence would let an exemption outlive its repair.
	var unjoined []relayGoroutine
	live := map[string]bool{}
	for _, g := range goroutines {
		if g.joined {
			continue
		}
		live[g.site] = true
		if _, ok := unjoinedOutboxRelays[g.site]; ok {
			continue
		}
		unjoined = append(unjoined, g)
	}

	var dead []string
	for site := range unjoinedOutboxRelays {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("unjoinedOutboxRelays names %d site(s) that are no longer unjoined relay "+
			"goroutines: %s\n\nEither the statement moved (update the file:line key) or it was "+
			"joined/removed (delete the entry). A stale exemption cannot outlive the thing it "+
			"excused.", len(dead), strings.Join(dead, ", "))
	}

	if len(unjoined) == 0 {
		return
	}
	sort.Slice(unjoined, func(i, j int) bool { return unjoined[i].site < unjoined[j].site })
	var lines []string
	for _, g := range unjoined {
		detail := "it raises no completion signal at all (no wg.Done(), no close(ch), no ch <- v)"
		if len(g.signals) > 0 {
			var names []string
			for _, s := range g.signals {
				names = append(names, fmt.Sprintf("%s (%s)", s.name, s.kind))
			}
			sort.Strings(names)
			detail = "it signals " + strings.Join(names, ", ") + " but " + g.frame + " never waits on that"
		}
		lines = append(lines, fmt.Sprintf("%s (%s runs %s.Run): %s", g.site, g.frame, g.handle, detail))
	}
	t.Fatalf("%d outbox relay goroutine(s) are not joined by the frame that started them:\n  %s\n\n"+
		"The frame that starts a relay also defers the close of the pool it drains through, and "+
		"every composition root here is os.Exit(run()) — so an unjoined relay does not wind down "+
		"when the frame returns, it stops existing wherever it is. The relay publishes a FACT to "+
		"the broker and THEN marks the record published; stopping between those two leaves the "+
		"estate holding an announcement the outbox table still calls pending, and the next "+
		"process republishes it (#815).\n\n"+
		"Fix it in the frame, not in the caller: sync.WaitGroup, wg.Add(1) before the `go`, "+
		"defer wg.Done() inside it, and a Wait that runs before the deferred store closers — "+
		"services/oms/cmd/oms/main.go and services/datamaster/cmd/datamaster/loops.go are the "+
		"two shapes. If a relay genuinely cannot be joined, add its site to unjoinedOutboxRelays "+
		"with the issue that retires it and the structural argument for why it cannot be holding "+
		"the store at teardown.",
		len(unjoined), strings.Join(lines, "\n  "))
}

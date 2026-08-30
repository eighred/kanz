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

// A BUS CONSUMER GOROUTINE LAUNCHED IN main() MUST BE JOINED BEFORE main RETURNS.
//
// pkg/bus does not stop a consumer when its context is cancelled — it DRAINS it.
// Subscribe deliberately spends up to DrainGrace + drainStopSlack (5s + 2s, see
// pkg/bus/nats.go) AFTER ctx.Done() handing already-buffered messages to the
// handler, because Stop() would abandon them and reorder the replayed log on
// every rolling deploy. The drain is the whole point.
//
// So a composition root that launches its consumer with a bare `go func(){…}()`
// and returns from main on `<-ctx.Done()` is not shutting down — it is running
// its LIFO defers (closeStore/pool.Close, the bus client Close, mesh.Close)
// UNDERNEATH a handler that is still folding events. That is not a lost
// shutdown log line; it is silent data loss on the capital path:
//
//   - accounting, wealth and alternatives each own a book of record. On SIGTERM
//     the drained fill hits ledger.Store.Append against a pool that main has
//     already closed. bus.WithDLQ is wired on all three, so the fold failure
//     routes the event to dlq.<subject> instead of the journal — and a DLQ'd
//     message is ACKED, so the broker considers it delivered.
//   - tools/replay refuses `dlq.` subjects, so those events are not recoverable
//     with the tooling this estate ships. The loss compounds across deploys.
//
// #233 fixed the three. This guard is what stops the fourth.
//
// SCOPE AND LIMITS, stated plainly so nobody reads more assurance into a green
// run than it carries:
//
//   - Only `go` statements lexically inside the COMPOSITION-ROOT FRAME are
//     checked. A goroutine launched by a helper is that helper's problem to
//     join, and the specific failure this guard exists to prevent — deferred
//     closers firing under a draining handler — is a property of the one frame
//     that registers them.
//
//     That frame is no longer `func main`. #266 moved every service's lifecycle
//     into a `run() int` so the process can exit non-zero WITHOUT skipping those
//     same deferred closers (os.Exit does skip them), leaving main as nothing but
//     `os.Exit(run())`. The defers, the goroutines and the joins all moved
//     together, so the invariant is unchanged — but a guard still reading
//     `func main` would have found an empty body and passed on every service at
//     once. See entryDecl below: the frame is located by its signal.NotifyContext
//     call, which is what makes it the frame whose defers unwind on SIGTERM,
//     whatever it ends up being named.
//   - A goroutine counts as a CONSUMER goroutine when its body reaches a
//     bus.NewConsumer call, directly or through a function declared in the same
//     main package. Reachability is syntactic: a consumer reached through a
//     method value or an interface will not be recognised. That direction fails
//     OPEN, which is why the non-vacuity floors below assert the analysis still
//     finds the consumers it is supposed to find.
//   - "Joined" means main waits, after the `go` statement, on the same
//     WaitGroup or channel the goroutine signals — either in main's own body or
//     in a same-package helper main calls and passes it to. Both happen inside
//     main's frame and therefore BEFORE the deferred closers run, which is
//     precisely the ordering at stake. The guard does not prove the wait is on
//     an unconditional path — an `if` around it would still pass.
//   - Runtime proof needs a cluster and a SIGTERM. This is a code assertion
//     about shutdown ordering, and nothing more.

// mainPkg is one `package main` composition root, parsed whole (the package,
// not one file: services/risk-engine/cmd/risk-engine spreads across several,
// and a helper that reaches bus.NewConsumer may live in any of them).
type mainPkg struct {
	dir      string // module-relative, e.g. "services/accounting/cmd/accounting"
	fset     *token.FileSet
	files    []*ast.File
	funcs    map[string]*ast.FuncDecl
	mainDecl *ast.FuncDecl
	// entryDecl is the COMPOSITION-ROOT FRAME: the function that installs the
	// signal context and therefore owns the defer chain that unwinds on SIGTERM.
	// Since #266 that is `run()`, not `main` — main is only `os.Exit(run())` — and
	// in the venue adapters it is `serve()`, one level deeper still. Locating it
	// by its signal.NotifyContext call rather than by name is what keeps this
	// guard pointed at the right frame across those renames.
	//
	// Falls back to mainDecl when no signal context is found, so a binary that
	// installs none is still analysed rather than silently skipped.
	entryDecl       *ast.FuncDecl
	hasConsumerCall bool
}

func (p *mainPkg) where(pos token.Pos, root string) string {
	position := p.fset.Position(pos)
	rel, err := filepath.Rel(root, position.Filename)
	if err != nil {
		rel = position.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), position.Line)
}

// mainPackages parses every services/*/cmd/* and cmd/* directory that declares
// a `func main`. Build constraints are ignored on purpose: a consumer goroutine
// behind a build tag is still a consumer goroutine, and skipping tagged files
// would let one hide.
func mainPackages(t *testing.T, root string) []*mainPkg {
	t.Helper()
	var dirs []string
	for _, pat := range []string{
		filepath.Join(root, "services", "*", "cmd", "*"),
		filepath.Join(root, "cmd", "*"),
	} {
		matches, err := filepath.Glob(pat)
		if err != nil {
			t.Fatalf("glob %s: %v", pat, err)
		}
		dirs = append(dirs, matches...)
	}
	sort.Strings(dirs)

	var out []*mainPkg
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		goFiles, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s/*.go: %v", dir, err)
		}
		p := &mainPkg{fset: token.NewFileSet(), funcs: map[string]*ast.FuncDecl{}}
		for _, gf := range goFiles {
			if strings.HasSuffix(gf, "_test.go") {
				continue
			}
			f, perr := parser.ParseFile(p.fset, gf, nil, parser.SkipObjectResolution)
			if perr != nil {
				t.Fatalf("parse %s: %v", gf, perr)
			}
			if f.Name.Name != "main" {
				continue
			}
			p.files = append(p.files, f)
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || fd.Body == nil {
					continue
				}
				p.funcs[fd.Name.Name] = fd
				if fd.Name.Name == "main" {
					p.mainDecl = fd
				}
			}
		}
		if p.mainDecl == nil {
			continue
		}
		rel, rerr := filepath.Rel(root, dir)
		if rerr != nil {
			rel = dir
		}
		p.dir = filepath.ToSlash(rel)
		for _, f := range p.files {
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isSelector(call.Fun, "bus", "NewConsumer") {
					p.hasConsumerCall = true
				}
				return true
			})
		}
		p.entryDecl = p.resolveEntry()
		out = append(out, p)
	}
	return out
}

// resolveEntry finds the composition-root frame: the same-package function whose
// body installs the signal context (signal.NotifyContext). That call is what
// makes a frame the one whose defers unwind on SIGTERM, which is exactly the
// ordering this guard is about — so it identifies the frame far more robustly
// than the name `main` did, and survived #266 renaming it to run()/serve().
//
// When more than one function matches, the OUTERMOST by source position wins;
// when none does, main stands in.
func (p *mainPkg) resolveEntry() *ast.FuncDecl {
	var best *ast.FuncDecl
	for _, fd := range p.funcs {
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isSelector(call.Fun, "signal", "NotifyContext") {
				found = true
			}
			return !found
		})
		if found && (best == nil || fd.Pos() < best.Pos()) {
			best = fd
		}
	}
	if best == nil {
		return p.mainDecl
	}
	return best
}

// reachesNewConsumer reports whether n contains a bus.NewConsumer call, either
// directly or through a function declared in the same main package. seen guards
// against recursion.
func (p *mainPkg) reachesNewConsumer(n ast.Node, seen map[string]bool) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isSelector(call.Fun, "bus", "NewConsumer") {
			found = true
			return false
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || seen[id.Name] {
			return true
		}
		fd, ok := p.funcs[id.Name]
		if !ok {
			return true
		}
		seen[id.Name] = true
		if p.reachesNewConsumer(fd.Body, seen) {
			found = true
			return false
		}
		return true
	})
	return found
}

// joinPoint is one end of a join: a completion signal raised by a goroutine, or
// a wait for one. kind is "waitgroup" or "channel"; the two never match each
// other, so `close(done)` cannot be satisfied by a stray `wg.Wait()`.
type joinPoint struct {
	name string
	kind string
	pos  token.Pos
}

// completionSignals returns the ways n announces that it has finished:
// `wg.Done()`, `close(ch)`, or `ch <- v`. A `ctx.Done()` receive is not a
// completion signal and is filtered out — it is the shutdown trigger, and
// leaving it in would only produce a confusing failure message (it can never
// match a wait, so it could never produce a false pass).
//
// The filter is on the SHAPE of the name, not the exact word `ctx`: a derived
// context is routinely called runCtx or shutdownCtx, and #813's guard reported
// pkg/bus/redrive.go's watchdog as "it signals runCtx (waitgroup)" — a true
// verdict with a detail line that named a context as a WaitGroup and would have
// sent the next reader looking for one.
func completionSignals(n ast.Node) []joinPoint {
	var out []joinPoint
	ast.Inspect(n, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Done" {
				if id, ok := sel.X.(*ast.Ident); ok && !isContextIdent(id.Name) {
					out = append(out, joinPoint{name: id.Name, kind: "waitgroup", pos: v.Pos()})
				}
			}
			if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "close" && len(v.Args) == 1 {
				if arg, ok := v.Args[0].(*ast.Ident); ok {
					out = append(out, joinPoint{name: arg.Name, kind: "channel", pos: v.Pos()})
				}
			}
		case *ast.SendStmt:
			if id, ok := v.Chan.(*ast.Ident); ok {
				out = append(out, joinPoint{name: id.Name, kind: "channel", pos: v.Pos()})
			}
		}
		return true
	})
	return out
}

// isContextIdent reports whether name is a context variable — `ctx` itself or
// any of the derived spellings this module uses (runCtx, shutdownCtx, baseCtx).
// Its only effect is on which Done() calls are reported as completion signals;
// a context has no Wait method, so widening it cannot create a false pass.
func isContextIdent(name string) bool {
	return name == "ctx" || strings.HasSuffix(name, "Ctx") || strings.HasSuffix(name, "Context")
}

// waitPoints returns the ways n blocks on someone else finishing: `wg.Wait()`
// or a receive `<-ch` on a plain identifier. `<-ctx.Done()` receives from a
// call expression, not an identifier, so it is structurally excluded.
func waitPoints(n ast.Node) []joinPoint {
	var out []joinPoint
	ast.Inspect(n, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Wait" {
				if id, ok := sel.X.(*ast.Ident); ok {
					out = append(out, joinPoint{name: id.Name, kind: "waitgroup", pos: v.Pos()})
				}
			}
		case *ast.UnaryExpr:
			if v.Op == token.ARROW {
				if id, ok := v.X.(*ast.Ident); ok {
					out = append(out, joinPoint{name: id.Name, kind: "channel", pos: v.Pos()})
				}
			}
		}
		return true
	})
	return out
}

// joinAnalysis is one function's join machinery, split by whether a wait
// actually blocks that function's own goroutine.
type joinAnalysis struct {
	goStmts []*ast.GoStmt
	// direct waits run on the analysed function's goroutine, so they are
	// guaranteed to complete before it returns (and so, for main, before the
	// deferred closers).
	direct []joinPoint
	// waits parked inside a `go` statement do NOT block the analysed function by
	// themselves. They count only through the bounded-join idiom: a WaitGroup
	// cannot be waited on with a timeout, so the caller spawns
	// `go func(){ wg.Wait(); close(drained) }()` and selects on `drained`. Such a
	// wait is honoured only when the goroutine holding it signals a channel the
	// function directly waits on.
	inGoroutine map[*ast.GoStmt][]joinPoint
}

func analyseFunc(body *ast.BlockStmt) joinAnalysis {
	a := joinAnalysis{inGoroutine: map[*ast.GoStmt][]joinPoint{}}
	ast.Inspect(body, func(n ast.Node) bool {
		if g, ok := n.(*ast.GoStmt); ok {
			a.goStmts = append(a.goStmts, g)
		}
		return true
	})
	for _, w := range waitPoints(body) {
		owner := (*ast.GoStmt)(nil)
		for _, g := range a.goStmts {
			if w.pos > g.Pos() && w.pos < g.End() {
				owner = g
				break
			}
		}
		if owner == nil {
			a.direct = append(a.direct, w)
			continue
		}
		a.inGoroutine[owner] = append(a.inGoroutine[owner], w)
	}
	return a
}

// waitsOnAfter reports whether the analysed function blocks on signal s at some
// point after pos. Pass token.NoPos to ask "anywhere in this function".
func (a joinAnalysis) waitsOnAfter(s joinPoint, pos token.Pos) bool {
	if a.directlyWaitsAfter(s, pos) {
		return true
	}
	// One level of indirection: the bounded-join idiom described on
	// joinAnalysis.inGoroutine. The relaying goroutine's own channel signal must
	// itself be directly waited on, after pos.
	for g, waits := range a.inGoroutine {
		for _, w := range waits {
			if w.name != s.name || w.kind != s.kind {
				continue
			}
			for _, relay := range completionSignals(g) {
				if relay.kind == "channel" && a.directlyWaitsAfter(relay, pos) {
					return true
				}
			}
		}
	}
	return false
}

func (a joinAnalysis) directlyWaitsAfter(s joinPoint, pos token.Pos) bool {
	for _, w := range a.direct {
		if w.name == s.name && w.kind == s.kind && w.pos > pos {
			return true
		}
	}
	return false
}

// analyseMain analyses main and then folds in the waits performed by
// same-package helpers main calls — `awaitConsumers(&consumers, logger)` blocks
// main's goroutine exactly as an inline `consumers.Wait()` would, and a rule
// that only understood the inline spelling would push composition roots toward
// copying twelve lines of select instead of naming them.
//
// One level deep, deliberately: a helper that hands the WaitGroup to a second
// helper is no longer something this guard can read honestly, and it should
// fail rather than guess. A wait is credited to main only when the identifier
// waited on is one of the helper's PARAMETERS, remapped to the argument main
// passed — a helper waiting on its own local is waiting on its own goroutines,
// not main's.
func (p *mainPkg) analyseMain() joinAnalysis {
	a := analyseFunc(p.entryDecl.Body)

	for _, call := range p.helperCallsIn(a) {
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			continue
		}
		helper := p.funcs[id.Name]
		if helper == nil || helper.Type.Params == nil {
			continue
		}
		ha := analyseFunc(helper.Body)

		i := 0
		for _, field := range helper.Type.Params.List {
			for _, name := range field.Names {
				if i >= len(call.Args) {
					break
				}
				arg := argIdent(call.Args[i])
				i++
				if arg == "" || name.Name == "_" {
					continue
				}
				for _, kind := range []string{"waitgroup", "channel"} {
					if ha.waitsOnAfter(joinPoint{name: name.Name, kind: kind}, token.NoPos) {
						a.direct = append(a.direct, joinPoint{name: arg, kind: kind, pos: call.Pos()})
					}
				}
			}
		}
	}
	return a
}

// helperCallsIn returns the calls to same-package functions made on main's own
// goroutine. Calls parked inside a `go` statement are excluded: they do not
// block main, so whatever they wait on is not a join of main's.
func (p *mainPkg) helperCallsIn(a joinAnalysis) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(p.entryDecl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name == p.entryDecl.Name.Name || p.funcs[id.Name] == nil {
			return true
		}
		for _, g := range a.goStmts {
			if call.Pos() > g.Pos() && call.Pos() < g.End() {
				return true
			}
		}
		out = append(out, call)
		return true
	})
	return out
}

// argIdent names the variable behind a call argument, seeing through the `&x`
// a *sync.WaitGroup parameter is always passed as. Anything else yields "".
func argIdent(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			if id, ok := v.X.(*ast.Ident); ok {
				return id.Name
			}
		}
	}
	return ""
}

// unjoinedConsumerGoroutines is the default-deny allow-list of consumer
// goroutines in a main() that are permitted to go unjoined. Keyed on the
// "file:line" of the `go` statement, so a moved call site un-certifies itself
// and has to be re-reviewed — the same fail-closed keying bus_dlq_test.go uses.
//
// The bar for an entry is not "this service is not on the capital path". It is
// a structural argument that no handler this goroutine dispatches can touch a
// resource main's deferred closers tear down. Empty today: every consumer
// goroutine in the estate is joined, and #233 is what made that true.
var unjoinedConsumerGoroutines = map[string]string{}

func TestEveryConsumerGoroutineInMainIsJoined(t *testing.T) {
	root := moduleRoot(t)
	pkgs := mainPackages(t, root)

	// NON-VACUITY 1: this module has 26 services and 12 CLI binaries. A scan that
	// finds a handful means the glob or the `func main` filter broke, and every
	// arm below would then pass over almost nothing.
	if len(pkgs) < 20 {
		t.Fatalf("found only %d main packages under services/*/cmd/* and cmd/* — this module has "+
			"far more, so the join check below would run over an unrepresentative sample", len(pkgs))
	}

	// NON-VACUITY 2: the whole guard rests on recognising bus.NewConsumer through
	// the AST. If that stops matching (the package is aliased, the constructor is
	// renamed) every goroutine silently becomes "not a consumer goroutine" and
	// the guard passes an estate that wires nothing correctly.
	withConsumer := 0
	for _, p := range pkgs {
		if p.hasConsumerCall {
			withConsumer++
		}
	}
	if withConsumer < 10 {
		t.Fatalf("only %d of %d main packages appear to call bus.NewConsumer. At least ten do "+
			"(accounting, alternatives, audit, autopilot, compliance, lake-sink, lineage, market-data, "+
			"oms, risk-engine, tv-sync, wealth, webhook-ingest). The AST match for bus.NewConsumer has "+
			"stopped working — fix the scanner, because with it broken this guard reports every "+
			"goroutine as harmless.", withConsumer, len(pkgs))
	}

	type finding struct {
		site    string
		pkg     string
		signals []joinPoint
	}
	var (
		consumerGoroutines []string
		unjoined           []finding
	)

	for _, p := range pkgs {
		if !p.hasConsumerCall {
			continue
		}
		analysis := p.analyseMain()
		for _, g := range analysis.goStmts {
			if !p.reachesNewConsumer(g, map[string]bool{p.entryDecl.Name.Name: true}) {
				continue
			}
			site := p.where(g.Pos(), root)
			consumerGoroutines = append(consumerGoroutines, site)

			signals := completionSignals(g)
			joined := false
			for _, s := range signals {
				if analysis.waitsOnAfter(s, g.Pos()) {
					joined = true
					break
				}
			}
			if joined {
				continue
			}
			if _, ok := unjoinedConsumerGoroutines[site]; ok {
				continue
			}
			unjoined = append(unjoined, finding{site: site, pkg: p.dir, signals: signals})
		}
	}

	// NON-VACUITY 3: the estate does launch consumer goroutines from main. Zero
	// means either the reachability walk stopped resolving them or the shape
	// moved wholesale (e.g. the risk-engine app.Lifecycle promotion) — in both
	// cases the invariant now lives somewhere this guard is not looking.
	if len(consumerGoroutines) == 0 {
		t.Fatal("found zero bus-consumer goroutines launched from a main() — either the reachability " +
			"walk stopped resolving them (fix the scanner), or every composition root moved its " +
			"consumer behind a shared lifecycle helper. If it is the latter, this guard must follow " +
			"the invariant to its new home rather than sit here passing on an empty set.")
	}

	// DEAD-ENTRY CHECK: an exemption naming a `go` statement that no longer
	// exists reads as a reviewed decision while protecting nothing.
	live := map[string]bool{}
	for _, site := range consumerGoroutines {
		live[site] = true
	}
	var dead []string
	for site := range unjoinedConsumerGoroutines {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("unjoinedConsumerGoroutines names %d `go` statement(s) that are no longer "+
			"unjoined consumer goroutines: %s\n\n"+
			"Either the statement moved (update the file:line key) or it was joined/removed "+
			"(delete the entry). A stale exemption cannot outlive the thing it excused.",
			len(dead), strings.Join(dead, ", "))
	}

	if len(unjoined) == 0 {
		return
	}
	sort.Slice(unjoined, func(i, j int) bool { return unjoined[i].site < unjoined[j].site })
	var lines []string
	for _, f := range unjoined {
		detail := "it raises no completion signal at all (no wg.Done(), no close(ch), no ch <- v)"
		if len(f.signals) > 0 {
			var names []string
			for _, s := range f.signals {
				names = append(names, fmt.Sprintf("%s (%s)", s.name, s.kind))
			}
			sort.Strings(names)
			detail = "it signals " + strings.Join(names, ", ") +
				" but main never waits on that after the `go` statement"
		}
		lines = append(lines, fmt.Sprintf("%s (%s): %s", f.site, f.pkg, detail))
	}
	t.Fatalf("%d bus-consumer goroutine(s) launched from main() are never joined:\n  %s\n\n"+
		"pkg/bus DRAINS on shutdown rather than stopping: Subscribe keeps handing buffered "+
		"messages to the handler for up to DrainGrace + drainStopSlack (5s + 2s, pkg/bus/nats.go) "+
		"AFTER ctx is cancelled, deliberately, so a rolling deploy does not abandon them. main "+
		"returning on <-ctx.Done() therefore runs its deferred closers — pool.Close, the bus "+
		"client Close, mesh.Close — UNDERNEATH a handler that is still folding events. On the "+
		"three book-of-record services that is a fill hitting a closed pool, routed to "+
		"dlq.<subject> by bus.WithDLQ and acked, and tools/replay refuses dlq. subjects.\n\n"+
		"Fix it in the composition root: give the goroutine a completion signal (a sync.WaitGroup "+
		"it Done()s, or a channel it closes) and wait on it on MAIN'S OWN GOROUTINE after the "+
		"`go` statement — inline, or in a same-package helper main passes it to — bounded, so a "+
		"handler that ignores context cancellation cannot hold the pod past its "+
		"terminationGracePeriodSeconds. See "+
		"services/accounting/cmd/accounting/main.go for the shape and "+
		"services/risk-engine/internal/app/lifecycle.go for the ordering it follows.",
		len(unjoined), strings.Join(lines, "\n  "))
}

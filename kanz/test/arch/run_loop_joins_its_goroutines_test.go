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

// A Run(ctx) LOOP MUST JOIN EVERY GOROUTINE IT STARTS.
//
// consumer_goroutine_join_test.go states its own scope plainly: it checks the
// COMPOSITION-ROOT FRAME only, on the reasoning that "a goroutine launched by a
// helper is that helper's problem to join". This guard is that other half — the
// helper's side of the same invariant — and #813 is why it exists rather than
// remaining a sentence.
//
// WHAT WENT WRONG. internal/marketedge/ingest's Engine.Run started a fold loop
// and a snapshot loop and returned on the fold loop's first send:
//
//	go func() { errc <- e.foldLoop(ctx) }()
//	go e.snapshotLoop(ctx)
//	return <-errc
//
// The snapshot loop holds a ticker that PUBLISHES TO THE BUS, and it was outside
// every WaitGroup in the chain above it: pkg/alpha's Runner joins Engine.Run,
// market-ingest joins the Runner, and then main's LIFO defers close the bus
// client. Every one of those joins completed while a snapshot was still inside
// Publish. publishSnapshot logs its error and returns, so the loser of that race
// is a market-data snapshot that disappears with a single Warn line, at the
// least observed moment of a deploy.
//
// The rescue at the time lived in the CALLER's cancel(), which is the actual
// defect: a function whose contract is "runs until ctx is cancelled, then
// returns" is correct only under callers that remembered. Lifetime belongs to
// the frame that starts the goroutine.
//
// SCOPE AND LIMITS, so a green run is not read for more than it carries:
//
//   - A "Run loop" is a function whose name begins with Run and whose FIRST
//     parameter is a context.Context. That is this module's convention for a
//     blocking loop a caller joins — 32 such functions exist across internal/,
//     pkg/, services/, cmd/ and tools/ — and the set is derived from the AST on
//     every run, never listed here.
//   - A Start()/Stop() pair is deliberately NOT in scope. Its goroutine is joined
//     by a sibling method through a struct field, which is a different (and
//     legitimate) shape this guard cannot read honestly, and pretending otherwise
//     would mean an exemption map of a dozen entries that all say "this is fine".
//   - "Joined" means the Run loop itself blocks on a completion signal the
//     goroutine raises: a wg.Wait() or a channel receive placed after the `go`
//     statement, or either one under a `defer` (a deferred wait runs on every
//     return path, so its source position carries no meaning). The guard does not
//     prove the wait is reached before a resource is torn down, and it does not
//     prove the goroutine ever terminates.
//   - Comments are NOT parsed (no parser.ParseComments), so nothing here can be
//     satisfied by prose that describes a join. Only `go`, `wg.Wait`, `close` and
//     channel operations are read.
//   - Runtime proof needs a cluster and a SIGTERM. This is a code assertion about
//     one function's return, and nothing more.

// runLoopScanDirs are the module subtrees walked. cmd/ and services/*/cmd/* are
// included even though composition roots are the other guard's subject: a Run
// loop declared in a main package is still a Run loop, and excluding it would
// leave a gap between the two guards rather than a seam.
var runLoopScanDirs = []string{"internal", "pkg", "services", "cmd", "tools"}

// unjoinedRunLoopGoroutines is the default-deny allow-list of `go` statements a
// Run loop may leave unjoined, keyed "file:line" so a moved call site
// un-certifies itself — the same fail-closed keying bus_dlq_test.go uses.
//
// The bar for an entry is a structural argument that the goroutine cannot touch
// anything the caller tears down after the join it escapes. "It is only a
// watchdog" is not that argument unless the code shows it.
//
// Empty today. #813 joined the ingest engine's snapshot loop and the redrive
// idle watchdog, which were the only two in the estate.
var unjoinedRunLoopGoroutines = map[string]string{}

// runGoroutine is one `go` statement found inside a Run loop.
type runGoroutine struct {
	site     string // module-relative "file:line"
	function string
	joined   bool
	signals  []joinPoint
}

// isRunLoop reports whether fd is a Run-shaped blocking loop: name begins with
// Run, first parameter is a context.Context.
func isRunLoop(fd *ast.FuncDecl) bool {
	if fd.Body == nil || !strings.HasPrefix(fd.Name.Name, "Run") {
		return false
	}
	if fd.Type.Params == nil || len(fd.Type.Params.List) == 0 {
		return false
	}
	return isSelector(fd.Type.Params.List[0].Type, "context", "Context")
}

// deferredWaits returns the waits n performs under a `defer`. A deferred wait
// runs on EVERY return path, so unlike an inline one its position relative to
// the `go` statement says nothing — and `defer wg.Wait()` registered before the
// goroutine starts is the correct spelling when a function has several returns.
func deferredWaits(n ast.Node) []joinPoint {
	var out []joinPoint
	ast.Inspect(n, func(node ast.Node) bool {
		d, ok := node.(*ast.DeferStmt)
		if !ok {
			return true
		}
		out = append(out, waitPoints(d)...)
		return false
	})
	return out
}

// frameJoins reports whether the frame that started g blocks on g finishing, and
// returns the completion signals g raises so a failure can say what it signalled
// instead of only that nothing waited.
//
// ONE IMPLEMENTATION, TWO SCOPES. This guard asks it about `go` statements in a
// Run(ctx) loop; outbox_relay_join_test.go asks it about the goroutine a
// composition root runs an outbox relay in. The rule is the same one — the frame
// that starts a goroutine is the frame that joins it — and it is written once so
// that widening what counts as a join widens both, rather than one of them.
//
// "Joined" means the frame blocks on a signal the goroutine raises: a wg.Wait()
// or channel receive placed AFTER the `go` statement, or either under a `defer`
// (a deferred wait runs on every return path, so its source position carries no
// meaning). It does not prove the wait is reached before a resource is torn
// down, and it does not prove the goroutine terminates.
func frameJoins(a joinAnalysis, deferred []joinPoint, g *ast.GoStmt) (bool, []joinPoint) {
	signals := completionSignals(g)
	for _, s := range signals {
		if a.waitsOnAfter(s, g.Pos()) {
			return true, signals
		}
		for _, d := range deferred {
			if d.name == s.name && d.kind == s.kind {
				return true, signals
			}
		}
	}
	return false, signals
}

// scanRunLoops reports every `go` statement inside every Run loop declared in f,
// marked joined or not. This is the one implementation: the estate scan and the
// fixture self-check below both go through it, so a self-check that passes is
// evidence about the machinery the estate scan actually uses.
func scanRunLoops(fset *token.FileSet, rel string, f *ast.File) (loops int, out []runGoroutine) {
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || !isRunLoop(fd) {
			continue
		}
		loops++
		a := analyseFunc(fd.Body)
		deferred := deferredWaits(fd.Body)
		for _, g := range a.goStmts {
			joined, signals := frameJoins(a, deferred, g)
			out = append(out, runGoroutine{
				site:     fmt.Sprintf("%s:%d", rel, fset.Position(g.Pos()).Line),
				function: fd.Name.Name,
				joined:   joined,
				signals:  signals,
			})
		}
	}
	return loops, out
}

// THE MACHINERY DISCRIMINATES — checked against a fixture, not assumed.
//
// A join detector that answered "joined" to everything would make the estate
// scan below pass over any code at all, and a count floor cannot tell that apart
// from a healthy estate. This parses a fixture holding one of each shape and
// asserts the analysis separates them, so both failure directions are visible:
// answer-always-joined loses RunUnjoined, answer-always-unjoined gains the other
// three.
func TestRunLoopJoinAnalysisSeparatesJoinedFromUnjoined(t *testing.T) {
	const fixture = `package fixture

import (
	"context"
	"sync"
)

func work(context.Context) {}

func RunWaitGroupJoined(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); work(ctx) }()
	wg.Wait()
	return nil
}

func RunDeferJoined(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() { defer wg.Done(); work(ctx) }()
	return nil
}

func RunChannelJoined(ctx context.Context) error {
	done := make(chan struct{})
	go func() { defer close(done); work(ctx) }()
	<-done
	return nil
}

func RunUnjoined(ctx context.Context) error {
	go work(ctx)
	return nil
}

func Serve(ctx context.Context) { go work(ctx) }

func RunWithoutContext() { go work(context.Background()) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", fixture, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	loops, found := scanRunLoops(fset, "fixture.go", f)

	// Serve is not Run-shaped and RunWithoutContext takes no context, so neither
	// is a Run loop. Getting this wrong in the other direction would silently
	// widen the estate scan onto Start()/Stop() pairs.
	if loops != 4 {
		t.Fatalf("scanRunLoops found %d Run loops in the fixture, want 4 (Serve and "+
			"RunWithoutContext are deliberately not Run-shaped)", loops)
	}
	if len(found) != 4 {
		t.Fatalf("scanRunLoops found %d `go` statements in the fixture, want 4", len(found))
	}
	var unjoined []string
	for _, g := range found {
		if !g.joined {
			unjoined = append(unjoined, g.function)
		}
	}
	sort.Strings(unjoined)
	if len(unjoined) != 1 || unjoined[0] != "RunUnjoined" {
		t.Fatalf("the analysis reports %v as unjoined, want exactly [RunUnjoined]. "+
			"Every other shape in the fixture is a real join (inline wg.Wait, defer wg.Wait, "+
			"channel receive), so this is the join detector answering the same way for "+
			"everything — with which the estate scan below proves nothing.", unjoined)
	}
}

func TestEveryRunLoopJoinsTheGoroutinesItStarts(t *testing.T) {
	root := moduleRoot(t)

	var (
		loops      int
		goroutines []runGoroutine
	)
	for _, sub := range runLoopScanDirs {
		fset := token.NewFileSet()
		walkGoFiles(t, root, sub, fset, func(rel string, f *ast.File) {
			n, found := scanRunLoops(fset, rel, f)
			loops += n
			goroutines = append(goroutines, found...)
		})
	}

	// NON-VACUITY 1: 32 Run-shaped functions exist across the five subtrees. A
	// scan finding a handful means the name match or the context.Context
	// parameter match broke, and every arm below would run over almost nothing.
	if loops < 25 {
		t.Fatalf("found only %d Run(ctx) loops across %v — this module has far more, so the "+
			"join check below would run over an unrepresentative sample. The scanner's "+
			"context.Context parameter match is the usual suspect.", loops, runLoopScanDirs)
	}

	// NON-VACUITY 2: seven of those Run loops start goroutines. Zero would mean
	// the `go` statement walk stopped resolving them, and the guard would report
	// a clean estate having examined nothing.
	if len(goroutines) < 5 {
		t.Fatalf("found only %d `go` statement(s) inside %d Run(ctx) loops. At least ten exist "+
			"(pkg/alpha's Runner alone has four, internal/marketedge/ingest two). The `go` "+
			"statement walk has stopped working, and with it broken this guard reports every "+
			"Run loop as harmless.", len(goroutines), loops)
	}

	// live is the set of sites that are STILL UNJOINED, not merely still present.
	// Keying it on presence would let an exemption outlive its repair: the
	// goroutine gets joined, the entry stays, and the next unjoined goroutine to
	// land on that line inherits a waiver nobody granted it.
	var unjoined []runGoroutine
	live := map[string]bool{}
	for _, g := range goroutines {
		if g.joined {
			continue
		}
		live[g.site] = true
		if _, ok := unjoinedRunLoopGoroutines[g.site]; ok {
			continue
		}
		unjoined = append(unjoined, g)
	}

	// DEAD-ENTRY CHECK: an exemption naming a `go` statement that is no longer an
	// unjoined one in a Run loop reads as a reviewed decision while protecting
	// nothing.
	var dead []string
	for site := range unjoinedRunLoopGoroutines {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("unjoinedRunLoopGoroutines names %d `go` statement(s) that are no longer "+
			"unjoined goroutines in a Run(ctx) loop: %s\n\n"+
			"Either the statement moved (update the file:line key) or it was joined/removed "+
			"(delete the entry). A stale exemption cannot outlive the thing it excused.",
			len(dead), strings.Join(dead, ", "))
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
			detail = "it signals " + strings.Join(names, ", ") +
				" but the Run loop never waits on that"
		}
		lines = append(lines, fmt.Sprintf("%s (%s): %s", g.site, g.function, detail))
	}
	t.Fatalf("%d goroutine(s) started by a Run(ctx) loop are never joined by it:\n  %s\n\n"+
		"A Run(ctx) loop's contract is that it returns when its work is over, and every caller "+
		"in this module treats it that way: pkg/alpha's Runner joins Engine.Run under a "+
		"WaitGroup, market-ingest joins the Runner, and main's LIFO defers then close the bus "+
		"client and the pool. A goroutine outside that join keeps running past all of it — "+
		"#813 was a book-snapshot publish still inside the producer while the transport was "+
		"being closed underneath it, reported as one Warn line.\n\n"+
		"Fix it in the Run loop, not in the caller: derive a cancellable context, start the "+
		"goroutine under a sync.WaitGroup, cancel on the first return, and Wait before "+
		"returning. Relying on the caller's cancel() makes the function correct only under "+
		"callers that remembered. See internal/marketedge/ingest/engine.go's Run for the shape.",
		len(unjoined), strings.Join(lines, "\n  "))
}

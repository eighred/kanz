package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// EVERY REGISTERED METRIC MUST HAVE A NON-TEST WRITER.
//
// This is the sibling of TestEveryObservabilityMetricExistsInSource, from the other
// end. That one asks whether a metric NAMED IN A RULE exists in Go. This one asks
// whether a metric that exists in Go is ever actually SET — because a collector
// that is constructed and registered and never written exports the metric FAMILY
// with NO SERIES, and every downstream consumer reads that as zero:
//
//   - Prometheus holds no sample, so `metric > threshold` is an empty vector and
//     an alert over it can never fire.
//   - A Grafana panel renders empty, which a human opening it during an incident
//     reads as QUIET rather than BROKEN.
//   - A KEDA Prometheus trigger with the default ignoreNullValues gets 0 and no
//     error, so the ScaledObject sits at minReplicaCount UNDER ANY LOAD and looks
//     like a healthy idle system.
//
// #283 is all three at once, and it shipped in the same commit as the metric.
// kanz_bus_consumer_lag and kanz_bus_pending_messages were registered in
// pkg/bus/metrics.go with doc comments describing "a periodic poller" that was
// never written; two ScaledObjects (infra/deploy/risk-engine-scaledobject.yaml,
// market-data-scaledobject.yaml) were then built on them and neither service
// could ever scale up. Nothing was red. This test is what would have caught it on
// day one, and it is a STATIC check because the defect is statically visible: a
// collector with no call site.
//
// WHAT COUNTS AS A WRITER, AND WHY IT IS NOT JUST "A Set() SOMEWHERE".
//
// The obvious version of this guard — look for Set/Inc/Add/Observe on the
// collector's identifier — WOULD HAVE PASSED #283, and that is the single most
// important thing about this file. pkg/bus/metrics.go did contain
// `m.consumerLag.WithLabelValues(…).Set(lag)`; it was inside SetConsumerLag,
// and SetConsumerLag had no callers. The dead metric was one level up from where
// the naive check looks, which is exactly where this defect class lives: a
// setter written for a poller that never got written.
//
// So a write counts only if the FUNCTION CONTAINING IT is itself used —
// referenced by name anywhere else in the module's non-test Go — or is a package
// initializer. A private setter nobody calls is not a writer. A prometheus.*Func
// collector (NewGaugeFunc, NewCounterFunc, …) needs no separate writer at all:
// the closure IS the write, evaluated at scrape time.
//
// SCOPE AND LIMITS, stated so nobody reads more into a green run than it carries.
//
//   - The match is by IDENTIFIER NAME, module-wide, not by type resolution. A
//     collector bound to a field named `Lag` is satisfied by any `.Lag.Set(…)`
//     anywhere, including on an unrelated type. That fails OPEN on collision and
//     it is the deliberate trade: a type-checked version needs go/packages over
//     the whole module in a test that must stay fast, and the failure this exists
//     to catch — a name with ZERO live writers anywhere — is not one a collision
//     can hide.
//   - The used-by-name check is ONE level, not a transitive call graph. A writer
//     inside a function called only by another function nobody calls still passes.
//     A real call graph is not buildable by name here: handlers reach this
//     estate's bus as VALUES, so `ingestor.Handler` is never "called" anywhere and
//     a transitive walk would report most of the module unreachable. One level is
//     what separates a used setter from an unused one, which is the defect.
//   - A collector HANDED TO ANOTHER FUNCTION satisfies the guard without the
//     write being found. `order.WithAccountBindings(…, sharedCollateral)` stores
//     it on a field called `sharedCount` and increments THAT, and following the
//     rename takes real dataflow — a parameter hop plus a struct-field store, in
//     another package. This is a deliberate weakening and it is bounded: the
//     defect class is a collector that is constructed, registered, and then
//     nothing whatever happens to it. Neither #283 gauge escaped anywhere; they
//     were struct fields touched only by a setter with no caller, which is what
//     the rule above catches.
//   - It proves a call site exists, not that it executes. A writer behind a
//     condition nothing satisfies still passes. Execution is what a real broker
//     is for; see pkg/bus/backlog_integration_test.go, which scrapes the registry
//     after a real subscription against a real NATS and a real Kafka.
//   - Test files are excluded on purpose. A metric written only from a _test.go is
//     exactly the defect: green suite, dead series in production.
func TestEveryRegisteredMetricHasAWriter(t *testing.T) {
	root := moduleRoot(t)

	decls, writes, used, escaped := scanModuleMetrics(t, root)
	written := liveWriters(writes, used)

	// NON-VACUITY, half one. A scan that finds no collectors passes no matter how
	// many dead metrics the module carries — broken guard, green run.
	if len(decls) < 40 {
		t.Fatalf("found only %d prometheus collector declarations in the module — the scanner is broken, "+
			"not the services (this estate had well over 40 when the guard was written)", len(decls))
	}
	// NON-VACUITY, half two. If the writer scan stopped finding call sites, every
	// metric would be reported and somebody would exempt the lot.
	if len(written) < 40 {
		t.Fatalf("found only %d identifiers with a LIVE metric write call — the writer scanner is broken", len(written))
	}
	// NON-VACUITY, half three: the *Func path must actually be exercised, or the
	// "a closure is a writer" branch is untested reasoning.
	funcCollectors := 0
	for _, d := range decls {
		if d.selfWritten {
			funcCollectors++
		}
	}
	if funcCollectors == 0 {
		t.Fatal("found zero prometheus.*Func collectors — the closure-is-the-writer branch matched nothing, " +
			"so it is unverified (services/lineage and services/oms both register several)")
	}

	seenDead := map[string]bool{}
	var problems []string
	for _, d := range decls {
		if d.selfWritten {
			continue
		}
		if d.binding == "" {
			problems = append(problems, fmt.Sprintf(
				"%s:%d  %s is constructed inline and bound to nothing, so nothing can ever write it",
				d.file, d.line, d.name))
			continue
		}
		if written[d.binding] || escaped[d.binding] {
			continue
		}
		if _, exempt := metricsWithoutAWriter[d.name]; exempt {
			seenDead[d.name] = true
			continue
		}
		detail := fmt.Sprintf("nothing in the module's non-test Go calls Set/Inc/Add/Observe on %q", d.binding)
		if fns := writes[d.binding]; len(fns) > 0 {
			sort.Strings(fns)
			detail = fmt.Sprintf("the only write to %q is inside %s, which nothing else in the module "+
				"references — a setter with no caller is not a writer, and this is the exact shape of #283",
				d.binding, strings.Join(fns, ", "))
		}
		problems = append(problems, fmt.Sprintf("%s:%d  %s: %s", d.file, d.line, d.name, detail))
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d registered metric(s) have no writer:\n  %s\n\n"+
			"A registered collector with no writer exports the metric family with NO SERIES, and an empty "+
			"series reads as zero to every consumer: an alert cannot fire, a dashboard panel renders quiet, "+
			"and a KEDA trigger holds the workload at minReplicaCount under any load (#283). Either write it, "+
			"convert it to a prometheus.*Func whose closure reads the state it describes, or delete it. Adding "+
			"an entry to metricsWithoutAWriter requires a tracked issue that will remove it again.",
			len(problems), strings.Join(problems, "\n  "))
	}

	// DEAD-ENTRY CHECK, the same shape as metricSurfacesPendingRepair in
	// observability_metrics_test.go and dlqExemptBroadcastOnlyConsumers in
	// bus_dlq_test.go. An exemption that no longer describes anything protects
	// nothing, looks load-bearing, and silently covers the next metric that takes
	// its name.
	var dead []string
	for name := range metricsWithoutAWriter {
		if !seenDead[name] {
			dead = append(dead, name)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("metricsWithoutAWriter names %d metric(s) that now have a writer, or no longer exist: %s\n\n"+
			"The repair happened — delete the entry.", len(dead), strings.Join(dead, ", "))
	}
}

// metricsWithoutAWriter is a DEFAULT-DENY allow-list: every collector in the
// module is checked unless its metric name is named here with a justification
// and the issue that will remove it.
//
// IT IS EMPTY, AND THAT IS THE POINT — the two metrics that motivated this guard
// (kanz_bus_consumer_lag, kanz_bus_pending_messages) were repaired in the same
// change that added it, so it landed with nothing to exempt. Leave the map rather
// than deleting it: an empty default-deny list is a working guard with nothing
// carved out, and removing the mechanism means the next person needing a
// temporary exemption reaches for weakening the check instead.
var metricsWithoutAWriter = map[string]string{}

// metricDecl is one prometheus collector construction found in the source.
type metricDecl struct {
	name        string
	binding     string // identifier the collector is assigned to; "" ⇒ unbound
	file        string // module-relative, forward slashes
	line        int
	selfWritten bool // a *Func collector — its closure is the writer
}

// collectorCtors are the prometheus constructors that produce a collector
// somebody must write to. The *Func variants are handled separately (see
// selfWritingCtors) because their closure is the write.
var collectorCtors = map[string]bool{
	"NewCounter": true, "NewCounterVec": true,
	"NewGauge": true, "NewGaugeVec": true,
	"NewHistogram": true, "NewHistogramVec": true,
	"NewSummary": true, "NewSummaryVec": true,
}

var selfWritingCtors = map[string]bool{
	"NewCounterFunc": true, "NewGaugeFunc": true, "NewUntypedFunc": true,
}

var metricWriteMethods = map[string]bool{
	"Set": true, "Inc": true, "Dec": true, "Add": true, "Sub": true,
	"Observe": true, "SetToCurrentTime": true,
}

// scanModuleMetrics walks the module's non-test Go and returns
//
//	decls  — every collector declaration;
//	writes — collector-binding name ⇒ the names of the functions that write it
//	         ("" for a write outside any function);
//	used   — every identifier referenced anywhere EXCEPT as its own declaration's
//	         name, which is how a function that nothing calls is recognised;
//	esc    — identifiers handed to a function as an argument, where the guard
//	         loses track of them (see the type comment's third limit).
func scanModuleMetrics(t *testing.T, root string) ([]metricDecl, map[string][]string, map[string]bool, map[string]bool) {
	t.Helper()

	var decls []metricDecl
	writes := map[string][]string{}
	used := map[string]bool{}
	esc := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".gotmp":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file this package cannot parse is not a pass. It is a hole the
			// guard cannot see through, and silence here is how a dead metric hides.
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		decls = append(decls, collectorDecls(t, fset, f, rel)...)
		collectWrites(f, writes)
		collectUsedNames(f, used)
		collectEscapedNames(f, esc)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return decls, writes, used, esc
}

// registerMethods are the calls that do NOT count as handing a collector
// somewhere it might be written. Registering a collector is exactly what every
// dead metric in this defect class already does; if `MustRegister(x)` counted as
// an escape, the guard would exempt precisely the thing it exists to find.
var registerMethods = map[string]bool{
	"MustRegister": true, "Register": true, "Unregister": true,
}

// goPredeclared is Go's predeclared identifier set. A conversion or builtin call
// is not somewhere a collector can escape to; see collectEscapedNames.
var goPredeclared = map[string]bool{
	"bool": true, "byte": true, "complex64": true, "complex128": true, "error": true,
	"float32": true, "float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true, "uint": true,
	"uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"any": true, "comparable": true,
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// collectEscapedNames records identifiers passed as a bare argument to a call,
// which is where this guard's name-based tracking ends. See the type comment.
func collectEscapedNames(f *ast.File, into map[string]bool) {
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Neither registering a collector nor WRITING to one is an escape. The
		// write exclusion is not cosmetic: `m.pending…Set(pending)` passes an
		// argument that shares the collector field's name, and counting that as an
		// escape marked kanz_bus_pending_messages live while its setter had no
		// caller — the guard exempting the very metric it was written for.
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (registerMethods[sel.Sel.Name] || metricWriteMethods[sel.Sel.Name]) {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && (registerMethods[id.Name] || goPredeclared[id.Name]) {
			// A conversion is syntactically a call: `float64(pending)` would
			// otherwise mark `pending` as having escaped somewhere it might be
			// written, which is how kanz_bus_pending_messages evaded the first two
			// drafts of this guard. Predeclared names cannot write a metric.
			return true
		}
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok {
				into[id.Name] = true
			}
		}
		return true
	})
}

// liveWriters reduces writes+used to the set of collector bindings that have at
// least one write in a function something else actually uses.
//
// THIS IS THE STEP THAT WOULD HAVE CAUGHT #283. `consumerLag` had a write; the
// write was in SetConsumerLag; nothing referenced SetConsumerLag; so consumerLag
// had no live writer and the gauge exported no series.
func liveWriters(writes map[string][]string, used map[string]bool) map[string]bool {
	live := map[string]bool{}
	for binding, fns := range writes {
		for _, fn := range fns {
			// "" is a write outside any function (a package-level initializer), and
			// main/init run without anyone naming them.
			if fn == "" || fn == "main" || fn == "init" || used[fn] {
				live[binding] = true
				break
			}
		}
	}
	return live
}

// collectorDecls finds every collector construction in f and the identifier it
// is bound to.
//
// Binding is resolved from the three forms this estate uses — a keyed field in a
// composite literal (`pending: prometheus.NewGaugeVec(…)`), a short assignment
// (`x := …`) and a var spec — rather than by name resolution, which would need
// the type checker. A construction matching none of them is reported UNBOUND,
// which is the correct answer: `reg.MustRegister(prometheus.NewGaugeVec(…))`
// really does register a collector nothing can ever write.
func collectorDecls(t *testing.T, fset *token.FileSet, f *ast.File, rel string) []metricDecl {
	t.Helper()

	// Pass one: every collector construction, keyed by position.
	type found struct {
		name        string
		selfWritten bool
		line        int
	}
	ctors := map[token.Pos]found{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "prometheus" {
			return true
		}
		self := selfWritingCtors[sel.Sel.Name]
		if !collectorCtors[sel.Sel.Name] && !self {
			return true
		}
		name := metricNameFromOpts(t, rel, fset.Position(call.Pos()).Line, call)
		if name == "" {
			return true
		}
		ctors[call.Pos()] = found{name: name, selfWritten: self, line: fset.Position(call.Pos()).Line}
		return true
	})
	if len(ctors) == 0 {
		return nil
	}

	// Pass two: claim each construction for the identifier it is bound to.
	binding := map[token.Pos]string{}
	claim := func(lhs ast.Expr, rhs ast.Expr) {
		call, ok := rhs.(*ast.CallExpr)
		if !ok {
			return
		}
		if _, isCtor := ctors[call.Pos()]; !isCtor {
			return
		}
		if id, ok := lhs.(*ast.Ident); ok {
			binding[call.Pos()] = id.Name
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.KeyValueExpr:
			claim(v.Key, v.Value)
		case *ast.AssignStmt:
			for i := range v.Rhs {
				if i < len(v.Lhs) {
					claim(v.Lhs[i], v.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i := range v.Values {
				if i < len(v.Names) {
					claim(v.Names[i], v.Values[i])
				}
			}
		}
		return true
	})

	out := make([]metricDecl, 0, len(ctors))
	for pos, c := range ctors {
		out = append(out, metricDecl{
			name: c.name, binding: binding[pos], file: rel, line: c.line, selfWritten: c.selfWritten,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

// metricNameFromOpts pulls the literal Name out of a collector's Opts argument.
//
// A collector whose Opts carry Namespace or Subsystem instead of a full literal
// Name is a HARD FAILURE rather than a skip. The assembled name would not match
// what the guard reports, and — the reason this is fatal — it would also break
// TestEveryObservabilityMetricExistsInSource, whose own doc says so explicitly. Two
// guards silently degrading together is worse than either one being red.
func metricNameFromOpts(t *testing.T, rel string, line int, call *ast.CallExpr) string {
	t.Helper()
	if len(call.Args) == 0 {
		return ""
	}
	lit, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return ""
	}
	var name string
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Namespace", "Subsystem":
			t.Errorf("%s:%d declares a collector with %s in its Opts. Every metric in this module uses a "+
				"full literal Name; the assembled form defeats this guard AND "+
				"TestEveryObservabilityMetricExistsInSource. Match on the assembled name in both, or use a literal.",
				rel, line, key.Name)
		case "Name":
			s, ok := kv.Value.(*ast.BasicLit)
			if ok && s.Kind == token.STRING {
				if unquoted, err := strconv.Unquote(s.Value); err == nil {
					name = unquoted
				}
			}
		}
	}
	return name
}

// collectWrites records, for every metric write call in f, every identifier on
// the selector chain leading to it, against the name of the top-level function
// containing it. `m.pending.WithLabelValues(s, g).Set(v)` inside
// `func (m *BusMetrics) SetPending(…)` contributes
// writes["pending"] += "SetPending" (and, harmlessly, "m" and "WithLabelValues"
// — no collector is ever bound to a name like those).
//
// A write inside a closure is attributed to the named function the closure is
// written in, which is the right answer: the closure's liveness is its enclosing
// function's.
func collectWrites(f *ast.File, into map[string][]string) {
	record := func(fn string, n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !metricWriteMethods[sel.Sel.Name] {
				return true
			}
			names := map[string]bool{}
			chainNames(sel.X, names)
			for name := range names {
				if !contains(into[name], fn) {
					into[name] = append(into[name], fn)
				}
			}
			return true
		})
	}
	for _, d := range f.Decls {
		switch v := d.(type) {
		case *ast.FuncDecl:
			if v.Body != nil {
				record(v.Name.Name, v.Body)
			}
		default:
			record("", d) // package-level initializer
		}
	}
}

// contains is shared with the rest of this package — see
// observability_rules_reachable_test.go.

// collectUsedNames records every identifier referenced in f EXCEPT a function
// declaration's own name. A function whose name never turns up here is called by
// nothing and referenced as a value by nothing, so the metric writes inside it
// never run.
//
// Declaring the name is not using it — that asymmetry is the whole mechanism,
// and it is why FuncDecl.Name is walked around rather than through.
func collectUsedNames(f *ast.File, into map[string]bool) {
	add := func(n ast.Node) {
		if n == nil {
			return
		}
		ast.Inspect(n, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				into[v.Name] = true
			case *ast.SelectorExpr:
				into[v.Sel.Name] = true
			}
			return true
		})
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			add(d)
			continue
		}
		if fn.Recv != nil {
			add(fn.Recv)
		}
		add(fn.Type)
		if fn.Body != nil {
			add(fn.Body)
		}
	}
}

func chainNames(e ast.Expr, into map[string]bool) {
	switch v := e.(type) {
	case *ast.Ident:
		into[v.Name] = true
	case *ast.SelectorExpr:
		into[v.Sel.Name] = true
		chainNames(v.X, into)
	case *ast.CallExpr:
		chainNames(v.Fun, into)
	case *ast.IndexExpr:
		chainNames(v.X, into)
	case *ast.ParenExpr:
		chainNames(v.X, into)
	case *ast.StarExpr:
		chainNames(v.X, into)
	}
}

// TestMetricWriterGuardCatchesADeadGauge is the guard's own proof of work.
//
// A count floor shows the scanners found SOMETHING; it does not show the
// analysis distinguishes a written metric from a dead one. This runs every case
// through the same code the guard above uses and asserts it separates them, so a
// future edit that makes the analyzer permissive fails here rather than turning
// the real guard silently green.
//
// `kanz_sample_orphan_setter` IS #283, reduced: a collector whose only Set()
// sits in an exported setter that nothing calls. It is here because the first
// draft of this guard passed it, which is the same way the metric it was written
// for passed everything for years.
//
// Its setter's PARAMETER deliberately shares the field's name and is passed
// through a conversion — `Set(float64(orphanSetter))`. Both of those are
// syntactically calls taking a bare identifier, and each in turn made a draft of
// collectEscapedNames mark the collector as escaped, i.e. exempt. That is
// precisely how kanz_bus_pending_messages slipped past the guard written to
// catch it, twice, before the exclusions in collectEscapedNames existed. Remove
// either exclusion and this case flips to "escaped" and fails here.
func TestMetricWriterGuardCatchesADeadGauge(t *testing.T) {
	const src = `package sample

import "github.com/prometheus/client_golang/prometheus"

type m struct {
	live         *prometheus.GaugeVec
	dead         *prometheus.GaugeVec
	orphanSetter *prometheus.GaugeVec
}

func build(reg prometheus.Registerer, sink func(prometheus.Counter)) *m {
	x := &m{
		live:         prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kanz_sample_live"}, []string{"a"}),
		dead:         prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kanz_sample_dead"}, []string{"a"}),
		orphanSetter: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kanz_sample_orphan_setter"}, []string{"a"}),
	}
	escapes := prometheus.NewCounter(prometheus.CounterOpts{Name: "kanz_sample_escapes"})
	reg.MustRegister(x.live, x.dead, x.orphanSetter, escapes)
	sink(escapes)
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "kanz_sample_func"}, func() float64 { return 1 }))
	reg.MustRegister(prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kanz_sample_unbound"}, []string{"a"}))
	return x
}

func (x *m) touch(v float64) { x.live.WithLabelValues("a").Set(v) }

func (x *m) SetOrphan(orphanSetter float64) {
	x.orphanSetter.WithLabelValues("a").Set(float64(orphanSetter))
}

func caller(x *m) { x.touch(1) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	decls := collectorDecls(t, fset, f, "sample.go")
	writes := map[string][]string{}
	used := map[string]bool{}
	esc := map[string]bool{}
	collectWrites(f, writes)
	collectUsedNames(f, used)
	collectEscapedNames(f, esc)
	written := liveWriters(writes, used)

	got := map[string]string{}
	for _, d := range decls {
		switch {
		case d.selfWritten:
			got[d.name] = "func"
		case d.binding == "":
			got[d.name] = "unbound"
		case written[d.binding]:
			got[d.name] = "written"
		case esc[d.binding]:
			got[d.name] = "escaped"
		default:
			got[d.name] = "dead"
		}
	}
	want := map[string]string{
		"kanz_sample_live":          "written",
		"kanz_sample_dead":          "dead",
		"kanz_sample_orphan_setter": "dead",
		"kanz_sample_escapes":       "escaped",
		"kanz_sample_func":          "func",
		"kanz_sample_unbound":       "unbound",
	}
	for name, expect := range want {
		if got[name] != expect {
			t.Errorf("%s: analyzer says %q, want %q — the guard no longer separates a written metric from a "+
				"dead one, so TestEveryRegisteredMetricHasAWriter is green for the wrong reason",
				name, got[name], expect)
		}
	}
	if len(got) != len(want) {
		t.Errorf("analyzer found %d collectors in the fixture, want %d: %v", len(got), len(want), got)
	}
}

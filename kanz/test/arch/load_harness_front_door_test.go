package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A LOAD HARNESS MAY NOT PUBLISH AN ORDER COMMAND (#865).
//
// # The rule this enforces, and why it is not a style preference
//
// CLAUDE.md: "No step is skippable — not by MCP, not by an admin endpoint, not by
// a test helper that 'just needs a fill.'" A write-path load generator is the
// most tempting place in the estate to break that. Publishing
// `order.order.submit` straight onto the bus is four lines, needs no gateway, no
// token and no role — and it skips the gateway's authentication, its trade-role
// check, its halt gate and its idempotency handling, which are four controls on
// the capital path. The resulting throughput number would describe a path no
// client can take, which is worse than having no number: it would be quoted.
//
// test/load/orderflow submits through POST /v1/orders for exactly that reason.
// This guard is what keeps that true for whatever lands in test/load/ next,
// rather than a paragraph in one file's doc comment that a second harness would
// never read.
//
// # The forbidden set is DERIVED, not listed
//
// The order COMMAND subjects are read out of services/oms/internal/order's own
// constants. A list written here would be a second copy of the thing it guards —
// the defect class where somebody enumerates a set by hand and misses a member —
// and a fifth command subject added tomorrow would be publishable from test/load
// with this guard still green.
//
// # What it matches, precisely
//
// A command subject is forbidden WHERE IT IS BEING PUBLISHED: as the value of a
// `Subject:` or `EventType:` field in a composite literal (how every publisher in
// this estate builds a bus.Event), or as an argument to a `Publish` call. It is
// NOT forbidden as data — test/load/orderflow reads
// kanz_bus_pending_messages{subject="order.order.submit"} to see the standing
// command backlog, and naming the subject there is the measurement, not a
// bypass. A rule that banned the string outright would have forced that
// measurement to obscure its own label, which is worse than not having the rule.
//
// The second prong is the event CLASS: nothing under test/load may name
// EVENT_CLASS_COMMAND. A harness issuing a command has to classify it as one,
// and that name is much harder to reach by accident than a subject string.
//
// # What it cannot catch
//
// It matches parsed Go, so a comment naming the subject does not trip it (the
// failure mode where a guard matches its own prose) and neither does a subject
// assembled by concatenation at runtime. That gap is accepted: this guards the
// easy, tempting shortcut, not a determined author. The positive assertion below
// covers the other direction — the front door has to still be there.
func TestNoLoadHarnessPublishesAnOrderCommand(t *testing.T) {
	root := moduleRoot(t)
	forbidden := orderCommandSubjects(t, root)
	// NON-VACUITY. If the constants move or are renamed, this set empties and
	// every check below passes while asserting nothing.
	if len(forbidden) < 4 {
		t.Fatalf("derived only %d order command subject(s) from services/oms/internal/order — the "+
			"scanner is broken, not the harness, and this guard would pass vacuously: %v",
			len(forbidden), sortedSet(forbidden))
	}

	loadRoot := filepath.Join(root, "test", "load")
	files := 0
	sawFrontDoor := false
	err := filepath.WalkDir(loadRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipWalkDir(d) {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		fset := token.NewFileSet()
		// ParseFile WITHOUT ParserComments: the literals this walks are code, and
		// a guard that also read comments would fire on the very paragraphs
		// explaining why the subject must not be published here.
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		// FILE-LOCAL CONSTANTS ARE RESOLVED FIRST, and this is not a refinement —
		// it is what the guard needed to work at all. The first revision matched
		// only string LITERALS, so a mutation that declared
		// `const subjectShortcut = "order.order.submit"` and then built
		// `bus.Event{Subject: subjectShortcut}` SURVIVED it: the publish was
		// there, the guard was green, and the shape it survived through is the
		// ordinary one — every publisher in test/load already names its subjects
		// through consts, because that is how seed/ and ingest/ are written.
		consts := stringConsts(f)
		offend := func(pos token.Pos, subject, where string) {
			t.Errorf("%s:%d publishes the order COMMAND subject %q (%s).\n"+
				"  A load harness must submit orders through the api-gateway's POST /v1/orders,\n"+
				"  not by publishing the command itself. Publishing it skips the gateway's\n"+
				"  authentication, its trade-role check, its halt gate and its idempotency\n"+
				"  handling — four controls on the capital path — and the throughput number\n"+
				"  that came back would describe a path no client can take.\n"+
				"  See test/load/orderflow/submit.go.",
				rel, fset.Position(pos).Line, subject, where)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				// NON-TEST FILES ONLY, and this cost a surviving mutation to
				// learn. Rewriting the harness's own URL to "/v1/nowhere" left
				// this guard green, because submit_test.go asserts the route and
				// its literal satisfied the check — the positive half was being
				// met by a test ABOUT the front door rather than by a harness
				// USING it.
				if strings.HasSuffix(path, "_test.go") {
					return true
				}
				if v, uerr := strconv.Unquote(node.Value); uerr == nil && v == "/v1/orders" {
					sawFrontDoor = true
				}
			case *ast.Ident:
				// The event class, wherever it appears. A command has to be
				// classified as one.
				if strings.Contains(node.Name, "EVENT_CLASS_COMMAND") {
					t.Errorf("%s:%d names %s. Nothing under test/load may issue a COMMAND: the write "+
						"harness goes through the api-gateway, which is what puts an order in front "+
						"of the gateway's auth, trade-role, halt and idempotency controls.",
						rel, fset.Position(node.Pos()).Line, node.Name)
				}
			case *ast.KeyValueExpr:
				// The publisher shape: bus.Event{Subject: ..., EventType: ...}.
				key, ok := node.Key.(*ast.Ident)
				if !ok || (key.Name != "Subject" && key.Name != "EventType") {
					return true
				}
				if v, ok := stringValue(node.Value, consts); ok && forbidden[v] {
					offend(node.Pos(), v, "as a published event's "+key.Name)
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || !strings.Contains(sel.Sel.Name, "Publish") {
					return true
				}
				for _, arg := range node.Args {
					if v, ok := stringValue(arg, consts); ok && forbidden[v] {
						offend(node.Pos(), v, "as an argument to "+sel.Sel.Name)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", loadRoot, err)
	}
	if files == 0 {
		t.Fatalf("parsed zero .go files under %s — test/load moved, and this guard is asserting nothing", loadRoot)
	}
	// THE POSITIVE HALF. Without it, deleting the write harness outright would
	// make this file green forever: no publisher, no violation, and no measurement
	// of the write path either. #865 is closed by a harness that EXISTS.
	if !sawFrontDoor {
		t.Error("no file under test/load submits to \"/v1/orders\". The write-path harness that #865 " +
			"asked for is gone, or it stopped using the front door — either way the write path has " +
			"no measured degradation point again.")
	}
}

// EVERY METRIC THE WRITE HARNESS TURNS ON MUST EXIST IN THE ESTATE (#865).
//
// The harness refuses to run on kanz_oms_live_venue_adapters, decides a run is
// INVALID on kanz_compliance_ungoverned_orders_total, and reads its saturation
// signal from kanz_bus_pending_messages. Every one of those is a hand-written
// string against a metric owned by another package, and the failure mode when one
// is renamed is silent in the worst direction: an absent family reads as "the
// counter did not move", so a harness whose watch list has gone stale reports a
// clean run forever.
//
// declaredGoMetrics is the same truth set TestEveryObservabilityMetricExistsInSource
// uses for the alert rules — one answer to "does this metric exist", reused rather
// than re-derived.
func TestEveryMetricTheWriteHarnessWatchesExists(t *testing.T) {
	root := moduleRoot(t)
	// NOT declaredGoMetrics, AND THAT COST A SURVIVING MUTATION TO LEARN.
	//
	// declaredGoMetrics walks the WHOLE module, test/load included, so the
	// harness's own watch list declares itself: renaming a watched counter to
	// kanz_oms_claim_timeout_total — a metric nothing exports — left this guard
	// green, because the rename WAS the declaration. The truth set has to be the
	// metrics somebody ELSE exports.
	declared := metricsDeclaredOutsideTheLoadHarness(t, root)
	if len(declared) == 0 {
		t.Fatal("found zero kanz_* metric literals outside test/load — the scanner is broken, not the harness")
	}

	watched := metricLiteralsUnder(t, filepath.Join(root, "test", "load"))
	if len(watched) == 0 {
		t.Fatal("test/load names no kanz_* metric at all — either the write harness is gone or this " +
			"guard stopped finding it, and it would pass vacuously either way")
	}
	for _, m := range sortedSet(watched) {
		if !declared[m] {
			t.Errorf("test/load watches %q, which no Go source in this module declares.\n"+
				"  A metric family that is not exported reads as ZERO to every consumer, so a load\n"+
				"  run watching it reports 'this control did not degrade' on the strength of a\n"+
				"  series that does not exist.", m)
		}
	}
}

// orderCommandSubjects reads the order COMMAND subject constants out of
// services/oms/internal/order, the package that owns them.
//
// It takes the consts whose NAME begins with "Subject", which is exactly the
// naming that file uses for the four commands the OMS consumes
// (SubjectSubmit/Amend/Cancel/Approve) and does not use for the FACT event types
// (EventType*). A FACT subject in a load harness is fine — test/load/orderflow
// subscribes to two of them — so the two families must not be conflated.
func orderCommandSubjects(t *testing.T, root string) map[string]bool {
	t.Helper()
	path := filepath.Join(root, "services", "oms", "internal", "order", "events.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if !strings.HasPrefix(name.Name, "Subject") || i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if v, uerr := strconv.Unquote(lit.Value); uerr == nil {
				out[v] = true
			}
		}
		return true
	})
	return out
}

// metricLiteralsUnder collects every kanz_* string literal in the Go under dir.
func metricLiteralsUnder(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipWalkDir(d) {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			// A WATCHED METRIC IS A BARE NAME, and the anchoring matters: a
			// prefix test also matched error-message fragments that merely BEGIN
			// with a metric name ("kanz_oms_simulated_venues, so whether an order
			// ..."), and this guard then reported the harness as watching a
			// metric nobody declares. A guard that fires on its own prose is
			// noise that gets muted.
			if uerr == nil && metricName.MatchString(v) {
				out[v] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// metricName is a WHOLE Prometheus metric name in this estate's namespace.
var metricName = regexp.MustCompile(`^kanz_[a-z0-9_]+$`)

// stringLit (subject_topology_test.go), stringConsts and stringValue
// (operator_placement_test.go) are reused rather than re-implemented here — one
// unwrapper and one resolver per concept, so a fix to either reaches both guards.

// metricsDeclaredOutsideTheLoadHarness is the set of kanz_* metric names the
// module declares ANYWHERE EXCEPT test/load.
//
// It is deliberately the same shape as declaredGoMetrics — a literal scan over
// non-test Go — minus the one subtree whose inclusion made this guard vacuous.
// The two answer different questions: that one asks "does this metric exist in
// Go at all", which is the right question for the alert rules; this one asks
// "does something other than the load harness export it", which is the only
// question that can catch a watch list gone stale.
func metricsDeclaredOutsideTheLoadHarness(t *testing.T, root string) map[string]bool {
	t.Helper()
	loadRoot := filepath.Join(root, "test", "load")
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipWalkDir(d) || path == loadRoot {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range metricLiteral.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// metricLiteral matches a quoted kanz_* metric name in Go source.
var metricLiteral = regexp.MustCompile(`"(kanz_[a-z0-9_]+)"`)

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

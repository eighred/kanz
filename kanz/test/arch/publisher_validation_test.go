package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY PACKAGE THAT PUBLISHES A FACT MUST PROVE ONE ENVELOPE AGAINST A REAL
// PRODUCER (#245).
//
// pkg/bus/producer.go's own header states the cost in past tense: TWELVE publish
// sites "validated fine in unit tests (which inject fake Publishers that never
// validate) and failed on the first real broker." That is not a coverage gap. It
// is a STRUCTURAL one — the doubles are at the wrong LAYER.
//
// There are two places a test can substitute a fake, and only one of them is safe:
//
//	Event level    something.Publish(ctx, bus.Event{…})   ← the Producer's own API
//	Message level  client.Publish(ctx, bus.Message{…})    ← the transport
//
// A double at the EVENT level replaces bus.Producer, and Producer is what stamps
// event_id / publish_time / producer_sequence / payload_schema_ref / tenant_id
// and then runs bus.Validate. Everything that makes an envelope legal happens in
// the thing the double removed, so the test asserts the handler's intent and
// nothing about what a broker would accept. Seven of this module's ten doubles
// accept literally any Event.
//
// A double at the MESSAGE level (the "Tier-B" pattern in internal/risk/publish
// and now services/accounting/internal/cashmove) keeps the real Producer and
// fakes only the socket. Validate runs. The envelope is real. That costs about
// twenty lines and it is the whole difference.
//
// So the rule: a package that builds a bus.Event in non-test code must construct
// a real bus.Producer somewhere in its own tests. It does not forbid the
// Event-level double — those are genuinely the cheapest way to assert which
// FACTs a handler emits in what order, and error injection needs them — it
// requires that the package ALSO pin its envelope against the real thing at
// least once.
//
// WHAT IT WOULD AND WOULD NOT HAVE CAUGHT. It catches the class exactly:
// services/accounting/internal/cashmove moves investor cash into the ledger, had
// seven tests, and every one of them called the unexported encode() — the
// package had never once seen bus.Validate. It does NOT catch a bad field that
// Validate does not check (a wrong PartitionKey is a legal envelope on the wrong
// ordering key), and it cannot catch a subject with no stream bound to it; that
// needs a real broker, which is what TestEverySubjectIsCarriedByAStream and the
// *_integration_test.go files are for.
//
// LIMITS, stated so a green run is not read for more than it says.
//
//   - Syntactic, by identifier. "Constructs a bus.Event" is `bus.Event{…}` in a
//     non-test file; "has a real producer" is a `bus.NewProducer` call in a
//     _test.go in the same DIRECTORY. Both are name matches, not type resolution.
//     Type-checking the module here would need go/packages over every package on
//     a test that has to stay fast, and the defect this exists to catch — a
//     publishing package with NO real producer anywhere near it — is not one an
//     aliased import can hide, because an alias would break the population scan
//     and the count floor below.
//   - It proves a Producer is CONSTRUCTED, not that the package's own publish
//     path runs through it. A test that builds a producer and never uses it
//     satisfies this. That is the same bounded weakening
//     TestEveryRegisteredMetricHasAWriter accepts for the same reason: the
//     failure mode is a package with nothing of the kind, and a decorative
//     producer is not a shape anyone writes by accident.
//   - The proof must be in the package's OWN directory. A Tier-B test living in
//     test/contract/ reads as absent here, deliberately: a shared contract test
//     does not travel with the package when its envelope changes.
func TestEveryFactPublisherProvesOneEnvelope(t *testing.T) {
	root := moduleRoot(t)
	publishers, proven := scanFactPublishers(t, root)

	// NON-VACUITY. If the population scan breaks, every package passes and the
	// guard reports nothing while protecting nothing.
	if len(publishers) < 15 {
		t.Fatalf("found only %d packages constructing a bus.Event — the population scan is broken, "+
			"not the module (there were 22 when this guard was written)", len(publishers))
	}
	// If the PROOF scan breaks the other way, every package is reported and the
	// exemption list swallows the module.
	if len(proven) < 5 {
		t.Fatalf("found only %d packages constructing a bus.Producer in their own tests — the proof scan "+
			"is broken (there were 11 when this guard was written)", len(proven))
	}

	// DEAD-ENTRY CHECK for the integration proofs, before they are honoured: an
	// entry naming a test that no longer exists would silently excuse a package.
	for dir, note := range provenByRealBrokerTest {
		name := note.test
		found := false
		for _, f := range testFilesIn(t, root, dir) {
			if strings.Contains(f, "func "+name+"(") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("provenByRealBrokerTest names %s in %s, and no such test exists there any more.\n\n"+
				"That entry is what stops this guard reporting the package. Either the test was renamed "+
				"(update the entry) or the real-broker proof is gone (move the package back into "+
				"factPublishersWithoutARealProducer, with the issue that repairs it).", name, dir)
		}
	}

	seenExempt := map[string]bool{}
	var problems []string
	for _, pkg := range publishers {
		if _, ok := provenByRealBrokerTest[pkg.dir]; ok {
			continue
		}
		if proven[pkg.dir] {
			continue
		}
		if _, exempt := factPublishersWithoutARealProducer[pkg.dir]; exempt {
			seenExempt[pkg.dir] = true
			continue
		}
		problems = append(problems, fmt.Sprintf("%s  (bus.Event constructed at %s:%d)", pkg.dir, pkg.file, pkg.line))
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d package(s) publish a FACT with no test that constructs a real bus.Producer:\n  %s\n\n"+
			"Every envelope these packages emit is proven only by a double that does not run bus.Validate, "+
			"which is the shape pkg/bus/producer.go records failing on the first real broker. The fix is the "+
			"Tier-B pattern: build a real bus.Producer over a fake bus.Client (see "+
			"internal/risk/publish/publish_test.go or services/accounting/internal/cashmove/publish_test.go), "+
			"then assert bus.Validate on the unframed envelope. Adding an entry to "+
			"factPublishersWithoutARealProducer requires a reason and the issue that removes it.",
			len(problems), strings.Join(problems, "\n  "))
	}

	// DEAD-ENTRY CHECK, the same shape as metricsWithoutAWriter. An exemption that
	// no longer describes anything protects nothing, reads as load-bearing, and
	// silently covers the next package that moves into its path.
	var dead []string
	for dir := range factPublishersWithoutARealProducer {
		if !seenExempt[dir] {
			dead = append(dead, dir)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("factPublishersWithoutARealProducer names %d package(s) that now have a real-producer test, "+
			"or no longer publish: %s\n\nThe repair happened — delete the entry.", len(dead), strings.Join(dead, ", "))
	}
}

// factPublishersWithoutARealProducer is a DEFAULT-DENY allow-list, keyed by
// module-relative package directory. Every package that builds a bus.Event is
// checked unless it is named here with a reason and the issue that removes it.
//
// It landed with eleven entries and that is a statement about the module, not
// about the guard: this is the backlog #245 catalogued, written down where it
// cannot be lost. The two the issue ranked highest — cashmove and the OMS order
// service — are not on it, because they were repaired in the change that added
// it.
// integrationProof names the real-broker test that pins a package's envelope.
type integrationProof struct{ test, why string }

// testFilesIn returns the contents of every _test.go directly in dir. Errors are
// fatal rather than skipped: a directory this cannot read is one whose proof this
// guard cannot see, and treating that as "no proof" would move a proven package
// into the exemption list on a filesystem hiccup.
func testFilesIn(t *testing.T, root, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, readFile(t, filepath.Join(root, filepath.FromSlash(dir), e.Name())))
	}
	return out
}

// provenByRealBrokerTest is the SECOND proof shape, and it exists because the
// first one has a false negative that sent someone to write a redundant test.
//
// The scan reads "has a real producer" as a literal bus.NewProducer call in the
// package's own _test.go. A package whose test drives its OWN ENTRYPOINT against
// a live broker therefore reads as unproven — the NewProducer call is in main.go,
// one frame down, where a name match cannot see it. cmd/kanz-halt was listed
// below as having "no broker-shaped test at all" while
// TestIntegration_ToolFlipsTheRealGate was publishing its FACT over a real
// JetStream spine and asserting the real gate flipped, in CI, on every run.
//
// THAT IS STRONGER EVIDENCE THAN THE TIER-B PATTERN, NOT WEAKER. Tier-B builds a
// real Producer over a FAKE bus.Client: it proves Validate accepts the envelope.
// These tests publish to a real broker and assert the consumer on the far side
// reacted, which additionally proves the subject is bound to a stream — the case
// this guard's own comment says it cannot catch.
//
// It is an EXPLICIT LIST rather than a widened heuristic on purpose. The obvious
// generalisation — "the package's tests dial a real broker" — would mark a
// package proven for dialling in order to CONSUME, which is not evidence about
// anything it publishes. A false positive here silently drops coverage, which is
// worse than the false negative it would fix. Each entry names the test, and the
// dead-entry check above fails if that test stops existing.
var provenByRealBrokerTest = map[string]integrationProof{
	"cmd/kanz-halt": {
		test: "TestIntegration_ToolFlipsTheRealGate",
		why: "publishes the halt FACT through run(), the tool's real entrypoint, over a live " +
			"JetStream spine, then asserts the real translate.Gate folded it and flipped. Gated on " +
			"TEST_NATS_URL, which CI sets (kanz-ci.yml), so it executes rather than skipping there.",
	},
}

var factPublishersWithoutARealProducer = map[string]string{
	// NO OPERATOR CLI IS LISTED ANY MORE, and the group is kept as a heading
	// rather than deleted so the next control-plane CLI lands under it with the
	// bar already visible. kanz-halt was never really here (see
	// provenByRealBrokerTest — its entry described the scan's blind spot);
	// kanz-altevent and kanz-household are pinned Tier-B in their own
	// publish_test.go. #245.

	// Capital-path services whose only proof is an Event-level double.
	"internal/marketedge/ingest":              "capture double accepts any Event (engine_test.go:32). #245",
	"services/oms/internal/outbox":            "relay recorder accepts any Event (relay_test.go:46); the outbox records it drains are built in services/oms/internal/order. #245",
	"services/venue-binance/internal/binance": "reconCapture accepts anything; PayloadSchemaRef derivation never exercised. #245",
	"services/venue-okx/internal/okx":         "okxCapture accepts anything; same. #245",

	// Load generators. Listed rather than excluded by path: a load tool that
	// cannot publish measures nothing, and silently skipping test/ would also
	// skip anything else that moves there. Lowest value of the eleven. #245.
	"test/load/ingest": "load generator, no tests. #245",
	"test/load/seed":   "load seeder, no tests. #245",
}

// factPublisher is one non-test construction of a bus.Event.
type factPublisher struct {
	dir  string // module-relative package directory, forward slashes
	file string
	line int
}

// scanFactPublishers walks the module and returns
//
//	publishers — one entry per package directory whose NON-TEST Go constructs a
//	             bus.Event (the first construction found, for the message);
//	proven     — the set of package directories with a bus.NewProducer call in
//	             one of their own _test.go files.
//
// pkg/bus itself never appears: inside package bus the literal is `Event{}`, not
// `bus.Event{}`. That is correct — bus IS the producer, and pkg/bus/producer_test.go
// is where its own envelope contract lives.
func scanFactPublishers(t *testing.T, root string) ([]factPublisher, map[string]bool) {
	t.Helper()

	firstByDir := map[string]factPublisher{}
	proven := map[string]bool{}

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
		if filepath.Ext(path) != ".go" {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file this guard cannot parse is a hole it cannot see through, and
			// silence here is how the next unproven publisher hides.
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))

		if strings.HasSuffix(path, "_test.go") {
			if callsNewProducer(f) {
				proven[dir] = true
			}
			return nil
		}
		if line := firstBusEventLiteral(fset, f); line > 0 {
			if _, seen := firstByDir[dir]; !seen {
				firstByDir[dir] = factPublisher{dir: dir, file: rel, line: line}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	out := make([]factPublisher, 0, len(firstByDir))
	for _, p := range firstByDir {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out, proven
}

// firstBusEventLiteral returns the line of the first `bus.Event{…}` composite
// literal in f, or 0.
func firstBusEventLiteral(fset *token.FileSet, f *ast.File) int {
	line := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if line > 0 {
			return false
		}
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Event" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "bus" {
			line = fset.Position(lit.Pos()).Line
			return false
		}
		return true
	})
	return line
}

// callsNewProducer reports whether f contains a `bus.NewProducer(…)` call.
func callsNewProducer(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewProducer" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "bus" {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestFactPublisherGuardSeparatesTheTwoTiers is the guard's own proof of work.
//
// The count floors above show the scanners found SOMETHING. They do not show
// that the analysis distinguishes a Tier-B test from an Event-level double,
// which is the entire distinction the guard rests on — and the two are only one
// identifier apart in the source. This runs both shapes through the same
// detectors the guard uses, so an edit that makes them permissive fails HERE
// rather than turning the real guard silently green.
func TestFactPublisherGuardSeparatesTheTwoTiers(t *testing.T) {
	const publisherSrc = `package sample

import "github.com/eighred/kanz/pkg/bus"

type pub struct{ p publisher }

type publisher interface{ Publish(ctx any, e bus.Event) error }

func (x *pub) emit() error { return x.p.Publish(nil, bus.Event{Subject: "a.b.c"}) }
`
	// The double this guard exists to reject: it satisfies the package's own
	// Event-level interface, so bus.Producer — and therefore bus.Validate — is
	// never on the path.
	const eventDoubleSrc = `package sample

import "github.com/eighred/kanz/pkg/bus"

type capture struct{ sent []bus.Event }

func (c *capture) Publish(_ any, e bus.Event) error { c.sent = append(c.sent, e); return nil }
`
	// Tier-B: the fake is the TRANSPORT, and a real Producer sits above it.
	const tierBSrc = `package sample

import "github.com/eighred/kanz/pkg/bus"

type client struct{ sent []bus.Message }

func (c *client) Publish(_ any, m bus.Message) error { c.sent = append(c.sent, m); return nil }

func newRig() (*bus.Producer, *client) {
	c := &client{}
	p, _ := bus.NewProducer(c, bus.ProducerConfig{Source: "s", ProducerVersion: "v", Tenant: "t"})
	return p, c
}
`
	// A same-named symbol from another package must not satisfy the guard.
	const decoySrc = `package sample

import "example.com/other/kafka"

func build() { _, _ = kafka.NewProducer(nil), kafka.Event{} }
`

	parse := func(name, src string) (*token.FileSet, *ast.File) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return fset, f
	}

	fset, f := parse("publisher.go", publisherSrc)
	if got := firstBusEventLiteral(fset, f); got == 0 {
		t.Error("a package building bus.Event{} was not detected as a FACT publisher — the population " +
			"scan misses the thing the guard is about")
	}

	if _, f := parse("double_test.go", eventDoubleSrc); callsNewProducer(f) {
		t.Error("an Event-level double was accepted as a real-producer proof. That double is EXACTLY what " +
			"#245 is about: it never runs bus.Validate, so the envelope it accepts is not one a broker would.")
	}

	if _, f := parse("tierb_test.go", tierBSrc); !callsNewProducer(f) {
		t.Error("the Tier-B pattern (real bus.Producer over a fake bus.Client) was NOT recognised as a proof — " +
			"the guard would report every correctly-tested package and the exemption list would swallow the module")
	}

	fset, f = parse("decoy.go", decoySrc)
	if firstBusEventLiteral(fset, f) != 0 || callsNewProducer(f) {
		t.Error("a same-named symbol from a different package satisfied the matcher — the guard is matching " +
			"on the selector alone and would be green for the wrong reason")
	}
}

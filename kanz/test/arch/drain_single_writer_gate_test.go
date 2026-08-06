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

// ARCHIVER-DRAIN CANNOT REACH A KAFKA PRODUCE WITHOUT ITS SINGLE-WRITER GATE
// HAVING RUN FIRST (#285, assertion 3 of the design record).
//
// services/archiver is a SINGLE WRITER by deployment — one replica per stream,
// Recreate rather than RollingUpdate. archiver.go:31 states why: "Two producers
// can invert per-key order in Kafka, and a reordered log rebuilds a DIFFERENT
// book downstream — a subtler failure than losing it." That is the failure mode
// this guard exists for. It is not loss; it is a log that reads as complete and
// rebuilds the wrong book.
//
// archiver-drain re-produces parked events to THOSE SAME TOPICS. Run beside a
// live archiver it is the second producer the invariant forbids, and
// Drain.assertArchiverStopped — the probe of the archiver's own JetStream
// durable — is the entire thing standing between the operator and that outcome.
// An --archiver-is-stopped flag was considered and rejected in the design record
// precisely because it replaces a measurement with an assertion.
//
// WHAT THIS ADDS OVER drain_test.go, STATED NARROWLY, BECAUSE THE OBVIOUS
// CLAIM IS FALSE. The tempting justification — "the unit tests only prove the
// refusal fires, not that it is still on the path" — was written into this
// comment first and then disproved by mutating the code it describes. Moving
// assertArchiverStopped below the read loop in Drain.Run fails FOUR tests in
// drain_test.go, and one of them fails on precisely the right words:
//
//	--- FAIL: TestDrainRefusesWhileTheArchiverIsConsuming
//	    drain_test.go:124: the drain published 1 message(s) after the gate refused
//
// That test asserts the fake publisher is still empty after the refusal, so it
// already catches a reordered gate. It is a better test than the guard for the
// case it covers, and this guard does not replace it.
//
// What it covers is the DOOR THAT DOES NOT EXIST YET, and one package no test
// reaches at all:
//
//   - drain_test.go tests Run. It is behavioural, so it protects the
//     entrypoints someone remembered to write a test for. Adding a second
//     exported entrypoint — a RunOnce that hands a record straight to one() —
//     leaves the whole services/archiver suite green, because nothing calls it.
//     This guard is default-deny over EVERY exported method that can reach a
//     produce, so the new door is covered on the commit that adds it rather
//     than on the commit someone remembers it.
//   - services/archiver/cmd/archiver-drain has no test files. It holds a live
//     *bus.KafkaClient — it dials one and passes it in as both Kafka and Reader
//     — so a produce written there bypasses the gate entirely, and no test in
//     the module would notice. That is the second test below.
//
// Both were mutation-proved that way: suite green, guard red, in #285's PR.
//
// WHAT IT CHECKS, in the order it checks it:
//
//  1. The gate EXISTS. Some type in services/archiver/internal/archive has a
//     method that reaches a ConsumerActive probe. If none does, the gate has
//     been deleted from the package and the guard fails rather than passing over
//     a package with nothing to check.
//  2. Every EXPORTED method of that type which can reach a Kafka publish —
//     directly, or through another method or package-level function in the same
//     package — reaches the gate first, measured over the statements of its own
//     body.
//  3. The refusal is ACTED ON: the gate's error is bound and a return sits in
//     the branch that handles it. A gate whose result is discarded has run and
//     changed nothing.
//  4. The gate can still REFUSE: the method that probes returns ErrArchiverLive.
//  5. The archiver-drain COMMAND does not produce to Kafka itself, on a
//     default-deny list that is empty today. Everything it writes must go
//     through the gated Drain; a produce added to main.go would bypass the gate
//     completely rather than merely reorder it.
//
// LIMITS, stated so a green run is not read for more than it says.
//
//   - SYNTACTIC, BY IDENTIFIER. "A publish" is a call to a method named
//     Publish; "the gate" is a call to a method named ConsumerActive. There is
//     no type resolution — type-checking the module would need go/packages on a
//     test that has to stay fast, and this module's guards are go/parser
//     throughout. A produce reached through a differently-named method, or
//     through a func field or callback this scan cannot follow by name, is
//     invisible to it. That direction fails OPEN, which is what the non-vacuity
//     floors below are for: they assert the scan still finds the publishes and
//     the gate it is supposed to find, so a rename breaks the guard loudly
//     instead of quietly emptying it.
//   - POSITIONAL, NOT A DOMINANCE PROOF. "The gate runs first" means: among the
//     top-level statements of the exported entrypoint, the first that reaches
//     the gate precedes the first that reaches a publish. It is not a
//     control-flow dominance proof — a real one needs SSA (golang.org/x/tools),
//     which this module does not depend on. A gate nested inside a conditional
//     that can be skipped at runtime would satisfy this check. The converse
//     error is conservative: a publish written textually before the gate but
//     only INVOKED after it (a closure built early, called late) is reported,
//     and would have to be restructured or exempted.
//   - IT PROVES THE GATE IS ON THE PATH, NOT THAT THE GATE IS RIGHT. That the
//     probe covers every archived subject rather than the first, that an
//     unanswered probe counts as LIVE rather than idle, and that an empty
//     subject list refuses rather than passing vacuously are semantic
//     properties. All three are proven by TestDrainProbesEverySubject,
//     TestDrainTreatsAnUnansweredProbeAsLive and
//     TestDrainRefusesWhenTheGateHasNothingToProbe in
//     services/archiver/internal/archive/drain_test.go. This guard cannot see
//     any of them and is not a substitute for them.
//   - IT PROVES NOTHING ABOUT THE ESTATE. Whether the archiver is actually
//     stopped is a runtime fact about a JetStream durable. The gate measures it;
//     this only proves the measurement happens before the writes.
//   - SCOPE is this package and the archiver-drain command. A second producer to
//     the archived topics written somewhere else in the module is out of scope
//     here.
func TestArchiverDrainCannotProduceWithoutTheSingleWriterGate(t *testing.T) {
	root := moduleRoot(t)
	pkg := parseDrainPackage(t, root, drainPackageDir)

	// (1) LOCATE THE GATED TYPE BY ITS PROBE, NOT BY ITS NAME. Finding the Drain
	// as "whatever type reaches ConsumerActive" survives a rename of the type and
	// of the gate method, and turns the deletion of the probe itself — the most
	// complete way to lose this invariant — into a failure of this step rather
	// than a vacuous pass over a package that no longer gates anything.
	gated := pkg.typesReaching(isConsumerActiveProbe)
	if len(gated) == 0 {
		t.Fatalf("no type in %s reaches a ConsumerActive probe.\n\n"+
			"That probe IS the single-writer gate (%s). Either it was deleted — in which case "+
			"archiver-drain can now produce to the archived topics beside a live archiver, and two "+
			"producers invert per-key order in Kafka — or it was renamed, and this guard must be "+
			"re-pointed deliberately rather than left passing over a package it no longer understands.",
			drainPackageDir, singleWriterCitation)
	}
	if len(gated) > 1 {
		t.Fatalf("%d types in %s reach a ConsumerActive probe (%s). This guard assumes one gated "+
			"producer; with two it cannot tell which entrypoints belong to which, and would check "+
			"the wrong bodies. Point it at the right type deliberately.",
			len(gated), drainPackageDir, strings.Join(gated, ", "))
	}
	typeName := gated[0]

	publishes := pkg.directSites(typeName, isPublishCall)
	probes := pkg.directSites(typeName, isConsumerActiveProbe)

	// NON-VACUITY. Both scans fail open — a publish the matcher stops recognising
	// simply is not reported — so a floor on each is what stands between a broken
	// matcher and a guard that protects nothing while reporting success. The drain
	// publishes in two places (the re-archive and the re-park) and probes in one.
	if len(publishes) < 2 {
		t.Fatalf("found only %d Publish call site(s) in %s's methods; there were 2 when this guard was "+
			"written (the re-archive in one() and the re-park in repark()). The publish matcher has "+
			"stopped working, and with it broken this guard passes no matter where the gate sits.",
			len(publishes), typeName)
	}
	if len(probes) < 1 {
		t.Fatalf("found no ConsumerActive call site in %s's methods even though it was selected as the "+
			"gated type. The site matcher and the reachability walk disagree — fix the scanner.", typeName)
	}

	// (2) EVERY EXPORTED DOOR THAT CAN REACH A PUBLISH MUST GATE FIRST. Checking
	// every exported method rather than Run by name is what keeps this correct
	// when someone adds a second entrypoint: a RunOnce that skipped the gate would
	// be reported here, where naming Run would have missed it entirely.
	var entrypoints []string
	for name, fd := range pkg.methods[typeName] {
		if !ast.IsExported(name) || fd.Body == nil {
			continue
		}
		if pkg.reaches(fd, isPublishCall) {
			entrypoints = append(entrypoints, name)
		}
	}
	sort.Strings(entrypoints)
	if len(entrypoints) == 0 {
		t.Fatalf("no exported method of %s reaches a Kafka publish, yet %d publish site(s) exist in its "+
			"methods. The reachability walk is broken — it can no longer follow a call from the "+
			"entrypoint to the produce, so the ordering check below has nothing to order.",
			typeName, len(publishes))
	}

	var violations []string
	for _, name := range entrypoints {
		if defect := pkg.gateDefect(pkg.methods[typeName][name]); defect != "" {
			violations = append(violations, fmt.Sprintf("%s.%s (%s): %s",
				typeName, name, pkg.where(pkg.methods[typeName][name].Pos()), defect))
		}
	}
	if len(violations) > 0 {
		var sites []string
		for _, p := range publishes {
			sites = append(sites, pkg.where(p))
		}
		t.Fatalf("%d exported entrypoint(s) of %s can reach a Kafka produce without the single-writer "+
			"gate having run first:\n  %s\n\nPublish sites reached: %s\n\n"+
			"%s\n\narchiver-drain re-produces to those same topics, so it is the second producer that "+
			"invariant forbids. The gate — the probe of the archiver's own durable — must be the first "+
			"thing the entrypoint does, and its refusal must return:\n\n"+
			"    func (d *Drain) Run(ctx context.Context) (DrainReport, error) {\n"+
			"        rep := DrainReport{DryRun: !d.cfg.Write}\n"+
			"        if err := d.assertArchiverStopped(ctx); err != nil {\n"+
			"            return rep, err\n"+
			"        }\n"+
			"        ...\n\n"+
			"If the entrypoint reported is Run, drain_test.go is failing too and either fix serves. If it "+
			"is any OTHER exported method, this is the only thing that noticed: those tests drive Run, "+
			"and a new door nothing calls leaves the whole services/archiver suite green.",
			len(violations), typeName, strings.Join(violations, "\n  "),
			strings.Join(sites, ", "), singleWriterCitation)
	}

	// (4) THE GATE MUST STILL BE ABLE TO REFUSE. A probe whose result is measured
	// and then not acted on runs, costs a round trip, and permits everything — and
	// it satisfies every check above, because the call is still on the path.
	prober := pkg.methodOwning(typeName, isConsumerActiveProbe)
	if prober == "" {
		t.Fatalf("the ConsumerActive probe is not written in exactly one method of %s. The gate was "+
			"split across several, or the scan can no longer attribute it — either way the refusal "+
			"check below has no single body to read, so re-point this guard deliberately.", typeName)
	}
	if !mentionsIdent(pkg.methods[typeName][prober].Body, refusalError) {
		t.Fatalf("%s.%s probes ConsumerActive but never names %s.\n\n"+
			"The probe is a measurement; %s is the refusal. A gate that measures and returns nil "+
			"regardless sits on the path, satisfies every ordering check above, and permits exactly "+
			"the concurrent produce it exists to stop.\n\n%s",
			typeName, prober, refusalError, refusalError, singleWriterCitation)
	}
}

// drainPackageDir is where the gated producer lives. It is a constant rather
// than a search because the guard's failure messages have to name a real place
// an operator can go; the TYPE inside it is still found by behaviour.
const drainPackageDir = "services/archiver/internal/archive"

// refusalError is the named error the gate returns. drain.go's own comment says
// why it is named at all: it is "the platform's most consequential refusal", and
// a named error is what stops each call site inventing its own wording for it.
const refusalError = "ErrArchiverLive"

const singleWriterCitation = "services/archiver/internal/archive/archiver.go:31 — \"SINGLE WRITER, " +
	"DELIBERATELY. One replica per stream (Recreate, not RollingUpdate). Two producers can invert " +
	"per-key order in Kafka, and a reordered log rebuilds a DIFFERENT book downstream — a subtler " +
	"failure than losing it.\""

// archiverDrainCommandPublishSites is a DEFAULT-DENY allow-list of Publish call
// sites in the archiver-drain command package, keyed "file:line" so a moved call
// un-certifies itself and must be re-reviewed.
//
// EMPTY, and the bar for an entry is high: the command's whole job is to hand
// its brokers to archive.NewDrain and print the report. A produce written in
// main.go would not merely reorder the gate — it would sit entirely outside it,
// where no amount of care inside Drain.Run can help.
var archiverDrainCommandPublishSites = map[string]string{}

// TestArchiverDrainCommandProducesOnlyThroughTheGatedDrain is the other half of
// the invariant, and it covers the hole the first test structurally cannot.
//
// That test proves the gate dominates every produce INSIDE the Drain. It says
// nothing about a produce written beside it. The command already holds a live
// *bus.KafkaClient — it dials one and passes it in as both Kafka and Reader — so
// "publish something here" is one line away at the exact site where the gate has
// no reach at all.
func TestArchiverDrainCommandProducesOnlyThroughTheGatedDrain(t *testing.T) {
	root := moduleRoot(t)
	const cmdDir = "services/archiver/cmd/archiver-drain"
	pkg := parseDrainPackage(t, root, cmdDir)

	// NON-VACUITY, and it is the important half here: the expected result is "no
	// publishes found", which is also exactly what a broken scan reports. Proving
	// the parse loaded a command that still builds its drain through NewDrain is
	// what separates the two.
	if len(pkg.files) == 0 {
		t.Fatalf("parsed no Go files in %s — the command moved or was removed, and this guard is "+
			"pointed at nothing while reporting success.", cmdDir)
	}
	if !pkg.callsNamed("NewDrain") {
		t.Fatalf("%s never calls NewDrain. Either the command no longer routes its writes through the "+
			"gated Drain — which is the defect this test exists to catch, now invisible to it — or the "+
			"constructor was renamed and this guard must be re-pointed deliberately.", cmdDir)
	}

	var (
		strays []string
		live   = map[string]bool{}
	)
	for _, pos := range pkg.allSites(isPublishCall) {
		site := pkg.where(pos)
		live[site] = true
		if _, ok := archiverDrainCommandPublishSites[site]; ok {
			continue
		}
		strays = append(strays, site)
	}

	// DEAD-ENTRY CHECK. An exemption naming a call site that no longer exists
	// reads as a reviewed decision while protecting nothing, and silently covers
	// the next produce that lands on that line.
	var dead []string
	for site := range archiverDrainCommandPublishSites {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("archiverDrainCommandPublishSites names %d call site(s) that no longer exist: %s\n\n"+
			"Either the call moved (update the file:line key) or it was removed (delete the entry). "+
			"A stale exemption cannot outlive the thing it excused.", len(dead), strings.Join(dead, ", "))
	}

	if len(strays) > 0 {
		sort.Strings(strays)
		t.Fatalf("%d Publish call(s) in %s sit outside the gated Drain:\n  %s\n\n"+
			"%s\n\nThe command holds a live Kafka client so it can hand it to archive.NewDrain. "+
			"Producing with it directly bypasses assertArchiverStopped entirely — not reordered, "+
			"absent — and no check inside Drain.Run can see it. Route the write through the Drain, "+
			"or add the site here with a reason and the issue that removes it.",
			len(strays), cmdDir, strings.Join(strays, "\n  "), singleWriterCitation)
	}
}

// TestDrainGateGuardSeparatesGatedFromUngated is the guard's own proof of work.
//
// The floors above show the scanners found SOMETHING in the real package. They
// do not show the analysis can tell a gated entrypoint from an ungated one, and
// that distinction is the whole guard — the two differ by the position of a
// single statement. Running both shapes, plus the two ways the gate degrades
// without moving (result discarded, refusal removed), through the same functions
// the real check uses means an edit that makes them permissive fails HERE
// instead of turning the real guard silently green.
func TestDrainGateGuardSeparatesGatedFromUngated(t *testing.T) {
	const header = `package archive

type Drain struct{ cfg struct{ Kafka publisher; Gate gate } }

type publisher interface{ Publish(ctx any, m any) error }
type gate interface{ ConsumerActive(ctx any, subject, group string) (bool, error) }

var ErrArchiverLive = newErr()

func newErr() error { return nil }

func (d *Drain) assertArchiverStopped(ctx any) error {
	active, err := d.cfg.Gate.ConsumerActive(ctx, "s", "g")
	if err != nil || active {
		return ErrArchiverLive
	}
	return nil
}

func (d *Drain) one(ctx any) error { return d.cfg.Kafka.Publish(ctx, nil) }

func (d *Drain) repark(ctx any) error { return d.cfg.Kafka.Publish(ctx, nil) }
`
	// Gated: the refusal is the first statement that does anything, and it
	// returns. This is the shape drain.go has.
	const gatedRun = header + `
func (d *Drain) Run(ctx any) error {
	if err := d.assertArchiverStopped(ctx); err != nil {
		return err
	}
	if err := d.one(ctx); err != nil {
		return err
	}
	return d.repark(ctx)
}
`
	// Reordered: the gate still runs, still refuses, and every unit test that
	// drives a live gate still fails the run — after it has already produced.
	const lateGate = header + `
func (d *Drain) Run(ctx any) error {
	if err := d.one(ctx); err != nil {
		return err
	}
	if err := d.assertArchiverStopped(ctx); err != nil {
		return err
	}
	return nil
}
`
	// Dropped: the gate method survives, so a name-based search for it still
	// finds one and a reviewer skimming the file still sees a gate.
	const noGate = header + `
func (d *Drain) Run(ctx any) error { return d.one(ctx) }
`
	// Discarded: the gate runs first and its refusal goes nowhere.
	const ignoredRefusal = header + `
func (d *Drain) Run(ctx any) error {
	d.assertArchiverStopped(ctx)
	return d.one(ctx)
}
`
	// Indirect: the produce is two hops away, through a package-level helper.
	// Following it is why the walk does not stop at the type's own methods.
	const indirectPublish = header + `
func produce(d *Drain, ctx any) error { return d.one(ctx) }

func (d *Drain) Run(ctx any) error {
	if err := produce(d, ctx); err != nil {
		return err
	}
	return d.assertArchiverStopped(ctx)
}
`

	parse := func(name, src string) *drainPkg {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return newDrainPkg(fset, "", []*ast.File{f})
	}

	cases := []struct {
		name     string
		src      string
		wantWord string // "" means the entrypoint must be clean
	}{
		{"gate first", gatedRun, ""},
		{"gate after the produce", lateGate, "before"},
		{"gate call dropped", noGate, "never reaches"},
		{"refusal discarded", ignoredRefusal, "discard"},
		{"produce through a package-level helper", indirectPublish, "before"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parse("drain.go", tc.src)
			if got := p.typesReaching(isConsumerActiveProbe); len(got) != 1 || got[0] != "Drain" {
				t.Fatalf("the gated type was located as %v, want [Drain] — the type scan cannot find "+
					"the thing the guard checks", got)
			}
			run := p.methods["Drain"]["Run"]
			if run == nil {
				t.Fatal("Run was not parsed as a method of Drain")
			}
			if !p.reaches(run, isPublishCall) {
				t.Fatal("Run was not seen to reach a Publish — the reachability walk missed the produce, " +
					"and with it broken every entrypoint reads as harmless")
			}
			defect := p.gateDefect(run)
			if tc.wantWord == "" {
				if defect != "" {
					t.Fatalf("a correctly gated Run was reported as defective: %q. The real guard would "+
						"fail on drain.go as written and the repair would be to weaken it.", defect)
				}
				return
			}
			if defect == "" {
				t.Fatalf("an ungated Run was accepted. This is the defect the guard exists for: %s", tc.name)
			}
			if !strings.Contains(defect, tc.wantWord) {
				t.Fatalf("Run was reported, but for the wrong reason: got %q, want it to mention %q",
					defect, tc.wantWord)
			}
		})
	}

	// A same-named method on an unrelated type must not satisfy the probe scan,
	// or the guard would find its gate somewhere it has no bearing.
	decoy := parse("decoy.go", `package archive

type Archiver struct{ kafka publisher }

type publisher interface{ Publish(ctx any, m any) error }

func (a *Archiver) Handle(ctx any) error { return a.kafka.Publish(ctx, nil) }
`)
	if got := decoy.typesReaching(isConsumerActiveProbe); len(got) != 0 {
		t.Errorf("a type that never probes ConsumerActive was selected as the gated producer: %v. "+
			"The archiver itself publishes to these topics and has no gate; mistaking it for the drain "+
			"would point the ordering check at a body where it means nothing", got)
	}
}

// drainPkg is one parsed package: its methods indexed by receiver type, its
// package-level functions, and the fileset that positions them.
//
// Build constraints are ignored and _test.go files are excluded. Test files
// carry fakes whose Publish methods would be counted as produces, and a gate
// behind a build tag is still the gate.
type drainPkg struct {
	fset    *token.FileSet
	root    string
	files   []*ast.File
	methods map[string]map[string]*ast.FuncDecl // receiver type -> method name -> decl
	funcs   map[string]*ast.FuncDecl
}

func parseDrainPackage(t *testing.T, root, dir string) *drainPkg {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(abs)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(abs, e.Name()), nil, parser.SkipObjectResolution)
		if perr != nil {
			// A file this cannot parse is a hole it cannot see through, and silence
			// here is how the produce that skips the gate hides.
			t.Fatalf("parse %s/%s: %v", dir, e.Name(), perr)
		}
		files = append(files, f)
	}
	return newDrainPkg(fset, root, files)
}

func newDrainPkg(fset *token.FileSet, root string, files []*ast.File) *drainPkg {
	p := &drainPkg{
		fset:    fset,
		root:    root,
		files:   files,
		methods: map[string]map[string]*ast.FuncDecl{},
		funcs:   map[string]*ast.FuncDecl{},
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if fd.Recv == nil {
				p.funcs[fd.Name.Name] = fd
				continue
			}
			typ := declaredOn(fd)
			if typ == "" {
				continue
			}
			if p.methods[typ] == nil {
				p.methods[typ] = map[string]*ast.FuncDecl{}
			}
			p.methods[typ][fd.Name.Name] = fd
		}
	}
	return p
}

func (p *drainPkg) where(pos token.Pos) string {
	position := p.fset.Position(pos)
	rel, err := filepath.Rel(p.root, position.Filename)
	if err != nil || p.root == "" {
		rel = position.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), position.Line)
}

// typesReaching returns every receiver type with at least one method that
// reaches want, sorted.
func (p *drainPkg) typesReaching(want func(*ast.SelectorExpr) bool) []string {
	var out []string
	for typ, methods := range p.methods {
		for _, fd := range methods {
			if p.reaches(fd, want) {
				out = append(out, typ)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// methodOwning returns the name of the single method of typ whose OWN body
// contains want — the probe itself rather than a caller of it. Empty when none
// or more than one does, which the caller must treat as a scanner failure rather
// than as a pass.
func (p *drainPkg) methodOwning(typ string, want func(*ast.SelectorExpr) bool) string {
	var found []string
	for name, fd := range p.methods[typ] {
		if len(sitesIn(fd.Body, want)) > 0 {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

// directSites returns the positions of every want-matching call written
// literally in one of typ's method bodies.
func (p *drainPkg) directSites(typ string, want func(*ast.SelectorExpr) bool) []token.Pos {
	var out []token.Pos
	for _, name := range sortedKeys(p.methods[typ]) {
		out = append(out, sitesIn(p.methods[typ][name].Body, want)...)
	}
	return out
}

// allSites returns the positions of every want-matching call anywhere in the
// package, methods and plain functions alike.
func (p *drainPkg) allSites(want func(*ast.SelectorExpr) bool) []token.Pos {
	var out []token.Pos
	for _, f := range p.files {
		out = append(out, sitesIn(f, want)...)
	}
	return out
}

// callsNamed reports whether the package calls a function or method with this
// name anywhere. Used only for non-vacuity, where a name match is enough.
func (p *drainPkg) callsNamed(name string) bool {
	for _, f := range p.files {
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			if found {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				found = fn.Name == name
			case *ast.SelectorExpr:
				found = fn.Sel.Name == name
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// reaches reports whether fd can arrive at a want-matching call, following calls
// on its own receiver and calls to package-level functions.
func (p *drainPkg) reaches(fd *ast.FuncDecl, want func(*ast.SelectorExpr) bool) bool {
	return p.nodeReaches(fd.Body, receiverName(fd), want, map[string]bool{})
}

// nodeReaches is the walk behind reaches, and behind the per-statement ordering
// check. recv is the enclosing function's receiver identifier, which is how
// `d.one(...)` is told apart from `d.cfg.Kafka.Publish(...)`: only the former has
// the receiver alone on the left of the selector.
//
// It descends into function literals, because the drain's produce happens inside
// the handler closure it hands to Subscribe — a walk that stopped at the closure
// boundary would report that Run reaches no publish at all.
func (p *drainPkg) nodeReaches(n ast.Node, recv string, want func(*ast.SelectorExpr) bool, seen map[string]bool) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if want(fn) {
				found = true
				return false
			}
			id, ok := fn.X.(*ast.Ident)
			if !ok || id.Name != recv || recv == "" {
				return true
			}
			key := "m:" + fn.Sel.Name
			if seen[key] {
				return true
			}
			for _, methods := range p.methods {
				if callee, ok := methods[fn.Sel.Name]; ok {
					seen[key] = true
					if p.nodeReaches(callee.Body, receiverName(callee), want, seen) {
						found = true
						return false
					}
				}
			}
		case *ast.Ident:
			key := "f:" + fn.Name
			callee, ok := p.funcs[fn.Name]
			if !ok || seen[key] {
				return true
			}
			seen[key] = true
			if p.nodeReaches(callee.Body, "", want, seen) {
				found = true
				return false
			}
			// A package-level helper handed the receiver publishes through its own
			// parameter name, so re-walk under every name that could be it. Cheap,
			// and the alternative is a hole exactly where a refactor would put one.
			for _, param := range paramNames(callee) {
				if p.nodeReaches(callee.Body, param, want, map[string]bool{}) {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// gateDefect returns "" when fd reaches the gate before it can reach a produce,
// and otherwise says what is wrong in words that name the consequence.
//
// The comparison is over fd's OWN top-level statements. Anything the guard
// cannot order this way — a produce and the gate in the same statement — is
// reported rather than assumed benign: the ordering is the invariant, and "could
// not establish it" is not the same fact as "it holds".
func (p *drainPkg) gateDefect(fd *ast.FuncDecl) string {
	recv := receiverName(fd)
	list := fd.Body.List
	gateIdx, pubIdx := -1, -1
	for i, stmt := range list {
		if gateIdx < 0 && p.nodeReaches(stmt, recv, isConsumerActiveProbe, map[string]bool{}) {
			gateIdx = i
		}
		if pubIdx < 0 && p.nodeReaches(stmt, recv, isPublishCall, map[string]bool{}) {
			pubIdx = i
		}
	}
	if gateIdx < 0 {
		return "never reaches the single-writer gate at all, but does reach a Kafka produce"
	}
	if pubIdx < 0 {
		return ""
	}
	if gateIdx == pubIdx {
		return fmt.Sprintf("reaches the gate and a produce from the same statement (%s), so which runs "+
			"first cannot be established here. Split them", p.where(list[gateIdx].Pos()))
	}
	if gateIdx > pubIdx {
		return fmt.Sprintf("reaches a Kafka produce at %s, before the gate at %s",
			p.where(list[pubIdx].Pos()), p.where(list[gateIdx].Pos()))
	}
	if !refusalIsActedOn(list, gateIdx) {
		return fmt.Sprintf("runs the gate at %s and discards its refusal — no bound error, no return. "+
			"The probe costs a round trip and permits everything", p.where(list[gateIdx].Pos()))
	}
	return ""
}

// refusalIsActedOn accepts the two spellings of "the gate's error stops the
// run": the canonical `if err := gate(); err != nil { return ... }`, and an
// assignment immediately followed by the test. Anything else — including a bare
// call whose error is dropped — is reported, and a third correct spelling would
// have to be added here deliberately. That cost is the point: this is the line
// between a gate and a gesture.
func refusalIsActedOn(list []ast.Stmt, gateIdx int) bool {
	switch stmt := list[gateIdx].(type) {
	case *ast.IfStmt:
		return containsReturn(stmt.Body) || (stmt.Else != nil && containsReturn(stmt.Else))
	case *ast.AssignStmt:
		for _, lhs := range stmt.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == "_" {
				return false
			}
		}
		if gateIdx+1 >= len(list) {
			return false
		}
		next, ok := list[gateIdx+1].(*ast.IfStmt)
		return ok && containsReturn(next.Body)
	default:
		return false
	}
}

func containsReturn(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		if _, ok := node.(*ast.ReturnStmt); ok {
			found = true
		}
		return !found
	})
	return found
}

// mentionsIdent reports whether n names ident anywhere — used to ask whether the
// probing method can still return the refusal.
func mentionsIdent(n ast.Node, ident string) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		if id, ok := node.(*ast.Ident); ok && id.Name == ident {
			found = true
		}
		return !found
	})
	return found
}

func sitesIn(n ast.Node, want func(*ast.SelectorExpr) bool) []token.Pos {
	var out []token.Pos
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && want(sel) {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// isPublishCall matches any call to a method named Publish. It is the transport
// verb on both bus.Publisher and the drain's own Publisher interface, and
// matching the NAME rather than a resolved type is what keeps this a go/parser
// check — at the cost stated in the limits above.
func isPublishCall(sel *ast.SelectorExpr) bool { return sel.Sel.Name == "Publish" }

// isConsumerActiveProbe matches the single-writer gate's measurement: the probe
// of whether the archiver still holds its durable.
func isConsumerActiveProbe(sel *ast.SelectorExpr) bool { return sel.Sel.Name == "ConsumerActive" }

// declaredOn returns the receiver TYPE of fd, reusing decimal_grpc_domain_test's
// receiverTypeName rather than spelling the StarExpr unwrap a second time.
func declaredOn(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	return receiverTypeName(fd.Recv.List[0].Type)
}

func receiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 || len(fd.Recv.List[0].Names) == 0 {
		return ""
	}
	return fd.Recv.List[0].Names[0].Name
}

func paramNames(fd *ast.FuncDecl) []string {
	var out []string
	if fd.Type == nil || fd.Type.Params == nil {
		return out
	}
	for _, field := range fd.Type.Params.List {
		for _, name := range field.Names {
			if name.Name != "_" {
				out = append(out, name.Name)
			}
		}
	}
	return out
}

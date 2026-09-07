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

// THE DLQ WIRE CONTRACT HAS ONE DECLARATION (#285).
//
// `Kanz-DLQ-Original-Subject` is the address a parked message is sent back to.
// `Kanz-DLQ-Class` decides whether it may be sent back at all. `Kanz-DLQ-Parked-At`
// is the age the min-age gate reads, and `Kanz-DLQ-Redrives` is the loop bound.
// Between them they are the entire recovery path for an event that failed —
// including capital-path orders.
//
// They diverged exactly the way AGENTS.md's `secret()` example predicts, and for
// the reason that makes this class of bug survive: NOTHING READ THEM. pkg/bus
// stamped `Kanz-DLQ-Original-Subject`; services/archiver stamped
// `Kanz-DLQ-Subject` for the same field, plus a `Kanz-DLQ-Reason` of its own, and
// omitted Parked-At, Class, Attempts and Redrives entirely. Two names for one
// field cannot disagree until something keys on one of them — #220 built that
// something, and #285 found the mismatch waiting for it.
//
// The failure mode is the quiet one. A drain keyed on a header the parker stopped
// writing does not error; it matches nothing, reports "0 messages redriven", and
// is indistinguishable from a DLQ that is legitimately empty.
//
// WHAT THIS GUARD FORBIDS, outside pkg/bus, in non-test Go:
//
//	ARM A — a string literal beginning "Kanz-DLQ-". Checked on the AST, so the
//	        several comments that name these headers (deliberately — the divergence
//	        is documented where it happened) are not violations; only code is.
//	ARM B — writing a bare string literal into HeaderDLQClass instead of
//	        bus.ClassTerminal / bus.ClassTransient. That is "adopted the name,
//	        re-derived the vocabulary": the header would be spelled correctly and
//	        the VALUE would not, so PlanRedrive's `class == ClassTerminal` test
//	        silently stops recognising a poison message as poison and replays it
//	        onto a live subject.
//
// ARM B is a regex where ARM A is an AST walk, and the split is the same one
// principal_header_test.go makes. ARM A MUST not fire on prose — several comments
// name these headers on purpose, including the one recording the divergence — so
// it needs the parser. ARM B could not use the AST without re-implementing type
// resolution to prove which map is being indexed, so it reads the assignment shape
// textually. That means a comment reproducing `HeaderDLQClass: "` verbatim would
// false-positive; that is the acceptable direction for a guard on this field, and
// the fix is to reword the comment.
//
// _test.go IS OUT OF SCOPE. Tests assert the literal wire names on purpose —
// pkg/bus/consumer_test.go, dlq_integration_test.go and the archiver's own tests
// all do, and that is the point: a test that spells the header out is what catches
// a constant being renamed out from under the wire. A re-declaration hidden in a
// _test.go would escape this guard, but it could not be imported by production
// code either.

// dlqHeaderPrefix is the wire prefix every DLQ metadata header shares. Matching
// the PREFIX rather than the six exact names is deliberate: a seventh header must
// be born in pkg/bus beside the others, not discovered later in a service that
// parks its own way.
const dlqHeaderPrefix = "Kanz-DLQ-"

// dlqHeaderHome is the one package allowed to declare them, module-relative with
// forward slashes.
const dlqHeaderHome = "pkg/bus"

// dlqHeaderParker is the one non-test file outside pkg/bus that parks messages of
// its own, and therefore the reason this guard exists. Named rather than counted:
// there is exactly one, so a count floor of ">= 1" would be satisfied by any
// passing mention, while what must stay true is that THIS park still carries the
// canonical set. If the archiver moves, update this path — do not delete the
// check.
const dlqHeaderParker = "services/archiver/internal/archive/archiver.go"

// dlqHeaderExempt maps a module-relative path (forward slashes) to the reason it
// may declare a Kanz-DLQ-* name of its own, and the issue that retires the entry.
//
// IT IS EMPTY, AND THAT IS THE POINT. The archiver was migrated in #285 rather
// than grandfathered: an exemption here is an exemption on the only route by which
// a parked order gets back onto the spine. The map and the dead-entry arm below
// exist so a future exemption has to be written down with a reason and an issue,
// in a diff a reviewer sees — not so that one is expected.
var dlqHeaderExempt = map[string]string{}

// dlqClassLiteral matches a bare string literal being written into the class
// header — `Headers[bus.HeaderDLQClass] = "terminal"`, in either map-literal or
// assignment form. The constants exist so the parker and PlanRedrive cannot
// disagree about the two values without a compile error.
var dlqClassLiteral = regexp.MustCompile(`[A-Za-z0-9_.]*HeaderDLQClass\]?\s*[:=]=?\s*"`)

// dlqSharedConstant matches a reference to the shared contract — the shape a
// compliant parker has. Used only by the non-vacuity floor.
var dlqSharedConstant = regexp.MustCompile(`bus\.(?:HeaderDLQ[A-Za-z]*|ClassTerminal|ClassTransient)`)

func TestDLQHeadersLiveOnlyInPkgBus(t *testing.T) {
	root := moduleRoot(t)

	var (
		violations []string
		consumers  []string
		scanned    int
		homeDecls  = map[string]bool{}
		parkerBody string
	)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "vendor", "testdata", "gen":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(os.PathSeparator)))
		scanned++

		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", rel, rerr)
		}
		body := string(src)
		if rel == dlqHeaderParker {
			parkerBody = body
		}
		inHome := strings.HasPrefix(rel, dlqHeaderHome+"/")

		// Parse without comments: ARM A must not fire on prose.
		f, perr := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		var literals []string
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !strings.HasPrefix(v, dlqHeaderPrefix) {
				return true
			}
			literals = append(literals, v)
			return true
		})

		if inHome {
			for _, v := range literals {
				homeDecls[v] = true
			}
			return nil
		}

		if _, exempt := dlqHeaderExempt[rel]; exempt {
			return nil
		}
		if len(literals) > 0 {
			sort.Strings(literals)
			violations = append(violations, rel+" declares "+strings.Join(uniq(literals), ", "))
		}
		if m := dlqClassLiteral.FindString(body); m != "" {
			violations = append(violations, rel+" writes a bare class value: "+strings.TrimSpace(m)+`…"`)
		}
		if dlqSharedConstant.MatchString(body) {
			consumers = append(consumers, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// NON-VACUITY 1 — the walk ran. The module has ~560 non-test .go files; a scan
	// that sees a handful is a broken path, and this guard would pass on a
	// repository full of copies.
	if scanned < 400 {
		t.Fatalf("scanned only %d non-test .go files under %s — expected at least 400. "+
			"The walk is broken and this guard is asserting nothing", scanned, root)
	}

	// NON-VACUITY 2 — the shared home actually declares the contract. If pkg/bus
	// stops declaring these, ARM A passes everywhere for the worst reason: the
	// concept was renamed and every parker is free again.
	for _, want := range []string{
		dlqHeaderPrefix + "Original-Subject",
		dlqHeaderPrefix + "Attempts",
		dlqHeaderPrefix + "Error",
		dlqHeaderPrefix + "Parked-At",
		dlqHeaderPrefix + "Class",
		dlqHeaderPrefix + "Redrives",
	} {
		if !homeDecls[want] {
			t.Fatalf("%s does not declare %q. The shared home is the whole premise of this guard: "+
				"without it there is nothing for a parker to import, and forbidding the local "+
				"declaration would just forbid the header. Restore it in pkg/bus/dlq.go.",
				dlqHeaderHome, want)
		}
	}

	// NON-VACUITY 3 — the one parker outside pkg/bus still routes through it. The
	// archiver is why #285 exists; if its park stops naming these constants, ARM A
	// passes because it went back to spelling something else, or stopped recording
	// what a drain needs at all.
	if parkerBody == "" {
		t.Fatalf("%s was not scanned. It is the only non-test parker outside %s and the reason "+
			"this guard exists; if it moved, point dlqHeaderParker at its new path rather than "+
			"letting the check go quiet", dlqHeaderParker, dlqHeaderHome)
	}
	for _, want := range []string{
		"bus.HeaderDLQOriginalSubject",
		"bus.HeaderDLQParkedAt",
		"bus.HeaderDLQClass",
		"bus.ClassTerminal",
	} {
		if !strings.Contains(parkerBody, want) {
			t.Errorf("%s no longer references %s. Its park is the only one outside pkg/bus, and "+
				"without the original subject a drain has no address, without Parked-At it cannot "+
				"apply a min-age gate, and without an explicit class an undecodable envelope reads "+
				"as redrivable (#285)", dlqHeaderParker, want)
		}
	}
	if len(consumers) < 1 {
		sort.Strings(consumers)
		t.Fatalf("no non-test file outside %s references the shared DLQ constants. Either every "+
			"parker was removed or they all went back to local literals in a form this guard does "+
			"not see; either way it is no longer watching the recovery path", dlqHeaderHome)
	}

	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("these non-test files declare a %s* header, or write a bare class value, outside %s:\n  %s\n\n"+
			"These headers ARE the recovery path for a failed event, including a capital-path order, and "+
			"a local copy is how a drain silently stops finding messages — it was two names for the "+
			"originating subject before #285, and neither side noticed because nothing read either one.\n\n"+
			"Use pkg/bus instead:\n"+
			"  bus.HeaderDLQOriginalSubject — where a redrive sends it back; without it the redrive REFUSES.\n"+
			"  bus.HeaderDLQParkedAt        — RFC3339Nano UTC; the age RedriveOptions.MinAge gates on.\n"+
			"  bus.HeaderDLQClass           — bus.ClassTerminal or bus.ClassTransient, never a bare string.\n"+
			"  bus.HeaderDLQError / bus.HeaderDLQAttempts / bus.HeaderDLQRedrives — cause, dispatch count, loop bound.\n\n"+
			"If a genuinely new field is needed, add it to pkg/bus/dlq.go beside the others so the parker and "+
			"the drain share one declaration. If it truly cannot live there, add the path to dlqHeaderExempt "+
			"with the reason and the issue that retires it.",
			dlqHeaderPrefix, dlqHeaderHome, strings.Join(violations, "\n  "))
	}

	// DEAD ENTRIES: an exemption for a file that no longer exists, or that no
	// longer spells a DLQ header of its own, is stale permission on the recovery
	// path.
	var dead []string
	for rel := range dlqHeaderExempt {
		path := filepath.Join(root, filepath.FromSlash(rel))
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			dead = append(dead, rel+" (no such file)")
			continue
		}
		body := string(src)
		f, perr := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if perr != nil {
			dead = append(dead, rel+" (does not parse)")
			continue
		}
		touches := dlqClassLiteral.MatchString(body)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, uerr := strconv.Unquote(lit.Value); uerr == nil && strings.HasPrefix(v, dlqHeaderPrefix) {
				touches = true
			}
			return true
		})
		if !touches {
			dead = append(dead, rel+" (no longer declares a DLQ header or a bare class value — exemption outlived the repair)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("dlqHeaderExempt has %d stale entr(y/ies):\n  %s\n\n"+
			"An exemption on the DLQ wire contract must not outlive the thing it excused.",
			len(dead), strings.Join(dead, "\n  "))
	}
}

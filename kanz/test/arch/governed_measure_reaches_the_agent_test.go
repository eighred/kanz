package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A RECORD THE CALLER NEVER RECEIVES IS NOT A RECORD — ON THE LAST HOP (#757).
//
// # What already existed, and where it stopped
//
// measure_coverage_reaches_the_caller_test.go holds the producing half of this
// rule: every exported field of internal/risk/api/v1.Measure must be READ inside
// internal/risk/publish.ToProtoMeasureSet, so the InputCoverage #527 created
// cannot die between the engine's domain model and the wire.
//
// It stops at the wire, and it was green for the defect this guard was written
// for. services/mcp and services/copilot both read query.v1 and re-projected it
// back DOWN to map[string]float64 with dec.Float64Or(value, 0) — dropping the
// per-measure coverage, the response's quality flags, and the as-of stamp on the
// hop between the wire and an agent. The producing guard cannot see a consumer;
// a per-message test cannot see a gap between two mappings.
//
// # Why these planes and not every caller
//
// A gate that reads a risk number and refuses an order is welcome to its own
// arithmetic — it acts on the number and never restates it. An AGENT plane hands
// the number to a language model, which renders it as prose and drops caveats.
// So the scope is derived rather than listed: a service is agent-facing when
// SOME package under it imports internal/agentgate, which is this estate's
// definition of "a surface an agent invokes tools on". A third agent plane comes
// under this guard by existing, without anyone remembering to add it here.
//
// # What this checks
//
//  1. No package under an agent-facing service may CALL dec.Float64Or. That is
//     the estate's own named marker for a confident zero (dec.Float64's own doc
//     names the four issues it caused), and calling it on a governed value is
//     precisely how the record was dropped.
//  2. internal/measureread — the one place the "may this number be stated"
//     decision lives — must READ the fields the decision depends on. Without this
//     half, deleting the projection's body would satisfy (1) perfectly.
//
// Both halves are parsed as Go, NOT grepped. A guard that matched raw source
// would match its own prose above, and the two package comments that quote
// `dec.Float64Or(value, 0)` to explain what they stopped doing would satisfy or
// fail it for no reason at all.
const (
	agentGateImport   = "github.com/eighred/kanz/internal/agentgate"
	confidentZeroFunc = "Float64Or"
	projectionPkgDir  = "internal/measureread"
)

// projectionMustRead names the accessor each projection decision depends on, and
// what is lost if it goes. A field read nowhere in this package cannot reach an
// agent by any route, which is the same argument
// measure_coverage_reaches_the_caller_test.go makes one hop in.
var projectionMustRead = map[string]string{
	"GetCoverage":      "the per-measure InputCoverage (#527) — without it a zero computed over nothing is indistinguishable from a measured zero",
	"GetExcludedCount": "how many positions could not be assessed — the number that turns a value into a refusal",
	"GetQualityFlags":  "the response's trust signals — CURRENCY_EXCLUDED and an unmappable flag both have to fail closed",
	"GetAsOf":          "the domain time the state is effective at — without it an agent cannot tell current state from state folded hours ago",
	"GetUncertaintyAbs": "the propagated one-sigma band — absent means none was propagated, which is not the same as zero " +
		"uncertainty",
}

// confidentZeroExempt names a call site under an agent-facing service permitted
// to call dec.Float64Or, and the issue that retires the entry.
//
// EMPTY, AND THAT IS THE POINT. An agent plane has no legitimate use for a
// silent numeric fallback: every value it handles is about to be stated to a
// human through a model. An entry here is a decision that some number an agent
// reads may be substituted rather than refused, and that has to be argued in
// writing before it is true in code.
var confidentZeroExempt = map[string]string{}

func TestGovernedMeasureRecordReachesTheAgent(t *testing.T) {
	root := moduleRoot(t)
	planes := agentFacingServices(t, root)

	// NON-VACUITY, first half. Zero planes means the derivation broke — the
	// agentgate import path changed, or the walk did — and the guard would then
	// report a clean estate having inspected nothing.
	if len(planes) < 2 {
		t.Fatalf("found %d agent-facing service(s) (%v) — services/mcp and services/copilot both "+
			"import %s, so the derivation is broken rather than the estate", len(planes), planes, agentGateImport)
	}

	var problems []string
	seenExempt := map[string]bool{}
	for _, plane := range planes {
		for _, f := range goFilesUnder(t, filepath.Join(root, filepath.FromSlash(plane))) {
			rel := plane + "/" + f.rel
			if !callsQualifiedFunc(t, rel, f.body, confidentZeroFunc) {
				continue
			}
			if reason, ok := confidentZeroExempt[rel]; ok {
				seenExempt[rel] = true
				t.Logf("%s: calls dec.%s, tracked — %s", rel, confidentZeroFunc, reason)
				continue
			}
			problems = append(problems, rel+" calls dec."+confidentZeroFunc+
				", substituting a fallback for a value an agent will state as fact")
		}
	}

	// DEAD-ENTRY CHECK: an exemption that no longer matches anything has outlived
	// its repair and must not sit here implying a hole that was already closed.
	for rel := range confidentZeroExempt {
		if !seenExempt[rel] {
			problems = append(problems, "exemption for "+rel+" matches nothing — the call is gone, so delete the entry")
		}
	}

	for _, p := range problems {
		t.Error(p)
	}
	if len(problems) > 0 {
		t.Logf("the shared projection is %s: it decides whether a governed value may be stated at "+
			"all, and returns no number when it may not", projectionPkgDir)
	}
}

// TestTheProjectionReadsWhatTheDecisionDependsOn is the second half, and without
// it the first is satisfied by a projection that reads nothing.
func TestTheProjectionReadsWhatTheDecisionDependsOn(t *testing.T) {
	root := moduleRoot(t)
	read := map[string]bool{}
	files := goFilesUnder(t, filepath.Join(root, filepath.FromSlash(projectionPkgDir)))

	// NON-VACUITY: a package the walk did not find would report every accessor
	// missing, which would look like a catastrophic regression rather than a
	// broken test.
	if len(files) == 0 {
		t.Fatalf("no Go files under %s — the projection was moved or renamed, and this guard is "+
			"now checking an empty directory", projectionPkgDir)
	}
	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		for _, sel := range selectorsCalled(t, projectionPkgDir+"/"+f.rel, f.body) {
			read[sel] = true
		}
	}

	names := make([]string, 0, len(projectionMustRead))
	for n := range projectionMustRead {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !read[n] {
			t.Errorf("%s never calls %s — %s", projectionPkgDir, n, projectionMustRead[n])
		}
	}
}

// agentFacingServices returns the module-relative service directories with at
// least one package importing internal/agentgate, sorted.
func agentFacingServices(t *testing.T, root string) []string {
	t.Helper()
	found := map[string]bool{}
	for _, f := range goFilesUnder(t, filepath.Join(root, "services")) {
		if !importsPath(t, "services/"+f.rel, f.body, agentGateImport) {
			continue
		}
		// "copilot/internal/tools/tools.go" -> "services/copilot"
		parts := strings.SplitN(f.rel, "/", 2)
		if len(parts) < 2 {
			continue
		}
		found["services/"+parts[0]] = true
	}
	out := make([]string, 0, len(found))
	for s := range found {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// importsPath reports whether the file imports path, read from the AST rather
// than matched in the text, so a mention in a comment is not an import.
func importsPath(t *testing.T, rel, src, path string) bool {
	t.Helper()
	file := parseFile(t, rel, src)
	if file == nil {
		return false
	}
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) == path {
			return true
		}
	}
	return false
}

// callsQualifiedFunc reports whether the file CALLS a function of this name through any
// package qualifier — `dec.Float64Or(...)`, `decutil.Float64Or(...)`. It matches
// a call expression, so neither the prose above nor a package comment quoting
// the name can satisfy or trip it.
func callsQualifiedFunc(t *testing.T, rel, src, name string) bool {
	t.Helper()
	for _, sel := range selectorsCalled(t, rel, src) {
		if sel == name {
			return true
		}
	}
	return false
}

// selectorsCalled returns the names of every qualified function CALLED in the
// file — the Sel of a SelectorExpr in call position.
func selectorsCalled(t *testing.T, rel, src string) []string {
	t.Helper()
	file := parseFile(t, rel, src)
	if file == nil {
		return nil
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			out = append(out, sel.Sel.Name)
		}
		return true
	})
	return out
}

func parseFile(t *testing.T, rel, src string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Base(rel), src, parser.SkipObjectResolution)
	if err != nil {
		// A file this walk cannot parse is a file this guard cannot vouch for,
		// and silently skipping it is how a guard reports a clean estate over
		// source it never read.
		t.Fatalf("parse %s: %v", rel, err)
	}
	return file
}

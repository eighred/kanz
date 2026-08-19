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

// A RECORD THE CALLER NEVER RECEIVES IS NOT A RECORD (#509, #527).
//
// # The failure this was written for
//
// #527 gave every measure family an InputCoverage: how many positions reached
// the arithmetic, how many could not be assessed, and a bounded sample of which.
// measure_carries_its_coverage_test.go holds the producing half — no v1.Measure
// literal in internal/risk/compute may omit it.
//
// The record then stopped at the process boundary. ToProtoMeasureSet — the ONE
// domain→proto mapping, shared by the RISK-10 publisher and the gRPC query
// server so the bus and the query surface cannot diverge — built a
// domain.v1.RiskMeasure from Name, Value, UncertaintyAbs and SourceEventIds, and
// domain.v1.RiskMeasure had no field for coverage at all. Every consumer
// therefore received the number and not the fact that it was computed over
// nothing.
//
// That is not theoretical on this estate. The FI family is registered in
// production and the contract-terms store has no production writer, so DV01,
// Duration, Convexity and SpreadDuration are zeros over zero bonds today. The
// caller had one set-level flag: it could learn that SOMETHING in the response
// was unresolved, never which measure — so refusing the DV01 meant refusing the
// GrossExposure beside it, and in practice meant refusing nothing.
//
// Two guards already existed and neither could see it. The producing guard
// checks the compute package, which was correct. The quality-flag guards check
// that a FLAG survives translation, and the flag did. The gap was a FIELD of a
// Go struct with no counterpart in the mapping — precisely the shape
// submit_fields_reach_state_test.go names for order.v1.SubmitOrder: "the defect
// lives in the GAP BETWEEN two messages, which is exactly what a per-message
// test cannot see."
//
// # What this checks
//
// Every exported field of internal/risk/api/v1.Measure is READ inside
// ToProtoMeasureSet. Reading it is stronger than the counterpart-exists check
// submit_fields_reach_state_test.go performs and explicitly cannot: a proto
// field that exists and is never assigned is invisible, whereas a Go field that
// is never read cannot reach the wire by any route.
//
// It also subsumes the existence half, because the assignment is Go code: a
// mapping that reads m.Coverage has to put it somewhere, and there is nowhere to
// put it unless domain.v1.RiskMeasure declares the field.
//
// # The known weakness, stated rather than discovered later
//
// A field is counted as read when a selector of that name appears on any plain
// identifier inside the function — `m.Coverage`, not resolved through types. A
// same-named selector on an unrelated local would satisfy it. The function is
// twenty lines with one struct literal, so the confusion is remote; the
// alternative is a type-checking pass over a package test/arch is forbidden from
// importing (risk_boundary_test.go), to close a hole nothing has fallen into.
//
// It also cannot check that the field is read into the RIGHT proto field. That
// half is behavioural and lives where behaviour belongs: input_coverage_test.go
// in internal/risk/publish and TestMeasuresCarryTheirCoverageToTheCaller in
// services/risk-engine/internal/grpcsrv.
const (
	measureFieldsSourceFile = "internal/risk/api/v1/engine.go"
	measureFieldsStructName = "Measure"
	measureFieldsMapFile    = "internal/risk/publish/publish.go"
	measureFieldsMapFunc    = "ToProtoMeasureSet"
)

// measureFieldExempt maps a v1.Measure field to the reason the wire mapping does
// not carry it, and the issue that retires the entry.
//
// EMPTY, AND THAT IS THE POINT. A measure has four fields and a caller acting on
// the number needs all four. An entry here is a decision that some part of what
// the engine knows about a value stops inside the engine, which has to be argued
// in writing before it is true in code.
var measureFieldExempt = map[string]string{}

func TestEveryMeasureFieldReachesTheCaller(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	fields := measureStructFields(t, fset, filepath.Join(root, filepath.FromSlash(measureFieldsSourceFile)))
	// NON-VACUITY, first half. A struct the parser did not find, or found empty,
	// would report nothing missing having checked nothing — which is this guard's
	// own failure mode.
	if len(fields) < 4 {
		t.Fatalf("found %d exported fields on v1.%s (%v) — the parse is broken, not the estate; "+
			"Name, Value, UncertaintyAbs and Coverage are all declared there",
			len(fields), measureFieldsStructName, fields)
	}

	read := selectorsReadIn(t, fset, filepath.Join(root, filepath.FromSlash(measureFieldsMapFile)), measureFieldsMapFunc)
	// NON-VACUITY, second half. An empty read-set means the function was renamed
	// or the walk broke, and every field would then look dropped — which would
	// grow the exemption map to cover a bug in this file.
	if len(read) == 0 {
		t.Fatalf("%s reads no fields at all in %s — it was renamed, moved, or the walk is broken",
			measureFieldsMapFunc, measureFieldsMapFile)
	}

	var dropped []string
	seenExempt := map[string]bool{}
	for _, f := range fields {
		if read[f] {
			continue
		}
		if reason, ok := measureFieldExempt[f]; ok {
			seenExempt[f] = true
			t.Logf("%s: not carried, tracked — %s", f, reason)
			continue
		}
		dropped = append(dropped, f)
	}

	if len(dropped) > 0 {
		sort.Strings(dropped)
		t.Errorf("v1.%s field(s) %v are never read by %s.\n\n"+
			"That function is the ONLY domain→proto mapping for risk measures — the RISK-10 "+
			"publisher and the gRPC query server both go through it — so a field it does not "+
			"read reaches no consumer of either.\n\n"+
			"Coverage is the one this guard was written for: without it a caller receives a DV01 "+
			"of zero and cannot tell it from a book that holds no bonds, which is what #527 "+
			"existed to prevent and what stopping at the process boundary undid (#509).\n\n"+
			"Carry it, adding the field to domain.v1.RiskMeasure if it has none — or add an entry "+
			"to measureFieldExempt naming the issue and the argument for why this part of what "+
			"the engine knows stops inside the engine.",
			measureFieldsStructName, dropped, measureFieldsMapFunc)
	}

	// DEAD-ENTRY ARM. An exemption for a field that is now carried, or that no
	// longer exists, is a standing licence the next reader takes as a decision.
	for name, reason := range measureFieldExempt {
		if seenExempt[name] {
			continue
		}
		t.Errorf("measureFieldExempt names %q, which is now carried or no longer declared on "+
			"v1.%s — delete the entry (%s)", name, measureFieldsStructName, reason)
	}
}

// measureStructFields returns the exported field names of the named struct.
func measureStructFields(t *testing.T, fset *token.FileSet, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != measureFieldsStructName {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			return false
		}
		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				if name.IsExported() {
					out = append(out, name.Name)
				}
			}
		}
		return false
	})
	sort.Strings(out)
	return out
}

// selectorsReadIn returns the selector names appearing on a plain identifier
// inside the named function — `m.Coverage` contributes "Coverage".
func selectorsReadIn(t *testing.T, fset *token.FileSet, path, fn string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]bool{}
	found := false
	for _, decl := range f.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Name.Name != fn || d.Body == nil {
			continue
		}
		found = true
		ast.Inspect(d.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, ok := sel.X.(*ast.Ident); ok {
				out[sel.Sel.Name] = true
			}
			return true
		})
	}
	if !found {
		t.Fatalf("%s declares no function %s — it was renamed or moved, and this guard cannot "+
			"check a mapping it cannot find", strings.TrimPrefix(path, filepath.Dir(path)), fn)
	}
	return out
}

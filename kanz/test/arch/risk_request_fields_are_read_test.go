package arch

import (
	"go/ast"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY FIELD ON A RISK REQUEST MUST BE READ BY THE ENGINE THAT ANSWERS IT (#859).
//
// # The defect this exists to end
//
// `query.v1.ExposureRequest.as_of` and `MeasuresRequest.as_of` were declared,
// documented as working, parsed and validated by the gateway, forwarded through
// the gRPC server into `internal/risk/api/v1` — and then dropped on the floor.
// `EngineImpl.Exposure` and `.Measures` never read `req.AsOf`; both reached
// `store.Snapshot(id)`, the LIVE portfolio.
//
// So a caller asking "what were this portfolio's exposures as of last Tuesday"
// was answered with today's — and the response carried an `as_of` of *today's*
// state timestamp, so the answer looked internally consistent. A reconciliation,
// a regulatory as-of report or an investigation that pinned a date got the
// current book with a plausible timestamp attached. Nothing logged, nothing
// counted, nothing degraded.
//
// That is the platform's own rule — "nothing configured" and "checked, and fine"
// must never look the same — violated on a RISK ANSWER. A silently ignored
// request field is worse than an unsupported one, because the caller cannot tell.
//
// # What this checks, and why that is the right property
//
// Default-deny over the fields themselves: every exported field on every
// `*Request` type in `internal/risk/api/v1` must appear as a selector read on
// the corresponding parameter inside the engine method that takes it.
//
// It deliberately does NOT check what the engine DOES with the field. Honouring
// as_of and refusing it are both correct resolutions of #859 and the repository
// chose the refusal; a guard encoding either choice would have to be rewritten
// when the other lands, and a guard that must change to allow a correct fix is
// one that gets deleted rather than updated. What may never happen again is the
// third state: accepted, forwarded, and neither honoured nor refused. Reading
// the field is the floor both real answers clear and the silent one cannot.
//
// THE GUARD IS NOT THE WHOLE PROOF, AND IT IS NOT MEANT TO BE. `_ = req.AsOf`
// would satisfy it. The behaviour — INVALID_ARGUMENT naming the field — is held
// by TestExposureRefusesAnAsOfItCannotHonour and its Measures twin in
// internal/risk/engine. This guard's job is the structural half: it fires when a
// NEW field is added and forgotten, which is the failure mode that produced
// #859 and which no behavioural test can anticipate.
//
// # No exemption list, deliberately
//
// Surveyed before it was written: three request types, seven exported fields,
// and exactly two unread — the two `AsOf` fields #859 is about. So this lands
// with an empty allow-list rather than a set of grandfathered entries, and the
// first entry anyone needs to add is a conversation rather than a formality.
func TestEveryRiskRequestFieldIsReadByTheEngine(t *testing.T) {
	root := moduleRoot(t)
	apiDir := filepath.Join(root, "internal", "risk", "api", "v1")
	engineDir := filepath.Join(root, "internal", "risk", "engine")

	declared := riskRequestFields(t, apiDir)

	// NON-VACUITY (1/3): the request types must be found. A scanner that reads
	// none of them passes every assertion below while proving nothing — the guard
	// would be broken and green, which is the state #859 is a case study in.
	if len(declared) < 3 {
		t.Fatalf("found %d *Request types in internal/risk/api/v1, want at least 3 "+
			"(Exposure, Measures, Scenario). The extractor is broken or the types moved; "+
			"either way this guard is asserting nothing", len(declared))
	}
	totalFields := 0
	for _, fields := range declared {
		totalFields += len(fields)
	}
	// NON-VACUITY (2/3): request types with no fields would make the field loop a
	// no-op for every one of them.
	if totalFields < 5 {
		t.Fatalf("found %d exported fields across %d request types — too few to be the real "+
			"surface; the struct-field extractor is broken", totalFields, len(declared))
	}

	read, opaque := engineRequestReads(t, engineDir)

	// A method that hands the whole request to a helper defeats a selector scan:
	// the fields would be read out of this guard's sight and every one of them
	// would look ignored. Fail LOUDLY rather than either passing blindly or
	// reporting a field as unread when it is not — the guard's own blind spot is
	// the one thing it cannot detect about itself.
	if len(opaque) > 0 {
		sort.Strings(opaque)
		t.Fatalf("these engine methods pass the whole request onward, so this guard cannot see "+
			"which fields are read:\n\n  %s\n\nExtend the scan to follow the callee, or read the "+
			"fields at the method that owns the contract. Do not delete the check: a field read "+
			"only inside an unfollowed helper is indistinguishable from one nobody reads, which "+
			"is exactly the state #859 was.", strings.Join(opaque, "\n  "))
	}

	// NON-VACUITY (3/3): every declared request type must actually be taken by an
	// engine method. A type nobody answers would otherwise be checked against an
	// empty read set and produce a confusing failure, or — worse, if the map
	// lookup were lenient — none at all.
	var unanswered []string
	for reqType := range declared {
		if _, ok := read[reqType]; !ok {
			unanswered = append(unanswered, reqType)
		}
	}
	if len(unanswered) > 0 {
		sort.Strings(unanswered)
		t.Fatalf("no method in internal/risk/engine takes these request types: %s\n\n"+
			"Either the engine stopped implementing v1.Engine, or the parameter-type matcher "+
			"broke. Both make this guard blind.", strings.Join(unanswered, ", "))
	}

	var ignored []string
	for reqType, fields := range declared {
		for _, field := range fields {
			if !read[reqType][field] {
				ignored = append(ignored, reqType+"."+field)
			}
		}
	}
	if len(ignored) > 0 {
		sort.Strings(ignored)
		t.Fatalf("these risk request fields are DECLARED and never read by the engine that "+
			"answers them:\n\n  %s\n\n"+
			"A field the engine cannot see is a promise the API makes and the engine does not "+
			"keep. The caller is given no way to tell: the gateway parses it, the gRPC server "+
			"forwards it, and the answer comes back computed against something else entirely, "+
			"carrying a response field that makes it look consistent (#859).\n\n"+
			"Two honest resolutions, and this guard accepts either:\n"+
			"  - HONOUR it — read the field and compute what it asks for;\n"+
			"  - REFUSE it — read the field and return ErrInvalidRequest naming it, so every "+
			"caller learns immediately that it is not yet supported.\n\n"+
			"What is not acceptable is the third state, which is what this catches.",
			strings.Join(ignored, "\n  "))
	}

	t.Logf("%d request type(s), %d exported field(s), all read by internal/risk/engine",
		len(declared), totalFields)
}

// riskRequestFields returns the exported fields of every `*Request` struct
// declared in the risk api package, keyed by type name.
//
// The AST rather than a regex, and not as a style preference: this package's doc
// comments discuss `req.AsOf`, `as_of` and the field names at length, and a
// source-text scan matches its own prose. That has already produced three guards
// in this repository that passed with the checked thing deleted.
func riskRequestFields(t *testing.T, dir string) map[string][]string {
	t.Helper()

	out := map[string][]string{}
	for _, file := range parseNonTestGoFiles(t, dir) {
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !strings.HasSuffix(ts.Name.Name, "Request") {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			var fields []string
			for _, f := range st.Fields.List {
				// An embedded field has no name; it would carry its own fields in,
				// and this guard does not follow them. None exist today — fail if
				// one appears rather than quietly checking a subset.
				if len(f.Names) == 0 {
					t.Fatalf("%s has an embedded field, which this guard does not "+
						"follow — its promoted fields would be invisible here. Extend the "+
						"extractor rather than leaving the gap", ts.Name.Name)
				}
				for _, name := range f.Names {
					if name.IsExported() {
						fields = append(fields, name.Name)
					}
				}
			}
			sort.Strings(fields)
			out[ts.Name.Name] = fields
			return true
		})
	}
	return out
}

// engineRequestReads reports, per request type, which of its fields the engine
// reads — and separately, which methods hand the whole request onward where this
// scan cannot follow it.
func engineRequestReads(t *testing.T, dir string) (reads map[string]map[string]bool, opaque []string) {
	t.Helper()

	reads = map[string]map[string]bool{}
	for _, file := range parseNonTestGoFiles(t, dir) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Type.Params == nil {
				continue
			}
			for _, param := range fn.Type.Params.List {
				// v1.XRequest — a qualified type from the api package.
				sel, ok := param.Type.(*ast.SelectorExpr)
				if !ok || !strings.HasSuffix(sel.Sel.Name, "Request") {
					continue
				}
				reqType := sel.Sel.Name
				if reads[reqType] == nil {
					reads[reqType] = map[string]bool{}
				}
				for _, name := range param.Names {
					if name.Name == "_" {
						// The parameter is discarded outright, so nothing on it is read.
						// Left to the field loop to report, field by field.
						continue
					}
					collectParamReads(fn, name.Name, reads[reqType], &opaque)
				}
			}
		}
	}
	return reads, opaque
}

// collectParamReads records every `param.Field` selector in fn's body, and flags
// the method when `param` is used as anything else — passed whole to a call,
// assigned, returned — because those uses carry the fields out of view.
func collectParamReads(fn *ast.FuncDecl, param string, into map[string]bool, opaque *[]string) {
	// Selectors on the parameter are the reads. Recorded first so the
	// whole-identifier pass below can tell a selector's base from a bare use.
	selectorBases := map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == param {
			into[sel.Sel.Name] = true
			selectorBases[id] = true
		}
		return true
	})

	// Any OTHER mention of the identifier hands the value somewhere this scan
	// does not follow.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != param || selectorBases[id] {
			return true
		}
		*opaque = append(*opaque, fn.Name.Name+" (parameter "+param+")")
		return false
	})
}

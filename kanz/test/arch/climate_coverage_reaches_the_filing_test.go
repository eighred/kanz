package arch

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// A CLIMATE METRIC'S COVERAGE MAY NOT BE THROWN AWAY AT THE CALL SITE (#618).
//
// # What went wrong
//
// Every carbon metric in internal/sustainability degrades a missing vendor datum
// to zero while keeping the holding in the denominator. An uncovered position
// therefore pulls WACI DOWN, and leaves financed emissions and climate VaR LOW —
// all three in the flattering direction, and all three indistinguishable from a
// genuinely clean book. That number reached a SIGNED TCFD/SFDR filing.
//
// The package believed otherwise. Two doc comments said the gap was "surfaced as
// a data-coverage gap a layer up" and "surfaced as a Coverage metric rather than
// papered over". The layer up was Index.Coverage, which has never had a non-test
// caller — the live path takes []Holding off an HTTP body and never builds an
// Index. Both comments described a path that was not the one wired, which is why
// this is a guard and not a third comment.
//
// # What this checks
//
// The metric functions now return (value, Coverage), so the coverage cannot be
// missed by accident — but it CAN still be dropped on the floor with `v, _ :=`,
// and that single underscore restores the original defect exactly. So: no
// non-test call site may discard the Coverage return, unless it is named below
// with the reason.
//
// # Its relationship to TestEveryMeasureCarriesItsCoverage
//
// That guard (#527, #509) holds the same PROPERTY for internal/risk/compute — a
// measure computed over nothing says so on the response — by a different
// MECHANISM: it checks that v1.Measure composite literals set a Coverage field.
// Neither guard can see the other's shape, which is why this is a second file and
// not an edit to that one. The shared lesson is in that guard's doc: #527 fixed
// four families one at a time, left nothing behind to hold the property, and
// liquidity then shipped with the identical defect.
//
// # Known weakness, stated rather than discovered later
//
// Callees are matched BY NAME, not by resolved type — the arch suite parses
// without type information. A same-named function in an unrelated package would
// be checked too. All three names are distinctive enough that this has no false
// positives today, and a false positive is a build failure somebody reads, not a
// silent pass.

// coverageReturningMetrics are the internal/sustainability functions whose second
// return is the record that qualifies the first.
var coverageReturningMetrics = map[string]bool{
	"WeightedAverageCarbonIntensity": true,
	"FinancedEmissions":              true,
	"ClimateVaR":                     true,
}

// climateCoverageDiscardExempt names a call site permitted to discard the
// Coverage, and why. Keyed "<relative file> -> <callee>".
var climateCoverageDiscardExempt = map[string]string{
	"internal/sustainability/disclosure.go -> ClimateVaR": "#618 — climate VaR's coverage IS the " +
		"attribution (EVIC) coverage, and TCFDValues has already taken it from FinancedEmissions " +
		"three lines up and files it as TCFD_EMISSIONS_DATA_COVERAGE. Binding it a second time " +
		"would file one record under two codes and invite them to drift apart. THE COVERAGE IS " +
		"NOT LOST HERE — it is carried by its sibling. Retire this entry if climate VaR ever " +
		"acquires a datum financed emissions does not share (a per-sector physical hazard map " +
		"would do it), because then the two records stop being the same record.",
}

func TestNoClimateMetricDiscardsItsCoverage(t *testing.T) {
	root := moduleRoot(t)

	var problems []string
	used := map[string]bool{}
	sites := 0

	walkGoFiles(t, root, ".", token.NewFileSet(), func(rel string, f *ast.File) {
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call)
			if !coverageReturningMetrics[name] {
				return true
			}
			sites++
			// The coverage is the SECOND result. A one-variable assignment cannot
			// compile against a two-result call, so the only shapes here are two
			// LHS entries — and the question is whether the second is a blank.
			if len(as.Lhs) < 2 || !isBlank(as.Lhs[1]) {
				return true
			}
			key := rel + " -> " + name
			if _, ok := climateCoverageDiscardExempt[key]; ok {
				used[key] = true
				return true
			}
			problems = append(problems, fmt.Sprintf("%s discards the Coverage returned by %s", rel, name))
			return true
		})
	})

	// NON-VACUITY. internal/sustainability/disclosure.go alone makes five of these
	// calls (WACI and FinancedEmissions from each of TCFDValues and SFDRValues,
	// plus ClimateVaR). A scan finding fewer has lost sight of the call sites and
	// would stay green while every one of them dropped its coverage.
	if sites < 5 {
		t.Fatalf("found %d call(s) to a coverage-returning climate metric — expected at least 5. "+
			"The call shape moved and this guard is asserting nothing", sites)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("%d climate metric call(s) throw away the coverage that qualifies the number:\n\n  %s\n\n"+
			"An uncovered holding degrades to zero and KEEPS ITS WEIGHT, so the metric is understated "+
			"rather than absent, and a signed TCFD/SFDR filing then reports an unmeasured book exactly "+
			"as it reports a clean one (#618). Bind the second return and carry it to the filing, or "+
			"add a named exemption saying which sibling record already carries it.",
			len(problems), strings.Join(problems, "\n  "))
	}

	// A DEAD EXEMPTION IS A REPAIR NOBODY NOTICED. If the call site is gone, the
	// entry must go with it, or the list slowly becomes a record of the past.
	for key := range climateCoverageDiscardExempt {
		if !used[key] {
			t.Errorf("exemption %q matches no call site — the discard it excuses is gone, "+
				"so remove the entry", key)
		}
	}
}

// calleeName is the bare function name of a call, whether it is called plainly
// (FinancedEmissions), through a package (sustainability.FinancedEmissions) or on
// a receiver (in.Scenario.ClimateVaR).
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func isBlank(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "_"
}

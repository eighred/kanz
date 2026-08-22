package arch

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// NO READ OF RISK STATE MAY BE PROPORTIONAL TO THE ESTATE (#674).
//
// # What went wrong
//
// The bootstrap restore ran `FROM positions ORDER BY portfolio_id, instrument_id`
// and `FROM applied_keys` with no WHERE and no LIMIT, grouped the whole result in
// memory, and returned it as one slice. Startup time and peak memory both scaled
// with total portfolio count — recovery time as a function of how large the book
// has grown, on the one component whose entire job is to come back after a
// failure.
//
// It was correct on the day it was written and it does not survive scale, which
// is why nothing caught it: there is no failing test for "this will be slow
// later", and the unit tests all ran against a handful of portfolios.
//
// # What this checks
//
// Every SQL string handed to a Query/QueryRow in internal/risk/state/persist must
// carry a WHERE or a LIMIT. A read with neither is, by construction, proportional
// to whatever the table holds.
//
// THE Go-LEVEL SHAPE IS ALREADY GUARDED BY THE COMPILER — persist.StateStore has
// no slice-returning LoadAll any more, only LoadEach, so a consumer cannot ask
// for the estate in one piece. This guard covers the other half: a NEW query,
// added to this package later, that reaches for a whole table. The compiler has
// nothing to say about that, and the person adding it will be reading the
// neighbouring code, which is now all correctly bounded and therefore no warning
// at all.
//
// # What it CANNOT check
//
//   - A WHERE that is not selective (`WHERE 1=1`, or a predicate matching every
//     row) satisfies it. There is no cheap syntactic test for selectivity, and a
//     guard claiming one would be worse than this one: the behavioural half is
//     services/risk-engine/internal/app's TestRestoreFootprintDoesNotGrowWithThe
//     Estate, which measures the heap during a 100,000-record restore.
//   - SQL built by concatenation rather than written as a literal. None exists
//     here, and this package's queries are all constant strings by convention.

// boundedSQLScope is the package whose reads must stay bounded.
const boundedSQLScope = "internal/risk/state/persist"

// unboundedReadExempt maps "<file>:<line>" to the reason a read there may scan a
// whole table. DEFAULT-DENY and empty: every read in this package is bounded
// today, and an entry is somebody deciding on purpose that a restore may take
// time proportional to the book.
var unboundedReadExempt = map[string]string{}

func TestEveryRiskStateReadIsBounded(t *testing.T) {
	root := moduleRoot(t)

	var problems []string
	queries := 0
	used := map[string]bool{}

	fset := token.NewFileSet()
	walkGoFiles(t, root, boundedSQLScope, fset, func(rel string, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Query", "QueryRow":
			default:
				return true
			}
			// The SQL is the first string argument after the context.
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				sql := strings.ToUpper(lit.Value)
				if !strings.Contains(sql, "SELECT") {
					continue // an INSERT/UPDATE/DELETE, bounded by its own key
				}
				queries++
				line := fset.Position(lit.Pos()).Line
				key := fmt.Sprintf("%s:%d", rel, line)
				if _, ok := unboundedReadExempt[key]; ok {
					used[key] = true
					break
				}
				if strings.Contains(sql, "WHERE") || strings.Contains(sql, "LIMIT") {
					break
				}
				problems = append(problems, fmt.Sprintf("%s reads with neither WHERE nor LIMIT", key))
				break
			}
			return true
		})
	})

	// NON-VACUITY. This package issues several SELECTs today (one per portfolio
	// for Load, plus the three that make up a bootstrap page). A scan finding
	// none has lost the package or the call shape and would pass a tree in which
	// every read was a full scan.
	if queries < 3 {
		t.Fatalf("found %d SELECT(s) in %s — expected at least 3. The query shape moved and this "+
			"guard is asserting nothing", queries, boundedSQLScope)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("%d risk-state read(s) are proportional to the estate:\n\n  %s\n\nA restore that "+
			"reads a whole table makes startup time and peak memory scale with total portfolio "+
			"count, which turns recovery time into a function of how successful the platform is "+
			"(#674). Page it with a keyset cursor — see persist.paginateRecords, which already "+
			"does the walk.", len(problems), strings.Join(problems, "\n  "))
	}

	for key := range unboundedReadExempt {
		if !used[key] {
			t.Errorf("exemption %q matches no read — the scan it excuses is gone, so remove the "+
				"entry", key)
		}
	}
}

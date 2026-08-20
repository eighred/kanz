package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// A READ OF THE BAR SERIES MUST BOUND THE KNOWLEDGE AXIS, NOT ONLY THE
// OBSERVATION WINDOW (#416, owner ruling 2026-08-20).
//
// # The leak, and why nothing else catches it
//
// store.BarQuery has two time axes and only one of them is obvious:
//
//	From / To   the OBSERVATION window — which minutes the bars are ABOUT.
//	AsOf        the KNOWLEDGE horizon — what Kanz had LEARNED by then.
//
// BarQuery.AsOf's own doc states the consequence: "without it a run 'as of' last
// year reads corrections that arrived last week and reports a strategy that could
// not have existed". The store is bitemporal because a venue restating a candle
// writes a NEW knowledge_time rather than overwriting, so bounding From/To alone
// returns bars from exactly the right minutes saying what the venue decided
// LATER they should have said.
//
// THERE IS NO SYMPTOM. The bars are real, the window is the one asked for, the
// numbers are in range, and the error runs in the FLATTERING direction — a
// backtest reads better than the truth, and every model selected on top of it was
// selected partly for its ability to read the future. Nothing in this repository
// would say so: it compiles, the unit tests pass, and the only counter-evidence
// is live money.
//
// # Why this is separate from bar_reader_accounts_for_gaps_test.go
//
// That guard asks whether a reader accounted for the buckets that are NOT THERE.
// This one asks whether it accounted for what it did not YET KNOW. A reader can
// pass either and fail the other, and the two failures cost different things: a
// gap makes a claim unprovable, a knowledge leak makes a false claim look proven.
//
// # What it checks, and what it cannot
//
// Every composite literal of store.BarQuery in non-test code sets AsOf. Matched
// through go/ast rather than the text, because three guards in this directory
// have already passed with the checked thing deleted by matching their own prose.
//
// It CANNOT check that the value is right. `AsOf: time.Now()` satisfies it and is
// correct for a live read and total look-ahead for a backtest — which is exactly
// why internal/alpha/barview refuses a zero DecisionTime rather than defaulting
// to now. What this buys is that the field is a visible decision at every call
// site rather than a zero value nobody typed.
var barQueryKnowledgeExempt = map[string]string{
	"internal/marketdata/backfill": "NOT A DEFECT, and this entry should outlive the others — " +
		"the same standing this package has in bar_reader_accounts_for_gaps_test.go, for the " +
		"same reason. backfill is a WRITER: its read asks 'what does the store hold RIGHT NOW' " +
		"so an unchanged bar is not rewritten under a fresh knowledge_time. Pinning that read " +
		"to a past horizon would hide the newest version from the comparison and make every " +
		"re-run write a restatement of nothing, which is the defect store.SameCandle exists to " +
		"prevent. It measures nothing and reports nothing, so there is no claim a later " +
		"correction could corrupt.",
}

func TestABarReadBoundsTheKnowledgeHorizon(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// queries: package dir -> how many store.BarQuery literals it builds.
	queries := map[string]int{}
	// unbounded: package dir -> a literal that set no AsOf.
	unbounded := map[string]bool{}

	for _, sub := range []string{"internal", "services", "tools", "cmd", "pkg"} {
		walkGoFiles(t, root, sub, fset, func(rel string, f *ast.File) {
			dir := filepath.ToSlash(filepath.Dir(rel))
			// The store DEFINES BarQuery; its own uses are the implementation.
			if dir == "internal/marketdata/store" {
				return
			}
			alias := storeAlias(f)
			if alias == "" {
				return
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "BarQuery" {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != alias {
					return true
				}
				queries[dir]++
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "AsOf" {
						return true
					}
				}
				unbounded[dir] = true
				return true
			})
		})
	}

	// NON-VACUITY. A walk that stops matching passes having checked nothing —
	// the exact failure this guard exists to prevent, one level up. Eight
	// literals across seven packages when this was written.
	total := 0
	for _, n := range queries {
		total += n
	}
	if total < 5 {
		t.Fatalf("found only %d store.BarQuery literal(s) — the walk or the selector match is "+
			"broken, not the estate. Packages found: %v", total, sortedKeys(queries))
	}

	var leaks []string
	seenExempt := map[string]bool{}
	for dir := range unbounded {
		if _, ok := barQueryKnowledgeExempt[dir]; ok {
			seenExempt[dir] = true
			continue
		}
		leaks = append(leaks, dir)
	}
	if len(leaks) > 0 {
		sort.Strings(leaks)
		t.Errorf("%d package(s) read the OHLCV bar series bounding only the OBSERVATION window "+
			"and not the KNOWLEDGE horizon (store.BarQuery.AsOf): %v.\n"+
			"The store is bitemporal: a venue restating a candle writes a new knowledge_time "+
			"rather than overwriting, so a query bounded on From/To alone returns bars from "+
			"exactly the right minutes saying what the venue decided LATER they should have "+
			"said. There is no symptom — the bars are real, the window is the one asked for, "+
			"and the error runs in the flattering direction, so a backtest reads better than "+
			"the truth and the only counter-evidence is live money.\n"+
			"Set AsOf, or add an entry to barQueryKnowledgeExempt NAMING THE ISSUE that will.",
			len(leaks), leaks)
	}

	// DEAD-ENTRY ARM: an exemption for a package that now bounds the horizon, or
	// no longer reads the series, is a claim that outlived its repair.
	for dir, reason := range barQueryKnowledgeExempt {
		if seenExempt[dir] {
			continue
		}
		if queries[dir] > 0 {
			t.Errorf("exemption for %q is stale — every bar read there now bounds the knowledge "+
				"horizon. Delete the entry (%s)", dir, reason)
			continue
		}
		t.Errorf("exemption for %q no longer reads the bar series, or was renamed. Delete the "+
			"entry (%s)", dir, reason)
	}
}

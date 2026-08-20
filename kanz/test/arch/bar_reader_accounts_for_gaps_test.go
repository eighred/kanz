package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// A READER OF THE BAR SERIES MUST ACCOUNT FOR THE BUCKETS THAT ARE NOT THERE
// (#416).
//
// # The absence this guard exists to keep visible
//
// internal/marketedge/bars/fold.go emits NO BAR for a minute in which nothing
// traded, and is explicit that it cannot do better:
//
//	this process cannot tell "nothing traded" from "the feed was down" or "we
//	were rolling pods" — they look identical from here. [...] A gap is honest
//	and is THE CALLER'S TO INTERPRET.
//
// So a gapped series is the NORMAL shape of this input. The producer handed the
// question to its callers; for a long time no caller in the decision layer took
// it, and both readers returned confident numbers over windows nobody could
// speak for:
//
//	internal/alpha/outcome    a 10-minute horizon holding 1 of its 10 minutes
//	                          returned ok=true, Hit=false. A calibration report
//	                          counted a venue outage against the model.
//	internal/marketdata/      a 1m series with 9 of every 10 minutes absent
//	internal/marketdata/      reported rsi_14 = 100 — "maximally overbought" —
//	  indicator               from 15 prints spread over 150 minutes. A period
//	                          was an element count, not a duration.
//
// Both were in range, both plausible, and neither was detectable from any signal
// the repository had. That is the same failure #527/#566 fixed for risk measures
// over positions, and this is the same doctrine over intervals.
//
// # THE SIGNAL IS SOUND IN ONE DIRECTION ONLY, and this guard does not check the
// # other
//
// rollup/driver.go ruled on it and the ruling stands:
//
//	THE OBVIOUS TEST DOES NOT WORK. "1440 constituents" is not completeness: a
//	minute in which nothing traded produces NO BAR AT ALL, so a genuinely quiet
//	day is indistinguishable from a day whose ingestion died.
//
// A missing bucket proves a window cannot support a claim. A WHOLE WINDOW PROVES
// NOTHING ABOUT WHETHER THE PLATFORM WAS OBSERVING. What would settle the second
// half is an ingestion-coverage record stating which intervals were actually
// watched, which rollup/driver.go names as missing and which is missing
// still — and which can only ever be captured going FORWARD, since nothing can
// reconstruct whether a feed was live last July.
//
// So passing this guard means a package CONSULTED the gap vocabulary. It does not
// mean the package is correct, and it cannot: a caller could compute a Window and
// ignore it. What it buys is that the next bar-series reader is a visible
// decision in a diff rather than another silent confident number found years
// later by reading seven call sites by hand — which is how these two were found.
//
// # Why constructing a BarQuery is the trigger
//
// store.BarQuery is what a read of the series is SPELLED as; there is no other
// way to ask. Matching the composite literal through go/ast rather than the text
// means the guard cannot be satisfied by a comment mentioning the type, and
// cannot be evaded by renaming a variable. Three guards in this directory have
// already passed with the checked thing deleted because they matched raw source
// and found their own prose.

// gapVocabulary is what "accounted for it" is spelled as. Any one of these in the
// package is enough — the guard checks that the question was asked, not how it
// was answered.
var gapVocabulary = map[string]bool{
	"WindowOf":         true,
	"ContiguousSuffix": true,
	"Window":           true,
}

// barReaderExempt maps a package that reads the bar series to the issue that will
// make it account for gaps.
//
// EVERY ENTRY NAMES AN ISSUE. An exemption saying "not yet" without saying who is
// tracking it is how a confident number lives for a year.
var barReaderExempt = map[string]string{
	"internal/marketdata/backfill": "NOT A DEFECT, and this entry should outlive the others. " +
		"backfill is a WRITER: its read compares what the store already holds so an unchanged " +
		"bar is not rewritten under a fresh knowledge_time. It measures nothing over the window, " +
		"so there is no claim a gap could corrupt — and it is the process that FILLS gaps, so " +
		"refusing to act on one would be exactly backwards.",
	"internal/marketdata/rollup": "NOT A DEFECT — rollup already carries a STRONGER contract " +
		"than this guard checks, and it is the one that ruled the count-based test unsound. " +
		"Request.Watermark is required with no default, and a bucket is folded only once the " +
		"caller asserts the base series is complete through it (driver.go). Adopting " +
		"store.WindowOf here would REPLACE an asserted completeness with an inferred one, which " +
		"is the trade this platform has already refused in writing.",
}

func TestABarReaderAccountsForGaps(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// readers: package dir -> true when it constructs a store.BarQuery.
	readers := map[string]bool{}
	// accounts: package dir -> true when it names the gap vocabulary.
	accounts := map[string]bool{}

	for _, sub := range []string{"internal", "services", "tools", "cmd", "pkg"} {
		walkGoFiles(t, root, sub, fset, func(rel string, f *ast.File) {
			dir := filepath.ToSlash(filepath.Dir(rel))
			// The store DEFINES the vocabulary; it cannot be required to consult
			// itself, and its own BarQuery uses are the implementation.
			if dir == "internal/marketdata/store" {
				return
			}
			// Which package selector refers to the store, so `foo.Window` in an
			// unrelated package cannot be mistaken for the real thing.
			alias := storeAlias(f)

			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || alias == "" || pkg.Name != alias {
					return true
				}
				switch {
				case sel.Sel.Name == "BarQuery":
					readers[dir] = true
				case gapVocabulary[sel.Sel.Name]:
					accounts[dir] = true
				}
				return true
			})
		})
	}

	// NON-VACUITY. If the walk stops finding readers the guard passes having
	// checked nothing — the exact failure it exists to prevent, one level up.
	// Seven call sites were audited when this was written.
	if len(readers) < 5 {
		t.Fatalf("found only %d package(s) constructing a store.BarQuery — the walk or the "+
			"selector match is broken, not the estate. Readers found: %v",
			len(readers), sortedKeys(readers))
	}

	var unaccounted []string
	seenExempt := map[string]bool{}
	for dir := range readers {
		if accounts[dir] {
			continue
		}
		if _, ok := barReaderExempt[dir]; ok {
			seenExempt[dir] = true
			continue
		}
		unaccounted = append(unaccounted, dir)
	}

	if len(unaccounted) > 0 {
		sort.Strings(unaccounted)
		t.Errorf("%d package(s) read the OHLCV bar series and never consult the gap "+
			"vocabulary (store.WindowOf / store.ContiguousSuffix / store.Window): %v.\n"+
			"bars/fold.go emits NO BAR for a minute in which nothing traded and hands the "+
			"interpretation to the caller, so a gapped series is the normal input here. A "+
			"measure computed over one is a confident number about intervals nobody observed: "+
			"the alpha outcome resolver marked a venue outage as a model miss, and the "+
			"indicator library reported rsi_14=100 from fifteen prints spread over 150 "+
			"minutes.\nAccount for it, or add an entry to barReaderExempt NAMING THE ISSUE "+
			"that will.", len(unaccounted), unaccounted)
	}

	// DEAD-ENTRY ARM: an exemption for a package that now accounts for gaps, or
	// no longer reads the series at all, is a claim that outlived its repair.
	for dir, reason := range barReaderExempt {
		if seenExempt[dir] {
			continue
		}
		if accounts[dir] {
			t.Errorf("exemption for %q is stale — the package now consults the gap "+
				"vocabulary. Delete the entry (%s)", dir, reason)
			continue
		}
		t.Errorf("exemption for %q no longer reads the bar series, or was renamed. Delete "+
			"the entry (%s)", dir, reason)
	}
}

// storeAlias returns the local name f refers to internal/marketdata/store by, or
// "" when it does not import it.
func storeAlias(f *ast.File) string {
	const path = `"github.com/eighred/kanz/internal/marketdata/store"`
	for _, imp := range f.Imports {
		if imp.Path == nil || imp.Path.Value != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "store"
	}
	return ""
}

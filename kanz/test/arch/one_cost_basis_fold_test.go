package arch

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THERE IS ONE WEIGHTED-AVERAGE-COST FOLD (#428).
//
// A fund's cost basis decides its realized P&L, its unrealized P&L, and every
// risk and performance number computed from either. This platform had THREE
// implementations of the arithmetic that maintains it:
//
//   - services/oms/internal/position — the OMS book, in memory and in Postgres
//   - services/accounting/internal/ledger — the IBOR
//   - services/tv-sync/internal/projection — the TradingView projection
//
// They had already drifted, in both directions that matter:
//
//  1. THE ZERO-DIVISOR GUARD. big.Rat.Quo panics on a zero divisor, and the
//     divisor is derived rather than validated. #217 crash-looped the estate on
//     that line. The fix was applied to one copy and hand-copied to a second,
//     leaving the accounting fold to write down the real problem in its own
//     comment: "Two copies of one calculation is the standing risk; the next edit
//     to either should not have to rediscover which one had the guard."
//  2. FEES. tv-sync netted the fill's fee out of realized P&L; the other two did
//     not. "What has this position realized" had two answers depending on which
//     surface a person was looking at, and nothing anywhere said so.
//
// #428 consolidated them into internal/costbasis, because the backtest it builds
// would otherwise have become the FOURTH — and a backtest that computes a fund's
// P&L with its own copy of the basis arithmetic is a backtest whose results
// cannot be compared to the books.
//
// WHAT THIS CHECKS: no file outside internal/costbasis contains the re-averaging
// division that IS this calculation — a cost total divided by a new absolute
// quantity. That expression is the fold's fingerprint; a package computing it is
// maintaining its own basis whatever it calls the variables.
//
// WHAT IT CANNOT CHECK: that a fourth fold written in a completely different
// shape is caught. This is a tripwire on the shape the three real copies shared,
// not a proof of absence — which is why internal/costbasis carries the
// behavioural tests and this only stops the copy-paste.

// costBasisScopes are the trees searched. Deliberately narrow: this is about
// production folds, and a test constructing an expected average by hand is doing
// something legitimate.
var costBasisScopes = []string{"services", "internal", "tools", "cmd"}

// costBasisFoldRe matches the re-averaging division at the heart of the fold:
// a Quo whose divisor is a "new absolute quantity" and whose dividend is a
// running cost. Both spellings the real copies used are covered.
var costBasisFoldRe = regexp.MustCompile(`(?i)Quo\(\s*(cost|num|numerator)\s*,\s*(newAbs|absQty|newAbsQty|totalAbs)\s*\)`)

// costBasisExempt maps a module-relative file to the reason it may carry the
// fold, and what retires the entry.
//
// ONE ENTRY, WHICH IS THE POINT. This guard exists to keep the count at one.
var costBasisExempt = map[string]string{
	"internal/costbasis/costbasis.go": "THE one implementation. Every other package folds through it.",
}

func TestThereIsOneWeightedAverageCostFold(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	seenExempt := map[string]bool{}
	scanned, withFold := 0, 0

	for _, scope := range costBasisScopes {
		for _, gf := range goFilesUnder(t, filepath.Join(root, scope)) {
			if strings.HasSuffix(gf.rel, "_test.go") {
				continue
			}
			scanned++
			rel := filepath.ToSlash(filepath.Join(scope, gf.rel))
			if !costBasisFoldRe.MatchString(gf.body) {
				continue
			}
			withFold++
			if reason, ok := costBasisExempt[rel]; ok {
				seenExempt[rel] = true
				t.Logf("%s: exempt — %s", rel, reason)
				continue
			}
			offenders = append(offenders, rel)
		}
	}

	// NON-VACUITY, the walk half: a moved tree scans nothing and passes.
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test files across %v — the walk is broken, not the estate",
			scanned, costBasisScopes)
	}
	// NON-VACUITY, the match half: if the expression is reworded, this finds no
	// folds at all and passes while asserting nothing — including that the one
	// real implementation is still there.
	if withFold == 0 {
		t.Fatalf("found the re-averaging division in NO file — internal/costbasis should contain "+
			"exactly one. The pattern %s stopped matching and this guard is asserting nothing",
			costBasisFoldRe)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d file(s) outside internal/costbasis compute a weighted-average cost basis: %v.\n"+
			"A fund's basis decides its realized P&L, its unrealized P&L, and every risk and "+
			"performance figure derived from either. Three copies of this arithmetic existed before "+
			"#428 and had already drifted — one carried a zero-divisor guard the others learned "+
			"about only by hand-copying (#217), and one netted fees into realized P&L while the "+
			"others reported gross, so the same position showed two different numbers on two "+
			"different screens. Fold through internal/costbasis, or add an argued entry to "+
			"costBasisExempt.", len(offenders), offenders)
	}

	// DEAD-ENTRY ARM: an exemption for a file that no longer carries the fold has
	// outlived its repair. If this fires for costbasis.go itself, the one
	// implementation has moved and this guard is now protecting nothing.
	for rel, reason := range costBasisExempt {
		if !seenExempt[rel] {
			t.Errorf("exemption for %q (%s) matches no file computing the fold — either it moved "+
				"(update the key) or the one implementation is gone (which is the emergency, not "+
				"the exemption)", rel, reason)
		}
	}
}

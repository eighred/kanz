package arch

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	// riskTree is the subtree the base-currency filter rule governs.
	riskTree = "internal/risk/"
	// baseCurrencyFilterHome is the ONE file allowed to decide what counts
	// as "denominated in the portfolio's base currency" —
	// domain.Position.InBaseCurrency / UncertaintyInBaseCurrency and the
	// moneyInBase they share, plus Portfolio.CurrencyExclusions, which is
	// defined in terms of the same predicate.
	baseCurrencyFilterHome = "internal/risk/domain/portfolio.go"
)

// ONE BASE-CURRENCY FILTER, AND IT MUST REPORT WHAT IT DROPS.
//
// The risk engine has no FX layer (RISK-06), so every base-currency
// aggregate can only include positions already denominated in the
// portfolio's base currency. That rule was open-coded ten times across
// four packages:
//
//	if pos.MarketValue == nil || pos.MarketValue.CurrencyCode != base {
//	    continue
//	}
//
// in measures.go (sumInBaseCurrency, backing GrossExposure, NetExposure,
// VaR99 and Delta), uncertainty.go ×2, concentration.go, factorrisk.go,
// factor/exposure.go, var/historical.go, var/montecarlo.go,
// liquidity/horizon.go and liquidity/impact.go. Every copy dropped
// positions and NOT ONE recorded that it had, so a USD-base portfolio
// holding only EUR reported GrossExposure = 0 in ModeNormal with no
// quality flags — byte-identical to an empty book (#257).
//
// THE DANGEROUS PART IS THE DIRECTION. Dropping positions makes exposure
// SMALLER, never larger, so a concentration limit computed over a
// fraction of the book passes when the whole book would breach. A number
// that is wrong in the direction of "no action needed" is worse than no
// number.
//
// THE POINT OF THIS GUARD IS THE COPIES, NOT THE ARITHMETIC. The comment
// on measures.go's helper already said it existed so a future
// same-currency change "edits one function, not four" — and by the time
// #257 was filed there were ten, and the helper's OTHER claim (that
// surfacing the skip was "RISK-11's degraded-mode concern") pointed at
// Detector.Assess, which takes only a time.Time and cannot see a
// position at all. A stated intent that nothing enforces decays; this is
// what stops the eleventh copy.
//
// SCOPE AND LIMIT. Production files only: a test asserting
// `got.CurrencyCode != "USD"` is checking a result, not implementing a
// filter, and a guard that fires on correct code gets deleted. This is a
// syntactic check on the comparison, so it catches the copy-paste that
// actually happened ten times, not every conceivable re-expression of
// the rule. Behaviour is pinned separately by the unit tests asserting
// that a mixed-currency portfolio yields a non-empty CurrencyExclusions
// and v1.QualityFlagCurrencyExcluded on the response.
func TestOnlyOnePlaceFiltersOnBaseCurrency(t *testing.T) {
	root := moduleRoot(t)
	files := goFilesUnder(t, root)
	if len(files) == 0 {
		t.Fatalf("found zero Go files under %s — the scanner is broken, not the estate", root)
	}

	// Matches `X.CurrencyCode != base` and `X.GetCurrencyCode() == base`
	// alike: the accessor form is the same rule wearing a getter.
	filterRe := regexp.MustCompile(`(?:CurrencyCode|GetCurrencyCode\(\))\s*[!=]=`)

	var scanned, offenders []string
	homeMatches := false
	for _, f := range files {
		if !strings.HasPrefix(f.rel, riskTree) || strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		scanned = append(scanned, f.rel)
		if !filterRe.MatchString(f.body) {
			continue
		}
		if f.rel == baseCurrencyFilterHome {
			homeMatches = true
			continue
		}
		offenders = append(offenders, f.rel)
	}

	// NON-VACUITY. A path typo in riskTree would scan nothing and pass.
	if len(scanned) == 0 {
		t.Fatalf("scanned zero production files under %s%s — the filter is wrong, "+
			"so this test proves nothing", root, riskTree)
	}

	// DEAD-ENTRY CHECK. If the home file no longer compares a currency
	// code, the rule moved (or was inlined back into the call sites) and
	// this exemption is now a licence for the one file nobody is watching.
	if !homeMatches {
		t.Errorf("%s no longer contains a base-currency comparison, but is still "+
			"exempted here\n\n"+
			"Either the rule moved — in which case point baseCurrencyFilterHome at its "+
			"new home — or it was deleted, in which case delete the exemption. An "+
			"exemption that outlives the thing it exempts is how the next copy gets in.",
			baseCurrencyFilterHome)
	}

	sort.Strings(offenders)
	for _, f := range offenders {
		t.Errorf("%s compares a currency code against a base currency — that is the "+
			"base-currency filter rule, open-coded\n\n"+
			"Use domain.Position.InBaseCurrency (or UncertaintyInBaseCurrency), which "+
			"lives in %s. It is not about saving a line: Portfolio.CurrencyExclusions "+
			"reports which positions the measures left out, and it is defined in terms "+
			"of that SAME predicate, so what gets excluded and what gets reported "+
			"excluded cannot drift. A local copy silently drops positions that "+
			"CurrencyExclusions will not list, which restores #257 — measures quietly "+
			"computed over a fraction of the book, low in the direction that makes a "+
			"limit check pass.",
			f, baseCurrencyFilterHome)
	}
}

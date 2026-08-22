package sustainability

import "sort"

// A CLIMATE METRIC CARRIES THE SHARE OF THE BOOK IT COULD BE COMPUTED FROM (#618).
//
// # What went wrong without it
//
// Every carbon metric here degrades a missing vendor datum to zero and keeps the
// holding in the denominator. A position with no revenue figure has Intensity() ==
// 0 but still takes its weight in WACI, so it pulls the portfolio number DOWN; a
// position with no EVIC is skipped by PCAF attribution and takes no transition
// shock, so financed emissions and climate VaR come out LOW. Each degradation is
// individually defensible — an undefined ratio is not a number — and together they
// meant an unmeasured book and a genuinely clean one filed the same figures, under
// a signature, to a regulator.
//
// The package's own doc comments asserted this was already handled: carbon.go said
// a no-EVIC holding was "surfaced as a data-coverage gap a layer up" and ingest.go
// said "the gap is surfaced as a Coverage metric rather than papered over". The
// layer up was Index.Coverage, which has never had a non-test caller — the live
// path takes []Holding straight off an HTTP body and never builds an Index. Both
// comments described a path that was not the one wired.
//
// # Why the record travels with the number rather than beside it
//
// Coverage is returned BY the metric functions, not offered as a separate call a
// caller may or may not make. Computing WACI without receiving its coverage no
// longer compiles, which is the same device #240 used when it changed the size
// bound's type so the wrong comparison could not be written. A coverage record
// that must be asked for is one that gets forgotten, and forgetting it is exactly
// how this reached a signed filing.
//
// Coverage is measured in MARKET VALUE, not in holdings: one uncovered position
// carrying 90% of NAV and ninety carrying 1% between them are not the same
// disclosure, and a count cannot tell them apart. It is the same weight the metric
// itself uses, so the fraction says precisely how much of the number is real.
const (
	// datumRevenue is the carbon-intensity denominator — the WACI / SFDR GHG
	// intensity input.
	datumRevenue = "revenue"
	// datumEVIC is the PCAF attribution denominator — the input behind financed
	// emissions, the SFDR carbon footprint, the transition channel of climate VaR,
	// and therefore the implied temperature rise derived from them.
	datumEVIC = "evic"
)

// maxUncoveredSample bounds the instrument list a Coverage carries. On a book
// whose reference data was never loaded EVERY holding is uncovered, and the
// caller of a filing endpoint should not receive the whole book back as a
// diagnostic. UncoveredCount carries the magnitude; the sample is what lets
// somebody go and look. Mirrors compliance's maxUnresolvedSample in intent — the
// same bound for the same reason.
const maxUncoveredSample = 32

// Coverage is the share of a book's market value that carried the reference datum
// a metric needs. It is REPORTED alongside the metric, never used to silently
// adjust it: the arithmetic of the disclosure is what the framework prescribes,
// and this says how much of it rests on data.
type Coverage struct {
	// Datum names what was missing, so a reader knows which vendor field to go
	// and load rather than "some ESG data".
	Datum string
	// TotalValue is Σ|MV| over holdings that can affect the metric at all.
	// Zero-value positions are excluded from BOTH sides: they contribute nothing
	// to any of these metrics, so they can neither dilute a number nor count as a
	// gap in it.
	TotalValue float64
	// CoveredValue is Σ|MV| of the holdings that carried the datum.
	CoveredValue float64
	// UncoveredCount is every holding missing it, not just the sampled ones.
	UncoveredCount int
	// Uncovered is a bounded, sorted sample of the instrument ids missing it.
	Uncovered []string
}

// Fraction is the covered share of market value, in [0,1].
//
// AN EMPTY BOOK IS VACUOUSLY COMPLETE (1), not a total data failure (0). Nothing
// is unmeasured when there is nothing to measure, and reporting 0 there would make
// "no positions" indistinguishable from "no data for any position" — the very
// conflation this type exists to remove.
func (c Coverage) Fraction() float64 {
	if c.TotalValue <= 0 {
		return 1
	}
	return c.CoveredValue / c.TotalValue
}

// Complete reports that every holding able to move the metric carried the datum.
func (c Coverage) Complete() bool { return c.UncoveredCount == 0 }

// Unmeasured reports a book with value in it and the datum for none of it. The
// metric is then not a small number, it is not a measurement at all, and a filing
// path refuses it rather than signing the zero — see DisclosureInputs.TCFDValues.
func (c Coverage) Unmeasured() bool { return c.TotalValue > 0 && c.CoveredValue <= 0 }

// coverageOf walks the book once, weighting by |MV| exactly as the metrics do.
func coverageOf(holdings []Holding, datum string, has func(CarbonMetrics) bool) Coverage {
	cov := Coverage{Datum: datum}
	var uncovered []string
	for _, h := range holdings {
		v := abs(h.MarketValue)
		if v == 0 {
			continue // cannot move any of these metrics; not a gap in one
		}
		cov.TotalValue += v
		if has(h.Carbon) {
			cov.CoveredValue += v
			continue
		}
		cov.UncoveredCount++
		uncovered = append(uncovered, h.InstrumentID)
	}
	sort.Strings(uncovered)
	if len(uncovered) > maxUncoveredSample {
		uncovered = uncovered[:maxUncoveredSample]
	}
	cov.Uncovered = uncovered
	return cov
}

// IntensityCoverage is the revenue-data coverage behind WACI and SFDR GHG
// intensity: CarbonMetrics.Intensity() is defined only for positive revenue.
func IntensityCoverage(holdings []Holding) Coverage {
	return coverageOf(holdings, datumRevenue, func(c CarbonMetrics) bool { return c.Revenue > 0 })
}

// AttributionCoverage is the EVIC coverage behind PCAF financed emissions, the
// SFDR carbon footprint and the transition channel of climate VaR: all three
// attribute by MV/EVIC, which is undefined without it.
func AttributionCoverage(holdings []Holding) Coverage {
	return coverageOf(holdings, datumEVIC, func(c CarbonMetrics) bool { return c.EVIC > 0 })
}

package sustainability

import (
	"testing"
	"time"
)

// coveredBook is two holdings, both carrying full vendor reference data.
func coveredBook() []Holding {
	return []Holding{
		{InstrumentID: "OILCO", MarketValue: 1_000_000,
			Carbon: CarbonMetrics{Scope1: 40_000, Scope2: 10_000, Scope3: 200_000, Revenue: 500, EVIC: 2_000_000}},
		{InstrumentID: "TECHCO", MarketValue: 1_000_000,
			Carbon: CarbonMetrics{Scope1: 1_000, Scope2: 500, Scope3: 5_000, Revenue: 800, EVIC: 4_000_000}},
	}
}

// halfCoveredBook is coveredBook plus an equal-sized holding the vendor has no
// data for at all — the shape that used to dilute the portfolio number downward
// while counting in the denominator (#618).
func halfCoveredBook() []Holding {
	return append(coveredBook(), Holding{InstrumentID: "DARKCO", MarketValue: 2_000_000})
}

// A metric reports the share of the book it could actually be computed from, and
// names what was missing. Without this the caller cannot tell a low-carbon book
// from an unmeasured one.
func TestAMetricReportsTheCoverageItWasComputedFrom(t *testing.T) {
	_, cov := WeightedAverageCarbonIntensity(halfCoveredBook())
	if cov.Fraction() != 0.5 {
		t.Fatalf("intensity coverage = %v, want 0.5 (2m of 4m carries revenue data)", cov.Fraction())
	}
	if cov.Complete() {
		t.Fatal("a book with an uncovered holding reported Complete()")
	}
	if len(cov.Uncovered) != 1 || cov.Uncovered[0] != "DARKCO" {
		t.Fatalf("uncovered = %v, want [DARKCO]", cov.Uncovered)
	}
	if cov.Datum != datumRevenue {
		t.Fatalf("datum = %q, want %q", cov.Datum, datumRevenue)
	}
}

// A fully covered book says so, and an empty book is vacuously complete rather
// than reported as a total data failure.
func TestCoverageIsCompleteForAFullBookAndForNoBook(t *testing.T) {
	if _, cov := WeightedAverageCarbonIntensity(coveredBook()); !cov.Complete() {
		t.Fatalf("fully covered book reported incomplete: %+v", cov)
	}
	if _, cov := FinancedEmissions(nil); !cov.Complete() || cov.Fraction() != 1 {
		t.Fatalf("empty book should be vacuously complete, got %+v", cov)
	}
}

// THE DEFECT ITSELF. An uncovered holding really does drag WACI down — that is
// arithmetic and it is not being changed. What must not happen is the two numbers
// being indistinguishable at the point they are signed.
func TestAnUncoveredHoldingDilutesTheNumberAndSaysSo(t *testing.T) {
	full, fullCov := WeightedAverageCarbonIntensity(coveredBook())
	diluted, dilCov := WeightedAverageCarbonIntensity(halfCoveredBook())

	if !(diluted < full) {
		t.Fatalf("expected the uncovered holding to dilute WACI: full=%v diluted=%v", full, diluted)
	}
	if fullCov.Fraction() == dilCov.Fraction() {
		t.Fatalf("a diluted number carries the same coverage as a complete one (%v) — "+
			"the two are indistinguishable, which is the defect", fullCov.Fraction())
	}
}

// Financed emissions and climate VaR share the OTHER datum: a holding with no
// EVIC is skipped by PCAF attribution and takes no transition shock, so both
// understate against the same gap.
func TestTheAttributionMetricsReportTheEVICGap(t *testing.T) {
	_, feCov := FinancedEmissions(halfCoveredBook())
	if feCov.Datum != datumEVIC || feCov.Fraction() != 0.5 {
		t.Fatalf("financed-emissions coverage = %+v, want evic at 0.5", feCov)
	}
	_, cvCov := NGFSDisorderly().ClimateVaR(halfCoveredBook())
	if cvCov.Datum != datumEVIC || cvCov.Fraction() != 0.5 {
		t.Fatalf("climate-VaR coverage = %+v, want evic at 0.5", cvCov)
	}
}

// THE SIGNED ARTIFACT CARRIES THE COVERAGE. This is the issue's own mutation
// test: a filing over partially-covered holdings must not be byte-identical in
// shape to one over complete data.
func TestAFilingSignsTheCoverageOfTheNumbersItCarries(t *testing.T) {
	asOf := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)

	in := discInputs()
	full, err := FileTCFD(in, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	in.Holdings = halfCoveredBook()
	partial, err := FileTCFD(in, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := full.Lookup(tcfdIntensityCoverage); !ok {
		t.Fatalf("TCFD filing carries no %s line item", tcfdIntensityCoverage)
	}
	if _, ok := full.Lookup(tcfdAttributionCoverage); !ok {
		t.Fatalf("TCFD filing carries no %s line item", tcfdAttributionCoverage)
	}
	got, _ := partial.Lookup(tcfdIntensityCoverage)
	if got.Cmp(exactOrNil(0.5)) != 0 {
		t.Fatalf("filed intensity coverage = %v, want 1/2", got)
	}
	if full.Signature == partial.Signature {
		t.Fatal("a half-covered book produced the same signature as a fully covered one")
	}
}

// SFDR carries the same record: its GHG intensity and carbon footprint are the
// same two metrics under different codes.
func TestTheSFDRFilingCarriesCoverageToo(t *testing.T) {
	in := discInputs()
	in.Holdings = halfCoveredBook()
	rep, err := FileSFDR(in, time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{sfdrIntensityCoverage, sfdrAttributionCoverage} {
		v, ok := rep.Lookup(code)
		if !ok {
			t.Fatalf("SFDR filing carries no %s", code)
		}
		if v.Cmp(exactOrNil(0.5)) != 0 {
			t.Fatalf("%s = %v, want 1/2", code, v)
		}
	}
}

// A BOOK WITH NO CARBON DATA AT ALL IS REFUSED, not filed as a measured zero —
// the same discipline that refuses a non-finite metric (#633) and a value too
// small to render (#672). A signature is what makes a wrong number authoritative.
func TestABookWithNoCoverageIsRefusedRatherThanSignedAsZero(t *testing.T) {
	in := discInputs()
	in.Holdings = []Holding{{InstrumentID: "DARKCO", MarketValue: 1_000_000}}

	if _, err := FileTCFD(in, time.Now(), nil); err == nil {
		t.Fatal("filed a TCFD disclosure over a book with no carbon reference data at all")
	}
	if _, err := FileSFDR(in, time.Now(), nil); err == nil {
		t.Fatal("filed an SFDR disclosure over a book with no carbon reference data at all")
	}
}

// An empty book is not a coverage failure — it has nothing to cover — and must
// keep filing, so the refusal above cannot be reached by having no positions.
func TestAnEmptyBookStillFiles(t *testing.T) {
	in := discInputs()
	in.Holdings = nil
	if _, err := FileTCFD(in, time.Now(), nil); err != nil {
		t.Fatalf("an empty book should still file: %v", err)
	}
}

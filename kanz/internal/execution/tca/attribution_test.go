package tca_test

import (
	"errors"
	"math/big"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution/tca"
)

// EVERY EXPECTED NUMBER IN THIS FILE WAS COMPUTED OUTSIDE GO, in exact rational
// arithmetic, from the scenario and not from the implementation. A benchmark
// whose expected values come from the code it is checking proves the code has
// not changed, which is a different and much weaker claim than proving it is
// right.

func rat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, err := dec.ParseRat(s)
	if err != nil {
		t.Fatalf("bad literal %q in the test itself: %v", s, err)
	}
	return r
}

func bps(t *testing.T, r *big.Rat) string {
	t.Helper()
	if r == nil {
		return "<unset>"
	}
	return r.FloatString(10)
}

// TestAttribute_LegsSumToTheRealisedShortfall is the decisive test #866 names:
// a book with KNOWN fills against a KNOWN arrival produces a shortfall whose
// legs sum to the realised difference.
//
// The scenario is a 100-unit buy decided when the mid was 100.00, worked as two
// slices:
//
//	slice A — 60 units filled at 100.55, released into a 100.35 / 100.45 market
//	slice B — 40 units filled at 101.30, released into a 100.90 / 101.10 market
//
// Achieved average is 100.85 on 100 units, so the realised shortfall against
// arrival is 85 bps of the 10,000 arrival notional. Of that, 7 bps is the
// half-width of the markets that were actually quoted when each slice went out,
// 64 bps is how far the mid had already drifted away by then, and the remaining
// 14 bps is what was paid beyond both.
func TestAttribute_LegsSumToTheRealisedShortfall(t *testing.T) {
	a, err := tca.Attribute(orderpb.Side_SIDE_BUY, rat(t, "100"), []tca.Slice{
		{
			OrderID: "slice-a", FilledQuantity: rat(t, "60"), AveragePrice: rat(t, "100.55"),
			ReleaseMark: rat(t, "100.4"), ReleaseBid: rat(t, "100.35"), ReleaseAsk: rat(t, "100.45"),
		},
		{
			OrderID: "slice-b", FilledQuantity: rat(t, "40"), AveragePrice: rat(t, "101.30"),
			ReleaseMark: rat(t, "101.0"), ReleaseBid: rat(t, "100.90"), ReleaseAsk: rat(t, "101.10"),
		},
	})
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}

	if a.Quality != tca.QualityDecomposed {
		t.Fatalf("quality = %s, want DECOMPOSED — every slice carried a release mark and a quoted "+
			"touch, so there is nothing here the decomposition could not observe", a.Quality)
	}
	if a.Slices != 2 || a.Measured != 2 {
		t.Errorf("slices/measured = %d/%d, want 2/2", a.Slices, a.Measured)
	}
	if got := a.AveragePrice.FloatString(10); got != "100.8500000000" {
		t.Errorf("average price = %s, want 100.85 — the quantity-weighted mean of the two slices", got)
	}

	// THE FOUR FIGURES, EACH AGAINST AN INDEPENDENTLY COMPUTED EXPECTATION.
	// Checking only the sum would pass for any split that adds up, including the
	// degenerate one where spread and timing are zero and impact is the whole
	// shortfall — which is precisely the "arithmetic dressing" failure.
	for _, c := range []struct {
		name string
		got  *big.Rat
		want string
	}{
		{"shortfall", a.ShortfallBps, "85.0000000000"},
		{"spread", a.SpreadBps, "7.0000000000"},
		{"timing", a.TimingBps, "64.0000000000"},
		{"impact", a.ImpactBps, "14.0000000000"},
	} {
		if got := bps(t, c.got); got != c.want {
			t.Errorf("%s_bps = %s, want %s", c.name, got, c.want)
		}
	}

	// AND THE IDENTITY ITSELF, asserted separately from the four values above so
	// that a change which moved all four consistently still has to face it.
	sum := new(big.Rat).Add(a.SpreadBps, a.TimingBps)
	sum.Add(sum, a.ImpactBps)
	if sum.Cmp(a.ShortfallBps) != 0 {
		t.Fatalf("the legs do not sum to the shortfall: spread %s + timing %s + impact %s = %s, "+
			"shortfall %s — a decomposition whose parts do not reconstitute the whole is not an "+
			"attribution, it is three unrelated numbers",
			bps(t, a.SpreadBps), bps(t, a.TimingBps), bps(t, a.ImpactBps),
			sum.FloatString(10), bps(t, a.ShortfallBps))
	}
}

// TestAttribute_SellSideCostsAreSignedFromTheFundsPointOfView pins the half that
// is easy to get wrong and impossible to notice: a sell costs money when it
// receives BELOW arrival. Without this a report can average a good sell against
// a bad buy to zero.
//
// 100 units sold at 99.30 against a 100.00 arrival is a 70 bps cost, not a
// −70 bps saving. Of it, 5 bps is the quoted half-width and 40 bps is the mid
// having already fallen to 99.60 before the order was released.
func TestAttribute_SellSideCostsAreSignedFromTheFundsPointOfView(t *testing.T) {
	a, err := tca.Attribute(orderpb.Side_SIDE_SELL, rat(t, "100"), []tca.Slice{{
		OrderID: "sell-1", FilledQuantity: rat(t, "100"), AveragePrice: rat(t, "99.30"),
		ReleaseMark: rat(t, "99.6"), ReleaseBid: rat(t, "99.55"), ReleaseAsk: rat(t, "99.65"),
	}})
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if a.ShortfallBps.Sign() <= 0 {
		t.Fatalf("shortfall_bps = %s for a sell that received BELOW arrival — the sign convention "+
			"is inverted, and a cost report built on it averages a bad buy against a bad sell to "+
			"zero", bps(t, a.ShortfallBps))
	}
	for _, c := range []struct {
		name string
		got  *big.Rat
		want string
	}{
		{"shortfall", a.ShortfallBps, "70.0000000000"},
		{"spread", a.SpreadBps, "5.0000000000"},
		{"timing", a.TimingBps, "40.0000000000"},
		{"impact", a.ImpactBps, "25.0000000000"},
	} {
		if got := bps(t, c.got); got != c.want {
			t.Errorf("%s_bps = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestAttribute_RestingInsideTheSpreadShowsAsNegativeImpact is the case that
// proves the residual carries information rather than absorbing arithmetic. A
// buy that rests and is filled AT THE BID pays no spread it did not have to;
// the quoted half-width is still charged as the spread leg, and the saving
// surfaces as a negative impact.
//
// 10 units bought at 100.90 against a 100.00 arrival: 90 bps total, 10 bps of
// quoted half-width, 100 bps of drift, so impact is −20 bps.
func TestAttribute_RestingInsideTheSpreadShowsAsNegativeImpact(t *testing.T) {
	a, err := tca.Attribute(orderpb.Side_SIDE_BUY, rat(t, "100"), []tca.Slice{{
		OrderID: "passive-1", FilledQuantity: rat(t, "10"), AveragePrice: rat(t, "100.90"),
		ReleaseMark: rat(t, "101.0"), ReleaseBid: rat(t, "100.90"), ReleaseAsk: rat(t, "101.10"),
	}})
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if a.ImpactBps.Sign() >= 0 {
		t.Fatalf("impact_bps = %s for an order filled at the bid it was quoted — a passive fill "+
			"inside the spread must show as a negative residual, or the decomposition cannot "+
			"distinguish making from taking", bps(t, a.ImpactBps))
	}
	if got := bps(t, a.ImpactBps); got != "-20.0000000000" {
		t.Errorf("impact_bps = %s, want -20", got)
	}
}

// TestAttribute_OneUnobservableSliceRefusesTheWholeDecomposition is the
// "UNKNOWN is a third value" rule applied to a cost report. The shortfall is
// still exact; the legs come back UNSET rather than zero.
func TestAttribute_OneUnobservableSliceRefusesTheWholeDecomposition(t *testing.T) {
	a, err := tca.Attribute(orderpb.Side_SIDE_BUY, rat(t, "100"), []tca.Slice{
		{
			OrderID: "slice-a", FilledQuantity: rat(t, "60"), AveragePrice: rat(t, "100.55"),
			ReleaseMark: rat(t, "100.4"), ReleaseBid: rat(t, "100.35"), ReleaseAsk: rat(t, "100.45"),
		},
		{
			// No touch: this instrument was last seen as a trade print, so its
			// width was never observed.
			OrderID: "slice-b", FilledQuantity: rat(t, "40"), AveragePrice: rat(t, "101.30"),
			ReleaseMark: rat(t, "101.0"),
		},
	})
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if a.Quality != tca.QualityTotalOnly {
		t.Fatalf("quality = %s, want TOTAL_ONLY", a.Quality)
	}
	if a.SpreadBps != nil || a.TimingBps != nil || a.ImpactBps != nil {
		t.Fatalf("legs = spread %s / timing %s / impact %s, want all UNSET — a zero spread claims "+
			"the fund crossed for free and pushes every unexplained basis point into impact, the "+
			"one leg nobody can independently check",
			bps(t, a.SpreadBps), bps(t, a.TimingBps), bps(t, a.ImpactBps))
	}
	if got := bps(t, a.ShortfallBps); got != "85.0000000000" {
		t.Errorf("shortfall_bps = %s, want 85 — the headline needs only arrival and the fills, so "+
			"it survives a missing touch", got)
	}
	if a.Slices != 2 || a.Measured != 1 {
		t.Errorf("slices/measured = %d/%d, want 2/1 — the coverage pair is what tells an operator "+
			"a price-spine gap from a total absence", a.Slices, a.Measured)
	}
}

// TestAttribute_ACrossedTouchIsNotAWidth: ask <= bid is a book nobody saw in one
// consistent state, and a negative half-spread would report the fund being PAID
// to take liquidity.
func TestAttribute_ACrossedTouchIsNotAWidth(t *testing.T) {
	a, err := tca.Attribute(orderpb.Side_SIDE_BUY, rat(t, "100"), []tca.Slice{{
		OrderID: "crossed", FilledQuantity: rat(t, "10"), AveragePrice: rat(t, "100.50"),
		ReleaseMark: rat(t, "100.4"), ReleaseBid: rat(t, "100.50"), ReleaseAsk: rat(t, "100.40"),
	}})
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if a.Quality != tca.QualityTotalOnly || a.SpreadBps != nil {
		t.Fatalf("quality = %s, spread = %s — a crossed quote was accepted as a width, and it "+
			"produces a NEGATIVE cost of crossing", a.Quality, bps(t, a.SpreadBps))
	}
}

// TestAttribute_AnUntradedSliceDoesNotDegradeTheDecomposition. A child admitted
// and withdrawn before it traded is the ordinary outcome of cancelling a worked
// parent. It has nothing to measure, so counting it as unmeasurable would report
// a price-spine gap that does not exist.
func TestAttribute_AnUntradedSliceDoesNotDegradeTheDecomposition(t *testing.T) {
	a, err := tca.Attribute(orderpb.Side_SIDE_BUY, rat(t, "100"), []tca.Slice{
		{
			OrderID: "traded", FilledQuantity: rat(t, "10"), AveragePrice: rat(t, "100.90"),
			ReleaseMark: rat(t, "101.0"), ReleaseBid: rat(t, "100.90"), ReleaseAsk: rat(t, "101.10"),
		},
		{OrderID: "withdrawn", FilledQuantity: new(big.Rat)},
	})
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if a.Quality != tca.QualityDecomposed {
		t.Fatalf("quality = %s, want DECOMPOSED — a slice that never traded is not an "+
			"unmeasurable one", a.Quality)
	}
	if a.Slices != 1 {
		t.Errorf("slices = %d, want 1 — only the slice that traded is part of this measurement",
			a.Slices)
	}
}

// TestAttribute_RefusesWhatItCannotMeasure. Both refusals exist so an
// unmeasurable decision is never scored as a zero-cost one, which would drag
// every average toward whichever instruments the price spine covers worst.
func TestAttribute_RefusesWhatItCannotMeasure(t *testing.T) {
	good := []tca.Slice{{
		OrderID: "s", FilledQuantity: rat(t, "10"), AveragePrice: rat(t, "100.90"),
		ReleaseMark: rat(t, "101.0"), ReleaseBid: rat(t, "100.90"), ReleaseAsk: rat(t, "101.10"),
	}}

	if _, err := tca.Attribute(orderpb.Side_SIDE_BUY, nil, good); !errors.Is(err, tca.ErrNoArrivalMark) {
		t.Errorf("no arrival mark: err = %v, want ErrNoArrivalMark", err)
	}
	if _, err := tca.Attribute(orderpb.Side_SIDE_BUY, new(big.Rat), good); !errors.Is(err, tca.ErrNoArrivalMark) {
		t.Errorf("zero arrival mark: err = %v, want ErrNoArrivalMark — a zero benchmark makes "+
			"shortfall infinite, not free", err)
	}
	if _, err := tca.Attribute(orderpb.Side_SIDE_BUY, rat(t, "100"), nil); !errors.Is(err, tca.ErrNoFills) {
		t.Errorf("no slices: err = %v, want ErrNoFills", err)
	}
	if _, err := tca.Attribute(orderpb.Side_SIDE_BUY, rat(t, "100"), []tca.Slice{
		{OrderID: "unpriced", FilledQuantity: rat(t, "10")},
	}); err == nil {
		t.Error("a slice that filled at no recorded price was measured — it would understate the " +
			"decision's size while its shortfall landed nowhere")
	}
}

// TestAttribute_QualityUnknownIsNeverReturned. The zero value exists so an
// Attribution that was never computed cannot be mistaken for one that was.
func TestAttribute_QualityUnknownIsNeverReturned(t *testing.T) {
	var zero tca.Attribution
	if zero.Quality != tca.QualityUnknown {
		t.Fatal("the zero Attribution does not read as UNKNOWN")
	}
	a, err := tca.Attribute(orderpb.Side_SIDE_BUY, rat(t, "100"), []tca.Slice{{
		OrderID: "s", FilledQuantity: rat(t, "10"), AveragePrice: rat(t, "100.90"),
	}})
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if a.Quality == tca.QualityUnknown {
		t.Fatal("a computed attribution came back UNKNOWN, which is the value reserved for one " +
			"that was never computed")
	}
}

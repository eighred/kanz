package costbasis

import (
	"math/big"
	"testing"
)

// THE ONE FOLD, AND THE CASES ITS THREE COPIES DISAGREED ON (#428).
//
// These tests are not new coverage of new code — they are the union of what the
// OMS book, the accounting IBOR and the tv-sync projection each proved
// separately, now asserted once against the implementation all three call. The
// point of consolidating was that three copies had drifted; a consolidation
// without a test of the drifted cases would just pick a winner quietly.

func rat(n int64) *big.Rat { return big.NewRat(n, 1) }

func check(t *testing.T, l *Lot, qty, avg, realized string) {
	t.Helper()
	if got := l.Qty.RatString(); got != qty {
		t.Errorf("qty = %s, want %s", got, qty)
	}
	if got := l.AvgCost.RatString(); got != avg {
		t.Errorf("avg = %s, want %s", got, avg)
	}
	if got := l.Realized.RatString(); got != realized {
		t.Errorf("realized = %s, want %s", got, realized)
	}
}

// BUYING MORE RE-AVERAGES THE COST and realizes nothing — nothing was closed.
func TestFold_SameDirectionReAverages(t *testing.T) {
	l := NewLot()
	Fold(l, rat(10), rat(100))
	Fold(l, rat(10), rat(120))
	// (10·100 + 10·120) / 20 = 110
	check(t, l, "20", "110", "0")
}

// SELLING PART OF A LONG REALIZES AGAINST THE AVERAGE, not against any particular
// earlier fill, and leaves the average of what remains untouched.
func TestFold_PartialCloseRealizesAgainstTheAverage(t *testing.T) {
	l := NewLot()
	Fold(l, rat(10), rat(100))
	Fold(l, rat(-4), rat(130))
	check(t, l, "6", "100", "120") // 4 × (130 − 100)
}

// A SHORT PROFITS WHEN THE PRICE FALLS. The sign of the realized figure is the
// half of this arithmetic that is easy to get backwards and impossible to notice
// afterwards, because the number looks perfectly plausible either way.
func TestFold_ShortProfitsWhenPriceFalls(t *testing.T) {
	l := NewLot()
	Fold(l, rat(-10), rat(100))
	Fold(l, rat(4), rat(80))
	check(t, l, "-6", "100", "80") // 4 × (100 − 80)
}

// CROSSING THROUGH ZERO CLOSES ONLY WHAT WAS OPEN, and the new position opens at
// THIS fill's price. Carrying the old average across a flip would attach a long's
// basis to a short, and every unrealized figure after it would be wrong.
func TestFold_CrossingThroughZeroReBasesAtTheFillPrice(t *testing.T) {
	l := NewLot()
	Fold(l, rat(10), rat(100))
	Fold(l, rat(-15), rat(130))
	// Only the 10 open units realize: 10 × (130 − 100) = 300. The remaining
	// 5 open SHORT at 130.
	check(t, l, "-5", "130", "300")
}

// A LOT THAT NETS TO ZERO HAS NO BASIS. Reporting a stale average against a flat
// position would produce an unrealized P&L for something nobody holds.
func TestFold_FlatHasNoBasis(t *testing.T) {
	l := NewLot()
	Fold(l, rat(10), rat(100))
	Fold(l, rat(-10), rat(130))
	check(t, l, "0", "0", "300")
}

// THE ZERO DIVISOR IS A PANIC, NOT A WRONG NUMBER (#217).
//
// big.Rat.Quo panics on a zero divisor, and the divisor here is DERIVED rather
// than validated. This line crash-looped the estate once. The guard was then
// added to the OMS fold and hand-copied to accounting's — and the copies were
// exactly what the accounting comment warned about: "the next edit to either
// should not have to rediscover which one had the guard."
//
// There is now one place to guard and one test that it is guarded.
func TestFold_ZeroTimesZeroDoesNotPanic(t *testing.T) {
	l := NewLot()
	Fold(l, new(big.Rat), rat(50000)) // flat lot, zero quantity
	check(t, l, "0", "0", "0")
}

// EXACTNESS IS THE WHOLE REASON THIS IS big.Rat. A third at float64 would drift,
// and a basis that drifts is a fund's cost basis that drifts.
func TestFold_StaysExactWhereAFloatWouldNot(t *testing.T) {
	l := NewLot()
	Fold(l, rat(3), big.NewRat(100, 3)) // 33.333... exactly
	if got := l.AvgCost.RatString(); got != "100/3" {
		t.Fatalf("avg = %s, want 100/3 — the fold left the exact domain", got)
	}
	Fold(l, rat(-3), big.NewRat(100, 3))
	if got := l.Realized.RatString(); got != "0" {
		t.Fatalf("round-tripping at the same exact price realized %s, want 0", got)
	}
}

// UNREALIZED IS NIL WHEN UNKNOWABLE, AND THAT IS NOT ZERO. An unmarked position
// has an unknown unrealized P&L; a flat one genuinely has none. A caller
// rendering both as 0 would report a held position as breaking even.
func TestUnrealized_NilIsNotZero(t *testing.T) {
	l := NewLot()
	Fold(l, rat(10), rat(100))

	if got := l.Unrealized(nil); got != nil {
		t.Errorf("Unrealized(no mark) = %v, want nil — unknown is not zero", got)
	}
	if got := l.Unrealized(rat(130)); got == nil || got.RatString() != "300" {
		t.Errorf("Unrealized(130) = %v, want 300", got)
	}

	Fold(l, rat(-10), rat(130))
	if got := l.Unrealized(rat(130)); got != nil {
		t.Errorf("a flat lot reported unrealized %v, want nil", got)
	}
}

// A SHORT'S UNREALIZED HAS THE OPPOSITE SIGN, which falls out of the signed
// quantity rather than from a branch — worth pinning, because a sign error here
// is invisible in a report and inverts a risk figure.
func TestUnrealized_ShortInvertsWithTheSignedQuantity(t *testing.T) {
	l := NewLot()
	Fold(l, rat(-10), rat(100))
	if got := l.Unrealized(rat(80)); got == nil || got.RatString() != "200" {
		t.Errorf("short unrealized at 80 = %v, want 200 (a short profits as price falls)", got)
	}
}

// FEES ARE NOT IN HERE, AND THAT IS THE FIX RATHER THAN AN OMISSION.
//
// tv-sync subtracted the fill's fee from realized P&L; the OMS book and the
// accounting IBOR did not. So "what has this position realized" had two answers
// depending on which surface you looked at, and nothing said so because the fee
// line was buried inside one of three separate copies of the arithmetic.
//
// Fold computes GROSS. A surface that reports net does it on its own visible line
// (services/tv-sync/internal/projection/fold.go). This test pins that the shared
// fold does not quietly acquire a fee opinion later.
func TestFold_ComputesGrossRealizedAndTakesNoFeeOpinion(t *testing.T) {
	l := NewLot()
	Fold(l, rat(10), rat(100))
	Fold(l, rat(-10), rat(110))
	if got := l.Realized.RatString(); got != "100" {
		t.Fatalf("realized = %s, want 100 gross. If a fee were netted in here, every consumer "+
			"would inherit one surface's reporting policy invisibly — which is the drift #428 "+
			"consolidated away", got)
	}
}

// THE FUND'S BASIS IS COST-WEIGHTED, NOT AVERAGED. Two venues holding unequal
// sizes must not contribute equally: a naive mean of the venue averages would
// report a basis nobody paid, and every unrealized figure derived from it would
// be wrong in a direction that depends on which venue happens to be larger.
func TestAggregator_WeightsByQuantityNotByVenue(t *testing.T) {
	a := NewAggregator()

	big10 := NewLot()
	Fold(big10, rat(10), rat(100))
	small1 := NewLot()
	Fold(small1, rat(1), rat(200))

	a.Add(big10)
	a.Add(small1)

	got := a.Lot()
	// (10·100 + 1·200) / 11 = 1200/11 ≈ 109.09 — NOT the 150 a plain mean gives.
	if want := "1200/11"; got.AvgCost.RatString() != want {
		t.Fatalf("aggregate avg = %s, want %s. A plain mean of venue averages (150) reports a "+
			"basis nobody paid", got.AvgCost.RatString(), want)
	}
	if got.Qty.RatString() != "11" {
		t.Errorf("aggregate qty = %s, want 11", got.Qty.RatString())
	}
}

// REALIZED P&L ADDS ACROSS VENUES. It is a historical fact per venue, not a
// weighted quantity — summing is the only correct treatment, and averaging it
// would silently shrink a fund's booked P&L as it added exchanges.
func TestAggregator_SumsRealized(t *testing.T) {
	a := NewAggregator()
	for _, px := range []int64{110, 130} {
		l := NewLot()
		Fold(l, rat(10), rat(100))
		Fold(l, rat(-10), rat(px))
		a.Add(l)
	}
	if got := a.Lot().Realized.RatString(); got != "400" { // 100 + 300
		t.Fatalf("aggregate realized = %s, want 400", got)
	}
}

// A PERFECTLY HEDGED FUND MUST NOT DIVIDE BY ZERO. A long of 10 and a short of 10
// net to zero QUANTITY, so weighting by the signed total would panic in big.Rat.
// Weighting by the absolute quantity is what keeps a hedged book's basis
// meaningful — and this is the case that reaches the guard in an ordinary way
// rather than a contrived one.
func TestAggregator_HedgedAcrossVenuesDoesNotPanic(t *testing.T) {
	a := NewAggregator()
	long := NewLot()
	Fold(long, rat(10), rat(100))
	short := NewLot()
	Fold(short, rat(-10), rat(120))
	a.Add(long)
	a.Add(short)

	got := a.Lot()
	if got.Qty.Sign() != 0 {
		t.Fatalf("hedged qty = %s, want 0", got.Qty.RatString())
	}
	// (10·100 + 10·120) / 20 = 110 — a real basis across both legs.
	if want := "110"; got.AvgCost.RatString() != want {
		t.Fatalf("hedged avg = %s, want %s", got.AvgCost.RatString(), want)
	}
}

// AN EMPTY AGGREGATE IS FLAT WITH NO BASIS, not a division by zero.
func TestAggregator_EmptyIsFlat(t *testing.T) {
	got := NewAggregator().Lot()
	check(t, got, "0", "0", "0")
}

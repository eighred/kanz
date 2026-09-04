package tca

import (
	"math/big"
	"testing"
	"time"
)

// THE ARITHMETIC OF A REALISED PARTICIPATION FIGURE (#1007).
//
// The service-level proof — a POV parent worked against a tape thinner than the
// forecast it was scheduled on, with the exceedance published and counted — is
// services/oms/internal/order/participation_test.go. This file proves what that
// one cannot: the three-value quality model, and that an interval nobody could
// see never becomes a rate.

var partT0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// fixedVolume answers a fixed quantity per interval index, and UNKNOWN for any
// index it has no entry for — which is what a fold does for a minute with no
// candle behind it.
type fixedVolume struct {
	byIndex map[int]int64
	// asked records what was looked up, so a test can assert the source was
	// consulted for the series the order actually traded on rather than for
	// whatever the caller had to hand.
	asked []string
}

func (f *fixedVolume) Volume(instrumentID, venue string, from, _ time.Time) (*big.Rat, bool) {
	f.asked = append(f.asked, instrumentID+"@"+venue)
	i := int(from.Sub(partT0) / time.Hour)
	v, ok := f.byIndex[i]
	if !ok {
		return nil, false
	}
	return new(big.Rat).SetInt64(v), true
}

// hourly builds n one-hour intervals starting at partT0, filling each with the
// quantity given (0 meaning the slice traded nothing).
func hourly(venue string, filled ...int64) []Interval {
	out := make([]Interval, 0, len(filled))
	for i, q := range filled {
		in := Interval{
			Index: i,
			Venue: venue,
			From:  partT0.Add(time.Duration(i) * time.Hour),
			To:    partT0.Add(time.Duration(i+1) * time.Hour),
		}
		if q != 0 {
			in.Filled = new(big.Rat).SetInt64(q)
		}
		out = append(out, in)
	}
	return out
}

func ratStr(r *big.Rat) string {
	if r == nil {
		return "<nil>"
	}
	return r.RatString()
}

// THE WORST INTERVAL IS THE ANSWER, NOT THE AVERAGE.
//
// This is the whole reason max_slice_participation_rate exists beside the
// aggregate. The decision below is a comfortable 6% of the window overall and was
// 50% of one dead hour — and a cap says "no child is ever more than n% of its
// interval", so the aggregate would report a control that held while the control
// did not.
func TestParticipation_TheWorstIntervalIsCarriedBesideTheAggregate(t *testing.T) {
	src := &fixedVolume{byIndex: map[int]int64{0: 1000, 1: 20}}
	got := MeasureParticipation("BTC-USD", hourly("XSIM", 50, 10), src)

	if got.Quality != ParticipationMeasured {
		t.Fatalf("quality = %s, want MEASURED — both intervals had volume behind them", got.Quality)
	}
	// 60 filled over 1,020 printed.
	if s := ratStr(got.Overall); s != "1/17" {
		t.Errorf("overall = %s, want 1/17 (60 over 1020)", s)
	}
	if s := ratStr(got.MaxSlice); s != "1/2" {
		t.Errorf("max slice = %s, want 1/2 — 10 units into an hour that printed 20", s)
	}
	if got.MaxSliceIndex != 1 {
		t.Errorf("max slice index = %d, want 1", got.MaxSliceIndex)
	}
	if got.MaxSliceVenue != "XSIM" {
		t.Errorf("max slice venue = %q, want XSIM — an exceedance names a BOOK, and an alert "+
			"labelled by nothing cannot be routed to whoever trades it", got.MaxSliceVenue)
	}
	// AND THE AGGREGATE WOULD HAVE PASSED. Stated as an assertion rather than as
	// a comment, because it is the argument for carrying both numbers: if this
	// ever stops being true the second field has no reason to exist.
	if got.Overall.Cmp(big.NewRat(1, 10)) > 0 {
		t.Errorf("the aggregate %s is above 10%%, so this fixture no longer demonstrates a "+
			"decision that passes on average and breached in one interval", ratStr(got.Overall))
	}
	if !got.Exceeds(big.NewRat(1, 10)) {
		t.Error("a 50% interval did not exceed a 10% cap — the maximum is not being compared, " +
			"and a participation cap this platform reports as honoured would be one nothing checked")
	}
}

// AN INTERVAL WITH NO CANDLE LEAVES BOTH SUMS, AND IS COUNTED AS COVERAGE.
//
// The numerator must not keep a fill whose denominator was dropped: that would
// inflate the reported rate by exactly the size of the coverage gap, so the
// measurement would get worse the less of the tape the platform could see — and
// worse in the direction that manufactures breaches.
func TestParticipation_AnUnseenIntervalLeavesBothSums(t *testing.T) {
	src := &fixedVolume{byIndex: map[int]int64{0: 1000}} // interval 1 has no candle
	got := MeasureParticipation("BTC-USD", hourly("XSIM", 50, 10), src)

	if got.Quality != ParticipationPartial {
		t.Fatalf("quality = %s, want PARTIAL — one of two traded intervals was observable", got.Quality)
	}
	if got.Intervals != 2 || got.Measured != 1 {
		t.Errorf("coverage = %d of %d, want 1 of 2 — the pair is what tells a reader how much of "+
			"the decision the rate covers", got.Measured, got.Intervals)
	}
	if s := ratStr(got.Overall); s != "1/20" {
		t.Errorf("overall = %s, want 1/20 (50 over 1000) — the unseen interval's 10 units must "+
			"leave the numerator with its denominator, or the rate is inflated by the gap", s)
	}
	if s := ratStr(got.MaxSlice); s != "1/20" {
		t.Errorf("max slice = %s, want 1/20 — only the observed interval may produce a maximum", s)
	}
}

// PARTIAL IS ITS OWN VERDICT, AND A BREACH SURVIVES IT.
//
// This is the deliberate departure from AttributionQuality's all-or-nothing
// shape, and the case that argues for it: one interval of two lost its candle and
// the OTHER one was measured at 50% against a 10% cap. Collapsing PARTIAL into
// UNOBSERVABLE would discard a real, observed exceedance because a neighbouring
// minute was quiet — silencing the alarm with exactly the market thinness that
// caused both.
func TestParticipation_APartialMeasurementStillReportsABreach(t *testing.T) {
	src := &fixedVolume{byIndex: map[int]int64{1: 20}} // interval 0 has no candle
	got := MeasureParticipation("BTC-USD", hourly("XSIM", 50, 10), src)

	if got.Quality != ParticipationPartial {
		t.Fatalf("quality = %s, want PARTIAL", got.Quality)
	}
	if !got.Exceeds(big.NewRat(1, 10)) {
		t.Fatal("a measured 50% interval did not report a breach because a DIFFERENT interval " +
			"had no candle. The unseen minute cannot un-observe the seen one, and a cap that is " +
			"only checked when coverage is perfect is a cap nobody checks on a thin instrument")
	}
}

// NOTHING OBSERVABLE IS UNOBSERVABLE, WITH NO RATES AT ALL.
//
// Not a zero, and not a "0 of 2 measured" record carrying a rate anyway. A cap
// this decision was working under has been neither confirmed nor contradicted,
// and that is the claim the record must make.
func TestParticipation_NoCandlesAtAllIsUnobservable(t *testing.T) {
	got := MeasureParticipation("BTC-USD", hourly("XSIM", 50, 10), &fixedVolume{byIndex: map[int]int64{}})

	if got.Quality != ParticipationUnobservable {
		t.Fatalf("quality = %s, want UNOBSERVABLE", got.Quality)
	}
	if got.Overall != nil || got.MaxSlice != nil {
		t.Errorf("rates are %s / %s, want both UNSET — a rate nobody could compute must not "+
			"arrive as a number", ratStr(got.Overall), ratStr(got.MaxSlice))
	}
	if got.MaxSliceIndex != -1 {
		t.Errorf("max slice index = %d, want -1 — index 0 is a real slice and would send an "+
			"investigation to the first interval of every unmeasurable decision", got.MaxSliceIndex)
	}
	if got.Exceeds(big.NewRat(1, 100)) {
		t.Error("an unobservable measurement reported a breach of a 1% cap. Every one of these " +
			"is supposed to be an event a desk can point at; a breach derived from no observation " +
			"is a page nobody can act on")
	}
}

// A ZERO DENOMINATOR IS UNKNOWN, NOT AN INFINITE RATE AND NOT A ZERO ONE.
//
// A source claiming it saw the interval and that nothing printed in it, while
// this decision filled into it, is either a feed that was not running or the most
// extreme participation event there is — and nothing can tell those apart. The
// division must not happen at all.
func TestParticipation_AZeroDenominatorIsNotADenominator(t *testing.T) {
	got := MeasureParticipation("BTC-USD", hourly("XSIM", 50), zeroVolume{})

	if got.Quality != ParticipationUnobservable {
		t.Fatalf("quality = %s, want UNOBSERVABLE — a volume of zero is not a denominator", got.Quality)
	}
	if got.Overall != nil || got.MaxSlice != nil {
		t.Fatalf("rates are %s / %s from a zero-volume interval", ratStr(got.Overall), ratStr(got.MaxSlice))
	}
}

type zeroVolume struct{}

func (zeroVolume) Volume(string, string, time.Time, time.Time) (*big.Rat, bool) {
	return new(big.Rat), true
}

// A SLICE THAT NEVER TRADED IS NOT AN INTERVAL, AND NOT A COVERAGE GAP.
//
// A child admitted and cancelled before it filled participated in nothing.
// Counting it would put a zero into a maximum whose job is to find the WORST
// interval, and counting it as unmeasured would report a market-data gap that
// does not exist.
func TestParticipation_AnUnfilledSliceIsNotCounted(t *testing.T) {
	src := &fixedVolume{byIndex: map[int]int64{0: 1000, 1: 20}}
	got := MeasureParticipation("BTC-USD", hourly("XSIM", 50, 0), src)

	if got.Quality != ParticipationMeasured {
		t.Fatalf("quality = %s, want MEASURED", got.Quality)
	}
	if got.Intervals != 1 || got.Measured != 1 {
		t.Errorf("coverage = %d of %d, want 1 of 1 — the empty slice is not an interval this "+
			"decision traded in", got.Measured, got.Intervals)
	}
	if s := ratStr(got.MaxSlice); s != "1/20" {
		t.Errorf("max slice = %s, want 1/20 — the unfilled slice must not contribute a 0/20 that "+
			"outranks nothing but dilutes the record", s)
	}
	if len(src.asked) != 1 {
		t.Errorf("the source was consulted %d times, want 1 — asking about an interval that "+
			"traded nothing spends a lookup to learn something that cannot change the answer",
			len(src.asked))
	}
}

// AN UNSLICED DECISION IS NOT_WORKED, WHICH IS NOT A MEASUREMENT GAP.
//
// An ordinary order is one message at one instant; "what fraction of the tape was
// it" is a question about a working window it never had. Reporting it as
// UNOBSERVABLE would make an estate that simply trades most of its orders whole
// look like one whose market-data feed is broken.
func TestParticipation_AnUnslicedDecisionIsNotWorked(t *testing.T) {
	got := MeasureParticipation("BTC-USD", nil, &fixedVolume{byIndex: map[int]int64{0: 1000}})

	if got.Quality != ParticipationNotWorked {
		t.Fatalf("quality = %s, want NOT_WORKED", got.Quality)
	}
	if got.Intervals != 0 || got.Overall != nil {
		t.Errorf("intervals = %d, overall = %s, want 0 and unset", got.Intervals, ratStr(got.Overall))
	}
}

// A NIL SOURCE IS UNOBSERVABLE, NOT A PANIC AND NOT A ZERO.
//
// An OMS wired to no candle spine measures nothing, and it must say so in the
// same vocabulary a live feed uses for a market it cannot see — "nothing
// configured" and "checked, and nothing is known" reach the record identically
// here on purpose, and the deployment gap is visible in the quality counter
// rather than as a suspiciously calm rate.
func TestParticipation_ANilSourceIsUnobservable(t *testing.T) {
	got := MeasureParticipation("BTC-USD", hourly("XSIM", 50), nil)
	if got.Quality != ParticipationUnobservable {
		t.Fatalf("quality = %s, want UNOBSERVABLE from a nil source", got.Quality)
	}
}

// THE SOURCE IS ASKED ABOUT THE BOOK THE SLICE WAS WORKED ON.
//
// A parent that names no venue leaves each child to the router's choice, so one
// decision can be worked across two books. Dividing an OKX fill by Binance's
// candles is a denominator that never carried it, and the resulting rate is
// wrong in whichever direction the two books' volumes happen to differ.
func TestParticipation_EachIntervalIsMeasuredAgainstItsOwnVenue(t *testing.T) {
	src := &fixedVolume{byIndex: map[int]int64{0: 1000, 1: 20}}
	ivs := hourly("", 50, 10)
	ivs[0].Venue, ivs[1].Venue = "BINANCE", "OKX"

	MeasureParticipation("BTC-USD", ivs, src)

	want := []string{"BTC-USD@BINANCE", "BTC-USD@OKX"}
	if len(src.asked) != 2 || src.asked[0] != want[0] || src.asked[1] != want[1] {
		t.Errorf("the source was asked %v, want %v — a per-decision venue would measure both "+
			"slices against one book", src.asked, want)
	}
}

// A CAP THAT IS NOT A CAP IS NOT EXCEEDED.
//
// pov.go refuses a cap outside (0, 1] at admission for the same reason, so
// nothing that reaches here should carry one — and if something does, the answer
// must be "there is nothing to exceed" rather than a breach counted against a
// number nobody set.
func TestParticipation_AnAbsentOrDegenerateCapIsNeverExceeded(t *testing.T) {
	got := MeasureParticipation("BTC-USD", hourly("XSIM", 50), &fixedVolume{byIndex: map[int]int64{0: 60}})
	if got.Quality != ParticipationMeasured {
		t.Fatalf("precondition: quality = %s, want MEASURED", got.Quality)
	}
	for _, c := range []struct {
		name string
		cap  *big.Rat
	}{
		{"absent", nil},
		{"zero", new(big.Rat)},
		{"negative", big.NewRat(-1, 2)},
	} {
		if got.Exceeds(c.cap) {
			t.Errorf("a %s cap was reported as exceeded — a breach must name a control somebody "+
				"actually set", c.name)
		}
	}
}

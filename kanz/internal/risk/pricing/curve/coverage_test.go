package curve

import (
	"context"
	"testing"
	"time"
)

// THE DEFECT, STATED AS A TEST (#908).
//
// Two curves, same three surviving quotes. One came from a three-instrument
// strip that is complete; the other from a nine-instrument strip that lost its
// six longest tenors. Before this change the two artifacts were identical in
// every observable respect — same pillars, same zeros, same store entry, same
// log. If this test ever passes with the coverage stripped out, the artifacts
// are indistinguishable again and the defect is back.
func TestAShortStripIsADistinguishableCurve(t *testing.T) {
	ctx := context.Background()
	asOf := time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC)
	quotes := []RateQuote{
		{Kind: Deposit, Tenor: 0.25, Value: 0.043},
		{Kind: Swap, Tenor: 1, Value: 0.041},
		{Kind: Swap, Tenor: 2, Value: 0.040},
	}

	whole, _ := refreshWith(t, staticQuotes{qs: quotes, cov: StripCoverage{Configured: 3, Quoted: 3}}, asOf)
	short, shortStore := refreshWith(t, staticQuotes{qs: quotes, cov: StripCoverage{
		Configured: 9, Quoted: 3,
		Missing: []MissingQuote{
			{InstrumentID: "USD-SWAP-5Y", Reason: MissingNoQuote},
			{InstrumentID: "USD-SWAP-7Y", Reason: MissingNoQuote},
			{InstrumentID: "USD-SWAP-10Y", Reason: MissingNoQuote},
			{InstrumentID: "USD-SWAP-15Y", Reason: MissingUnusableMid},
			{InstrumentID: "USD-SWAP-20Y", Reason: MissingNoQuote},
			{InstrumentID: "USD-SWAP-30Y", Reason: MissingNoQuote},
		},
	}}, asOf)

	// The two price identically — that is the premise, not a bug. The curve is
	// the same curve; what differs is whether it is the curve that was ordered.
	if whole.Discount(30) != short.Discount(30) {
		t.Fatal("premise broken: the two curves are built from the same quotes and must price " +
			"the same. The finding is that they are NOT the same ARTIFACT")
	}

	wc, ok := whole.StripCoverage()
	if !ok {
		t.Fatal("a curve calibrated through Refresh must report its strip coverage")
	}
	if !wc.Complete() {
		t.Fatalf("a complete strip must report complete: %+v", wc)
	}

	sc, ok := short.StripCoverage()
	if !ok {
		t.Fatal("the SHORT curve reports no coverage at all — it is again indistinguishable " +
			"from a complete one, which is #908")
	}
	if sc.Complete() {
		t.Fatalf("a 3-of-9 strip reported COMPLETE: %+v", sc)
	}
	if sc.Configured != 9 || sc.Quoted != 3 {
		t.Fatalf("coverage = %d/%d, want 3/9 — the numbers the operator reads", sc.Quoted, sc.Configured)
	}
	if len(sc.Missing) != 6 {
		t.Fatalf("Missing names %d instruments, want all 6. The list is COMPLETE, not a sample: "+
			"the strip is bounded by configuration and the id is what somebody goes and looks at",
			len(sc.Missing))
	}
	if sc.Missing[0].InstrumentID != "USD-SWAP-5Y" || sc.Missing[0].Reason != MissingNoQuote {
		t.Fatalf("missing entry lost its identity or reason: %+v", sc.Missing[0])
	}

	// And it survives the trip through the point-in-time store, which is the
	// only way any downstream consumer ever sees a curve.
	stored, ok := shortStore.Curve(ctx, "USD", asOf)
	if !ok {
		t.Fatal("the short curve was not published")
	}
	got, ok := stored.StripCoverage()
	if !ok || got.Quoted != 3 || got.Configured != 9 {
		t.Fatalf("the store handed back a curve that cannot say what it was built from: %+v (ok=%v). "+
			"A caller resolving a curve at an arbitrary as-of, days later, is the reader that "+
			"cannot be reached by a log line", got, ok)
	}
}

// refreshWith calibrates one currency through the real Calibrator and returns
// both the curve it produced and the store it published into — the store,
// because a downstream consumer only ever sees a curve it resolved out of one.
func refreshWith(t *testing.T, src QuoteSource, asOf time.Time) (*Curve, *Store) {
	t.Helper()
	store := NewStore()
	cal := &Calibrator{Source: src, Store: store, Interp: LinearZero}
	c, err := cal.Refresh(context.Background(), "USD", asOf)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return c, store
}

// A curve that was not built from a declared strip reports UNKNOWN, never
// "complete". Bootstrap and NewZeroCurve have no strip to be short of, and a
// defaulted "everything resolved" there would be the same confident-zero the
// coverage record exists to break.
func TestACurveWithNoStripReportsUnknown(t *testing.T) {
	c, err := Bootstrap([]ParQuote{{Tenor: 1, Rate: 0.04}, {Tenor: 2, Rate: 0.042}}, LinearZero)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if cov, ok := c.StripCoverage(); ok {
		t.Fatalf("a bootstrapped curve claimed strip coverage %+v — it has no strip, so the "+
			"honest answer is UNKNOWN", cov)
	}

	z, err := NewZeroCurve([]float64{1, 5}, []float64{0.04, 0.045}, Continuous, LinearZero)
	if err != nil {
		t.Fatalf("NewZeroCurve: %v", err)
	}
	if _, ok := z.StripCoverage(); ok {
		t.Fatal("a hand-built curve claimed strip coverage")
	}
	// And an unreported coverage is not a completeness claim.
	var zero StripCoverage
	if zero.Reported() || zero.Complete() {
		t.Fatal("the zero StripCoverage reads as reported or complete — it is the UNKNOWN value")
	}
}

// A source that declares no strip must not have one invented for it: the curve
// reports UNKNOWN rather than "3 of 3".
func TestAnUnreportingSourceLeavesCoverageUnknown(t *testing.T) {
	c, _ := refreshWith(t, staticQuotes{qs: []RateQuote{{Kind: Swap, Tenor: 1, Value: 0.04}}},
		time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC))
	if cov, ok := c.StripCoverage(); ok {
		t.Fatalf("coverage %+v was synthesised from a source that declared none", cov)
	}
}

// OnCoverage fires on EVERY refresh — including the one where the whole strip
// is missing and Calibrate refuses, which is the state with no curve to read
// the coverage off and therefore the one that matters most.
func TestOnCoverageFiresWhenCalibrationRefuses(t *testing.T) {
	var seen []StripCoverage
	cal := &Calibrator{
		Source: staticQuotes{cov: StripCoverage{
			Configured: 2, Quoted: 0,
			Missing: []MissingQuote{
				{InstrumentID: "USD-DEP-3M", Reason: MissingNoQuote},
				{InstrumentID: "USD-SWAP-5Y", Reason: MissingUnusableMid},
			},
		}},
		Store:      NewStore(),
		Interp:     LinearZero,
		OnCoverage: func(_ string, cov StripCoverage) { seen = append(seen, cov) },
	}
	if _, err := cal.Refresh(context.Background(), "USD", time.Now()); err == nil {
		t.Fatal("an empty quote set must still refuse to calibrate")
	}
	if len(seen) != 1 {
		t.Fatalf("OnCoverage fired %d times on a refused refresh, want 1. Refresh's error says "+
			"\"no quotes\"; only the observer says WHICH instruments produced it", len(seen))
	}
	if seen[0].Configured != 2 || len(seen[0].Missing) != 2 {
		t.Fatalf("the refused refresh reported %+v, losing the identities", seen[0])
	}
}

// A source ERROR reports no coverage at all. The source could not say what it
// resolved, so a fabricated zero would claim "the whole strip is missing" when
// the truth is that nothing was looked at — a different fault with a different
// owner (the vendor, not the strip).
func TestOnCoverageIsSilentOnASourceError(t *testing.T) {
	fired := 0
	cal := &Calibrator{
		Source:     failingQuotes{},
		Store:      NewStore(),
		Interp:     LinearZero,
		OnCoverage: func(string, StripCoverage) { fired++ },
	}
	if _, err := cal.Refresh(context.Background(), "USD", time.Now()); err == nil {
		t.Fatal("a source error must surface")
	}
	if fired != 0 {
		t.Fatalf("OnCoverage fired %d times for a source that could not answer at all", fired)
	}
}

// OnCoverage fires for a COMPLETE strip too. A signal emitted only on failure
// makes "healthy" and "not reporting" the same series.
func TestOnCoverageFiresOnACompleteStrip(t *testing.T) {
	fired := 0
	cal := &Calibrator{
		Source: staticQuotes{
			qs:  []RateQuote{{Kind: Swap, Tenor: 1, Value: 0.04}},
			cov: StripCoverage{Configured: 1, Quoted: 1},
		},
		Store:      NewStore(),
		Interp:     LinearZero,
		OnCoverage: func(string, StripCoverage) { fired++ },
	}
	if _, err := cal.Refresh(context.Background(), "USD", time.Now()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if fired != 1 {
		t.Fatalf("OnCoverage fired %d times on a healthy refresh, want 1", fired)
	}
}

// A stress applied to a short curve is short of the same instruments. The shift
// path must not launder a degraded curve into an UNKNOWN one.
func TestAShiftCarriesTheStripCoverage(t *testing.T) {
	base, _ := refreshWith(t, staticQuotes{
		qs:  []RateQuote{{Kind: Swap, Tenor: 1, Value: 0.04}, {Kind: Swap, Tenor: 2, Value: 0.042}},
		cov: StripCoverage{Configured: 5, Quoted: 2, Missing: []MissingQuote{{InstrumentID: "USD-SWAP-30Y", Reason: MissingNoQuote}}},
	}, time.Now())

	for name, got := range map[string]*Curve{
		"parallel": Parallel{Bp: 100}.Apply(base),
		"pillar":   base.WithPillarBump(0, 1e-4),
	} {
		cov, ok := got.StripCoverage()
		if !ok {
			t.Fatalf("%s: the shocked curve forgot what its base was built from — a scenario "+
				"result is where a short curve matters most", name)
		}
		if cov.Complete() || cov.Configured != 5 || cov.Quoted != 2 {
			t.Fatalf("%s: coverage = %+v, want the base's 2/5", name, cov)
		}
	}
}

// The coverage a caller reads cannot be edited into the curve's own record.
func TestStripCoverageIsCopiedOut(t *testing.T) {
	c, _ := refreshWith(t, staticQuotes{
		qs:  []RateQuote{{Kind: Swap, Tenor: 1, Value: 0.04}},
		cov: StripCoverage{Configured: 2, Quoted: 1, Missing: []MissingQuote{{InstrumentID: "USD-SWAP-5Y", Reason: MissingNoQuote}}},
	}, time.Now())

	cov, _ := c.StripCoverage()
	cov.Missing[0].InstrumentID = "MUTATED"
	again, _ := c.StripCoverage()
	if again.Missing[0].InstrumentID != "USD-SWAP-5Y" {
		t.Fatal("a reader mutated the curve's own record of what it was built from")
	}
}

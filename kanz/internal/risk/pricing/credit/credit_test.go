package credit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/credit"
	"github.com/eighred/kanz/internal/risk/xva"
)

var (
	base = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	ctx  = context.Background()
)

// fakeSource is the QuoteSource seam under test control.
type fakeSource struct {
	quotes   []xva.CDSQuote
	recovery float64
	err      error
	calls    int
}

func (f *fakeSource) CDSQuotes(context.Context, string, time.Time) ([]xva.CDSQuote, float64, error) {
	f.calls++
	return f.quotes, f.recovery, f.err
}

// A realistic upward-sloping spread curve: 100bp at 1y out to 200bp at 10y.
func termStructure() []xva.CDSQuote {
	return []xva.CDSQuote{
		{Tenor: 1, Spread: 0.0100},
		{Tenor: 3, Spread: 0.0140},
		{Tenor: 5, Spread: 0.0165},
		{Tenor: 10, Spread: 0.0200},
	}
}

func newCal(src credit.QuoteSource) (*credit.Calibrator, *credit.Store) {
	st := credit.NewStore()
	return &credit.Calibrator{Source: src, Store: st, Disc: credit.FlatRate(0.03)}, st
}

// The seam end to end: quotes in, a bootstrapped curve in the store, resolvable
// as of the calibration time. This is what #113 asked for — the MODEL already
// existed (xva.BootstrapCDS), the schedulable seam did not.
func TestRefreshBootstrapsAndPublishesPointInTime(t *testing.T) {
	cal, st := newCal(&fakeSource{quotes: termStructure(), recovery: 0.4})

	c, err := cal.Refresh(ctx, "ACME", base)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(c.Hazards) != len(termStructure()) {
		t.Fatalf("got %d hazard segments, want one per quote (%d)", len(c.Hazards), len(termStructure()))
	}
	if c.Recovery != 0.4 {
		t.Errorf("recovery = %v, want the value the SOURCE quoted (0.4)", c.Recovery)
	}

	got, ok := st.Curve(ctx, "ACME", base)
	if !ok {
		t.Fatal("the calibrated curve was not published to the store")
	}
	if got.Survival(5) <= 0 || got.Survival(5) >= 1 {
		t.Errorf("Survival(5) = %v, want a probability strictly between 0 and 1", got.Survival(5))
	}

	// NON-VACUITY: the curve must actually reflect the quotes. A wider spread
	// curve must survive LESS, or the bootstrap output is not reaching the store.
	wide := termStructure()
	for i := range wide {
		wide[i].Spread *= 3
	}
	calWide, stWide := newCal(&fakeSource{quotes: wide, recovery: 0.4})
	if _, err := calWide.Refresh(ctx, "ACME", base); err != nil {
		t.Fatalf("Refresh (wide): %v", err)
	}
	wider, _ := stWide.Curve(ctx, "ACME", base)
	if wider.Survival(5) >= got.Survival(5) {
		t.Errorf("tripling every spread did not lower survival (%v vs %v) — the quotes are not "+
			"reaching the bootstrap", wider.Survival(5), got.Survival(5))
	}
}

// A FAILED CALIBRATION LEAVES THE PREVIOUS CURVE SERVING.
//
// This is the contract that matters most in this package. xva integrates CVA
// against Survival(t), so a curve replaced by nothing — or worse, by a
// zero-hazard one — reports a counterparty that cannot default and drives CVA to
// zero without anything failing. The same stance curve.Refresh and
// volsurface.Refresh take.
func TestAFailedRefreshDoesNotDisturbTheStoredCurve(t *testing.T) {
	src := &fakeSource{quotes: termStructure(), recovery: 0.4}
	cal, st := newCal(src)
	if _, err := cal.Refresh(ctx, "ACME", base); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}
	good, _ := st.Curve(ctx, "ACME", base)
	goodSurvival := good.Survival(5)

	for _, tc := range []struct {
		name string
		mut  func()
	}{
		{"source error", func() { src.err = errors.New("feed down") }},
		{"empty quote set", func() { src.err, src.quotes = nil, nil }},
		{"invalid recovery", func() { src.quotes, src.recovery = termStructure(), 1.5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mut()
			if _, err := cal.Refresh(ctx, "ACME", base.Add(time.Hour)); err == nil {
				t.Fatal("a bad refresh reported success")
			}
			// The original is still there, and still the newest.
			c, ok := st.Curve(ctx, "ACME", base.Add(2*time.Hour))
			if !ok {
				t.Fatal("the previously calibrated curve was removed by a FAILED refresh")
			}
			if c.Survival(5) != goodSurvival {
				t.Errorf("survival changed to %v after a failed refresh (was %v)", c.Survival(5), goodSurvival)
			}
		})
	}
}

// A MISSING CURVE IS REPORTED MISSING, NEVER SUBSTITUTED.
//
// The zero value of xva.CreditCurve has no hazards, so Survival(t) is 1 — "this
// counterparty cannot default", which prices CVA at zero. Returning that instead
// of ok=false would be the never-substitute-zero rule broken in the credit layer.
func TestAnUnknownReferenceIsNotFoundRatherThanRiskFree(t *testing.T) {
	_, st := newCal(&fakeSource{quotes: termStructure(), recovery: 0.4})

	if c, ok := st.Curve(ctx, "NEVER-QUOTED", base); ok {
		t.Fatalf("an unknown reference resolved to a curve (%+v) — a zero-hazard curve reads as "+
			"a counterparty that cannot default, and CVA would be zero", c)
	}
}

// asOf resolution: a curve is not visible before its own effective time, and the
// newest one at or before asOf wins. The whole point of the point-in-time store
// is reproducing a number as of a date.
func TestCurveResolvesTheVersionEffectiveAtAsOf(t *testing.T) {
	cal, st := newCal(&fakeSource{quotes: termStructure(), recovery: 0.4})
	if _, err := cal.Refresh(ctx, "ACME", base); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if _, ok := st.Curve(ctx, "ACME", base.Add(-time.Second)); ok {
		t.Error("a curve resolved BEFORE its effective time")
	}
	if _, ok := st.Curve(ctx, "ACME", base); !ok {
		t.Error("a curve did not resolve at exactly its effective time")
	}

	// A later, wider calibration must win from its own time onwards, while the
	// earlier one still answers earlier questions.
	wide := termStructure()
	for i := range wide {
		wide[i].Spread *= 2
	}
	cal.Source = &fakeSource{quotes: wide, recovery: 0.4}
	later := base.Add(24 * time.Hour)
	if _, err := cal.Refresh(ctx, "ACME", later); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}

	early, _ := st.Curve(ctx, "ACME", base)
	late, _ := st.Curve(ctx, "ACME", later)
	if late.Survival(5) >= early.Survival(5) {
		t.Error("the later, wider calibration did not take effect at its own asOf")
	}
	if got, _ := st.Curve(ctx, "ACME", later.Add(-time.Second)); got.Survival(5) != early.Survival(5) {
		t.Error("the later calibration leaked backwards — a point-in-time read is not reproducible")
	}
}

// Out-of-order publication (a nightly-close backfill arriving behind an intraday
// refresh) must not corrupt the ordering.
func TestOutOfOrderPublicationStaysSorted(t *testing.T) {
	st := credit.NewStore()
	mk := func(h float64) *xva.CreditCurve {
		c := xva.FlatHazard(h, 0.4)
		return &c
	}
	st.Put("ACME", base.Add(48*time.Hour), mk(0.05))
	st.Put("ACME", base, mk(0.01)) // backfill, arriving second
	st.Put("ACME", base.Add(24*time.Hour), mk(0.03))

	for _, tc := range []struct {
		at   time.Time
		want float64
	}{
		{base, 0.01},
		{base.Add(24 * time.Hour), 0.03},
		{base.Add(48 * time.Hour), 0.05},
	} {
		c, ok := st.Curve(ctx, "ACME", tc.at)
		if !ok {
			t.Fatalf("no curve at %v", tc.at)
		}
		if c.Hazards[0] != tc.want {
			t.Errorf("at %v: hazard = %v, want %v", tc.at, c.Hazards[0], tc.want)
		}
	}
}

// Put at the same asOf replaces rather than appending, so a re-run of the same
// calibration does not leave two versions with identical timestamps.
func TestPutAtTheSameAsOfReplaces(t *testing.T) {
	st := credit.NewStore()
	first := xva.FlatHazard(0.01, 0.4)
	second := xva.FlatHazard(0.02, 0.4)
	st.Put("ACME", base, &first)
	st.Put("ACME", base, &second)

	c, ok := st.Curve(ctx, "ACME", base)
	if !ok {
		t.Fatal("no curve")
	}
	if c.Hazards[0] != 0.02 {
		t.Errorf("hazard = %v, want the replacing value 0.02", c.Hazards[0])
	}
}

// RefreshFunc is what lets credit join the SAME scheduler as curve and vol,
// rather than growing a second timer. It must bind the reference entity and
// surface the error, since the scheduler decides what to do with it.
func TestRefreshFuncBindsTheReferenceAndSurfacesErrors(t *testing.T) {
	src := &fakeSource{quotes: termStructure(), recovery: 0.4}
	cal, st := newCal(src)

	fn := cal.RefreshFunc("ACME")
	if err := fn(ctx, base); err != nil {
		t.Fatalf("RefreshFunc: %v", err)
	}
	if src.calls != 1 {
		t.Errorf("source called %d times, want 1", src.calls)
	}
	if _, ok := st.Curve(ctx, "ACME", base); !ok {
		t.Error("RefreshFunc did not publish — the scheduler would tick and store nothing")
	}

	src.err = errors.New("feed down")
	if err := fn(ctx, base.Add(time.Hour)); err == nil {
		t.Error("RefreshFunc swallowed the calibration error — the scheduler would log a success")
	}
}

// Discounting must actually reach the bootstrap. Undiscounted and discounted
// bootstraps of the same quotes give different hazards, so a Disc that is
// ignored is a silently different curve.
func TestDiscountingReachesTheBootstrap(t *testing.T) {
	q, rec := termStructure(), 0.4

	discounted, stD := newCal(&fakeSource{quotes: q, recovery: rec})
	if _, err := discounted.Refresh(ctx, "ACME", base); err != nil {
		t.Fatalf("Refresh (discounted): %v", err)
	}

	stU := credit.NewStore()
	undiscounted := &credit.Calibrator{Source: &fakeSource{quotes: q, recovery: rec}, Store: stU} // Disc nil
	if _, err := undiscounted.Refresh(ctx, "ACME", base); err != nil {
		t.Fatalf("Refresh (undiscounted): %v", err)
	}

	d, _ := stD.Curve(ctx, "ACME", base)
	u, _ := stU.Curve(ctx, "ACME", base)
	if d.Survival(10) == u.Survival(10) {
		t.Error("discounted and undiscounted bootstraps produced identical curves — the " +
			"DiscountSource is not reaching xva.BootstrapCDS")
	}
}

// FlatRate is the named, visible alternative to a nil DiscountSource.
func TestFlatRateDiscountsContinuously(t *testing.T) {
	r := credit.FlatRate(0.05)
	if got := r.Discount(0); got != 1 {
		t.Errorf("Discount(0) = %v, want 1", got)
	}
	if got := r.Discount(1); got <= 0.95 || got >= 0.96 {
		t.Errorf("Discount(1) = %v, want exp(-0.05) ≈ 0.9512", got)
	}
	if r.Discount(10) >= r.Discount(1) {
		t.Error("discount factors must fall with maturity")
	}
}

// References lists what has been calibrated, for a composition root building one
// scheduler Job per entity.
func TestReferencesListsCalibratedEntitiesSorted(t *testing.T) {
	st := credit.NewStore()
	c := xva.FlatHazard(0.01, 0.4)
	for _, r := range []string{"ZETA", "ACME", "MID"} {
		st.Put(r, base, &c)
	}
	got := st.References()
	want := []string{"ACME", "MID", "ZETA"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted)", got, want)
		}
	}
}

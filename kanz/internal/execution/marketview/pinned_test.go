package marketview_test

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution/algo"
	"github.com/eighred/kanz/internal/execution/marketview"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
)

// A PINNED VIEW ANSWERS FROM ONE PUBLISHED CURVE (#897).
//
// THE FIXTURE IS THE STORE SUITE'S, DELIBERATELY. volume_test.go's two sessions
// give a shape of 1/4 in bucket 0 and 3/4 in bucket 24 over a mean 400-unit
// session, so the pinned curve below — 100 and 300 in those bins — is the same
// market expressed the way it crosses the wire. Every expectation is therefore
// comparable between the two views by inspection, which is what makes
// TestPinnedAndStoreBackedViewsAgree an equality rather than a coincidence.

func pinnedShape(t *testing.T) marketview.Shape {
	t.Helper()
	n := volprofile.BucketsPerSession(volprofile.DefaultBucket)
	exp := make([]*big.Rat, n)
	for i := range exp {
		exp[i] = new(big.Rat)
	}
	exp[0] = rat(100)
	exp[24] = rat(300)
	return marketview.Shape{
		InstrumentID: inst,
		Bucket:       volprofile.DefaultBucket,
		Horizon:      volprofile.DefaultHorizon,
		Expected:     exp,
	}
}

func mustPinned(t *testing.T, sh marketview.Shape) *marketview.Pinned {
	t.Helper()
	v, err := marketview.NewPinned(sh, "v-under-test")
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	return v
}

// THE TWO VIEWS INTEGRATE THE SAME CURVE THE SAME WAY.
//
// This is the assertion #897 turns on. A schedule is planned once, on whatever
// pod admitted the order, and re-derived by the child-admission check and by the
// driver — and the quantity comparison in
// services/oms/internal/order.authorizeChild is an EXACT RATIONAL. If the
// store-backed integration and the pinned one differed in any digit, a
// legitimate child would be refused as a forgery, on a parent that then advances
// no further while every screen shows it working.
func TestPinnedAndStoreBackedViewsAgree(t *testing.T) {
	fromStore := mustView(t, twoSessionStore(t, 2), day2)
	fromWire := mustPinned(t, pinnedShape(t))

	for _, tt := range []struct {
		name     string
		from, to time.Time
	}{
		{"the whole busy first bucket", day2, day2.Add(30 * time.Minute)},
		{"half of it", day2, day2.Add(15 * time.Minute)},
		{"the noon bucket", day2.Add(12 * time.Hour), day2.Add(12*time.Hour + 30*time.Minute)},
		{"a whole session", day2, day2.Add(volprofile.Session)},
		{"a quiet stretch", day2.Add(time.Hour), day2.Add(6 * time.Hour)},
		{"an interval spanning midnight", day2.Add(volprofile.Session - 15*time.Minute), day2.Add(volprofile.Session + 15*time.Minute)},
		{"a ragged interval that lines up with no bin", day2.Add(7 * time.Minute), day2.Add(12*time.Hour + 23*time.Minute)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want, wantKnown := fromStore.ExpectedVolume(inst, tt.from, tt.to)
			got, gotKnown := fromWire.ExpectedVolume(inst, tt.from, tt.to)
			if wantKnown != gotKnown {
				t.Fatalf("store says known=%v and the pinned curve says known=%v", wantKnown, gotKnown)
			}
			if !wantKnown {
				return
			}
			if got.Cmp(want) != 0 {
				t.Fatalf("pinned = %s, store = %s — two pods deriving one parent would disagree "+
					"in the last digit and the child would be refused as a forgery",
					got.RatString(), want.RatString())
			}
		})
	}
}

// A PINNED VIEW ANSWERS ABOUT ITS OWN INSTRUMENT AND NOTHING ELSE.
//
// A version addresses ONE (instrument, venue) series. A view that answered beyond
// it would size a child from a curve measured on a different market — the pin's
// failure arrived at from the other end.
func TestPinned_RefusesAnotherInstrument(t *testing.T) {
	v := mustPinned(t, pinnedShape(t))
	for _, other := range []string{"", "ETH-USDT"} {
		got, known := v.ExpectedVolume(other, day2, day2.Add(30*time.Minute))
		if known {
			t.Errorf("answered %s about %q from a curve measured on %s", got.RatString(), other, inst)
		}
		if got != nil {
			t.Errorf("answered UNKNOWN and returned %s — a caller ignoring the flag would read "+
				"that as a market", got.RatString())
		}
	}
}

// EVERY UNKNOWN IS AN UNKNOWN, and none of them is a zero.
func TestPinned_EveryRefusalIsUnknownRatherThanAZero(t *testing.T) {
	v := mustPinned(t, pinnedShape(t))
	for _, tt := range []struct {
		name     string
		from, to time.Time
		about    string
	}{
		{"a backwards interval", day2.Add(time.Hour), day2, "a caller's own bug, not a market"},
		{"a window longer than the history behind the curve",
			day2, day2.Add(volprofile.DefaultHorizon + time.Hour),
			"an extrapolation past everything measured"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, known := v.ExpectedVolume(inst, tt.from, tt.to)
			if known {
				t.Fatalf("answered %s as KNOWN — %s must reach an algorithm as UNKNOWN",
					got.RatString(), tt.about)
			}
			if got != nil {
				t.Errorf("answered UNKNOWN and returned %s", got.RatString())
			}
		})
	}
}

// THE HORIZON COMES OFF THE CURVE, NOT OFF THIS POD.
//
// It bounds how long an interval the shape may be integrated over, and a bound
// read from local configuration would make that refusal a property of the READER:
// two pods deployed with different settings would derive different schedules from
// one order, which is precisely what the pin exists to prevent.
func TestPinned_TheHorizonIsTheCurvesOwn(t *testing.T) {
	sh := pinnedShape(t)
	sh.Horizon = 2 * volprofile.Session
	v := mustPinned(t, sh)

	if _, known := v.ExpectedVolume(inst, day2, day2.Add(volprofile.Session)); !known {
		t.Fatal("a one-session window is UNKNOWN under a two-session horizon")
	}
	if _, known := v.ExpectedVolume(inst, day2, day2.Add(3*volprofile.Session)); known {
		t.Fatal("a three-session window answered under a two-session horizon — the curve's own " +
			"bound was ignored in favour of something else")
	}
}

// A MALFORMED CURVE IS REFUSED AT CONSTRUCTION, NOT ANSWERED AS UNKNOWN.
//
// The two are indistinguishable from a refused order — both produce "no volume
// profile" — so an operator would go looking at the market feed while the fault
// was in a decoder or a composition root.
func TestNewPinned_RefusesACurveThatCouldNeverAnswer(t *testing.T) {
	short := pinnedShape(t)
	short.Expected = short.Expected[:10]

	negative := pinnedShape(t)
	negative.Expected[3] = rat(-1)

	holed := pinnedShape(t)
	holed.Expected[3] = nil

	for _, tt := range []struct {
		name    string
		sh      marketview.Shape
		version string
	}{
		{"no instrument", marketview.Shape{Bucket: volprofile.DefaultBucket}, "v1"},
		{"a bucket that does not divide the session",
			marketview.Shape{InstrumentID: inst, Bucket: 7 * time.Minute}, "v1"},
		{"fewer buckets than the session has", short, "v1"},
		{"a negative bucket", negative, "v1"},
		{"a hole where a measured zero should be", holed, "v1"},
		{"no version", pinnedShape(t), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := marketview.NewPinned(tt.sh, tt.version); !errors.Is(err, marketview.ErrShape) {
				t.Fatalf("err = %v, want ErrShape", err)
			}
		})
	}
}

// VWAP OVER A PINNED CURVE PRODUCES THE MEASURED SHAPE, and refuses when the pin
// describes an instrument the plan does not name.
//
// End to end through the seam a driver uses: algo.Run resolves the order's own
// algorithm, the algorithm asks this view, and the view answers from a curve that
// crossed a process boundary rather than from a fold in this process.
func TestVWAPOverAPinnedCurve(t *testing.T) {
	view := mustPinned(t, pinnedShape(t))

	plan := algo.Plan{
		Algo:         algo.NameVWAP,
		InstrumentID: inst,
		Total:        rat(400),
		Start:        day2,
		End:          day2.Add(volprofile.Session),
		Slices:       2,
	}
	got, err := algo.Run(plan, algo.ParentState{OrderID: "p1"}, view)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, want := range []int64{100, 300} {
		if got[i].Quantity.Cmp(rat(want)) != 0 {
			t.Errorf("slice %d = %s, want %d — the schedule is not the published shape",
				i, got[i].Quantity.RatString(), want)
		}
	}

	other := plan
	other.InstrumentID = "ETH-USDT"
	if _, err := algo.Run(other, algo.ParentState{OrderID: "p1"}, view); !errors.Is(err, algo.ErrVolumeUnknown) {
		t.Fatalf("err = %v, want ErrVolumeUnknown — a pin addresses one series and must not size "+
			"an order on another", err)
	}
}

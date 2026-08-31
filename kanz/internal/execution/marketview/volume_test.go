package marketview_test

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution/algo"
	"github.com/eighred/kanz/internal/execution/marketview"
	"github.com/eighred/kanz/internal/marketedge/trades"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
)

// THE BINDING BETWEEN THE VOLUME PROFILE AND THE EXECUTION ALGORITHMS (#869).
//
// AN EXTERNAL TEST PACKAGE ON PURPOSE. Everything below goes through the exported
// surface a driver would use — a store, a venue, an as-of, and MarketView — so
// nothing here can assert a property that an actual caller could not rely on. The
// integration arithmetic is checked through ExpectedVolume rather than against the
// unexported cumulative helpers, for the same reason.
//
// EVERY EXPECTED NUMBER IS DERIVED FROM THE FIXTURE, NOT FROM THE CODE. The
// fixture is two identical sessions in which 100 units print in the first
// half-hour and 300 in the half-hour starting at noon, so the shape is 1/4 in
// bucket 0, 3/4 in bucket 24 and zero everywhere else, and the mean session volume
// is 400. Every expectation below is a hand-evaluated share of those two numbers.

const venue = "XBIT"

var (
	day0 = time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	day1 = day0.Add(volprofile.Session)
	day2 = day1.Add(volprofile.Session)
	inst = "BTC-USDT"
)

func rat(i int64) *big.Rat { return new(big.Rat).SetInt64(i) }

func trade(at time.Time, size int64) trades.Trade {
	return trades.Trade{Price: rat(1), Size: rat(size), EventTime: at}
}

// twoSessionStore folds the fixture and closes both sessions.
//
// THE SECOND SESSION IS CLOSED BY Advance, NOT BY A LATER PRINT, because that is
// what a live feed looks like at the moment a scheduler asks: the day is over and
// nothing newer has arrived. A fixture that closed it with a day-2 print would be
// testing a store one session ahead of the one an order is scheduled against.
func twoSessionStore(t *testing.T, minSessions int) *volprofile.Store {
	t.Helper()
	s, err := volprofile.New(volprofile.Config{MinSessions: minSessions})
	if err != nil {
		t.Fatalf("volprofile.New: %v", err)
	}
	ser := volprofile.Series{InstrumentID: inst, Venue: venue}
	for _, start := range []time.Time{day0, day1} {
		s.Observe(ser, trade(start.Add(10*time.Minute), 100))
		s.Observe(ser, trade(start.Add(12*time.Hour+10*time.Minute), 300))
	}
	s.Advance(day2)
	return s
}

func mustView(t *testing.T, s *volprofile.Store, asOf time.Time) *marketview.Volume {
	t.Helper()
	v, err := marketview.NewVolume(s, venue, asOf)
	if err != nil {
		t.Fatalf("NewVolume: %v", err)
	}
	return v
}

// ===== THE INTEGRATION =====

// THE EXPECTED VOLUME IS THE SHAPE'S SHARE OF THE LEVEL, INTEGRATED OVER THE
// INTERVAL.
//
// The part-bucket cases are the ones worth having: a slice interval is the
// parent's window divided by its slice count and will not line up with a
// half-hour bin, so an implementation that rounded to the nearest bin would give
// two different slices the same answer and size them identically.
func TestExpectedVolume_IsTheShapesShareOfTheLevel(t *testing.T) {
	v := mustView(t, twoSessionStore(t, 2), day2)

	for _, tt := range []struct {
		name     string
		from, to time.Time
		want     string // an exact rational, evaluated by hand from the fixture
	}{
		{"the whole busy first bucket", day2, day2.Add(30 * time.Minute), "100"},
		{"half of it", day2, day2.Add(15 * time.Minute), "50"},
		{"the noon bucket", day2.Add(12 * time.Hour), day2.Add(12*time.Hour + 30*time.Minute), "300"},
		{"a whole session", day2, day2.Add(volprofile.Session), "400"},
		{"a quiet stretch", day2.Add(time.Hour), day2.Add(6 * time.Hour), "0"},
		{"the second half of the first bucket plus a silent one", day2.Add(15 * time.Minute), day2.Add(45 * time.Minute), "50"},
		{"an interval spanning midnight", day2.Add(volprofile.Session - 15*time.Minute), day2.Add(volprofile.Session + 15*time.Minute), "50"},
		{"an interval covering both busy bins", day2, day2.Add(13 * time.Hour), "400"},
		{"a zero-length interval expects nothing", day2, day2, "0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, known := v.ExpectedVolume(inst, tt.from, tt.to)
			if !known {
				t.Fatalf("answered UNKNOWN for an interval the profile covers")
			}
			want, _ := new(big.Rat).SetString(tt.want)
			if got.Cmp(want) != 0 {
				t.Errorf("expected volume = %s, want %s", got.RatString(), want.RatString())
			}
		})
	}
}

// THE PARTS OF A WINDOW SUM TO THE WHOLE. A schedule asks one question per slice
// and the answers must add up to the answer for the window, or a parent worked as
// six slices is sized against a different market from the same parent worked as
// three.
func TestExpectedVolume_TheSlicesOfAWindowSumToIt(t *testing.T) {
	v := mustView(t, twoSessionStore(t, 2), day2)

	from, to := day2.Add(6*time.Hour), day2.Add(18*time.Hour)
	whole, known := v.ExpectedVolume(inst, from, to)
	if !known {
		t.Fatal("the whole window is UNKNOWN")
	}

	for _, slices := range []int{2, 3, 5, 7, 13} {
		span := to.Sub(from)
		sum := new(big.Rat)
		for i := range slices {
			a := from.Add(time.Duration(int64(span) * int64(i) / int64(slices)))
			b := from.Add(time.Duration(int64(span) * int64(i+1) / int64(slices)))
			part, ok := v.ExpectedVolume(inst, a, b)
			if !ok {
				t.Fatalf("%d slices: part %d is UNKNOWN", slices, i)
			}
			sum.Add(sum, part)
		}
		if sum.Cmp(whole) != 0 {
			t.Errorf("%d slices sum to %s, the whole window is %s", slices, sum.RatString(), whole.RatString())
		}
	}
}

// ===== EVERY UNKNOWN IS AN UNKNOWN =====

// A VERDICT THAT IS NOT KNOWN BECOMES known=false, WITH NO ZERO ANYWHERE.
//
// This is the retirement condition the dark-capability exemption for volprofile
// named: an algorithm that calls Store.Profile and ACTS ON THE VERDICT. Each row
// below is one of that type's non-KNOWN verdicts, or a read it refuses outright,
// and every one of them must arrive at an algorithm as UNKNOWN rather than as a
// number.
func TestExpectedVolume_EveryNonKnownVerdictIsUnknown(t *testing.T) {
	for _, tt := range []struct {
		name  string
		view  func(t *testing.T) *marketview.Volume
		inst  string
		from  time.Time
		to    time.Time
		about string
	}{
		{
			name:  "a series nobody has folded",
			view:  func(t *testing.T) *marketview.Volume { return mustView(t, twoSessionStore(t, 2), day2) },
			inst:  "ETH-USDT",
			about: "VerdictAbsent",
		},
		{
			name:  "fewer sessions than the desk requires",
			view:  func(t *testing.T) *marketview.Volume { return mustView(t, twoSessionStore(t, 5), day2) },
			inst:  inst,
			about: "VerdictTooFewSessions",
		},
		{
			name: "a shape older than the retention horizon",
			view: func(t *testing.T) *marketview.Volume {
				return mustView(t, twoSessionStore(t, 2), day2.Add(volprofile.DefaultHorizon+volprofile.Session))
			},
			inst:  inst,
			about: "VerdictStale",
		},
		{
			name: "an as-of that would look ahead",
			view: func(t *testing.T) *marketview.Volume {
				return mustView(t, twoSessionStore(t, 2), day1.Add(time.Hour))
			},
			inst:  inst,
			about: "ErrLookahead",
		},
		{
			name:  "another venue's book",
			view:  func(t *testing.T) *marketview.Volume { return mustViewOn(t, twoSessionStore(t, 2), "XOTHER", day2) },
			inst:  inst,
			about: "a profile keyed by a venue this view does not name",
		},
		{
			name:  "an unnamed instrument",
			view:  func(t *testing.T) *marketview.Volume { return mustView(t, twoSessionStore(t, 2), day2) },
			inst:  "",
			about: "a question about nothing",
		},
		{
			name:  "a backwards interval",
			view:  func(t *testing.T) *marketview.Volume { return mustView(t, twoSessionStore(t, 2), day2) },
			inst:  inst,
			from:  day2.Add(time.Hour),
			to:    day2,
			about: "a caller's own bug, not a market",
		},
		{
			name:  "a window longer than the retained history",
			view:  func(t *testing.T) *marketview.Volume { return mustView(t, twoSessionStore(t, 2), day2) },
			inst:  inst,
			from:  day2,
			to:    day2.Add(volprofile.DefaultHorizon + time.Hour),
			about: "an extrapolation past everything measured",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			from, to := tt.from, tt.to
			if from.IsZero() {
				from, to = day2, day2.Add(30*time.Minute)
			}
			got, known := tt.view(t).ExpectedVolume(tt.inst, from, to)
			if known {
				t.Fatalf("answered %s as KNOWN — %s must reach an algorithm as UNKNOWN, or a "+
					"volume-driven order is scheduled against a curve nobody measured",
					got.RatString(), tt.about)
			}
			if got != nil {
				t.Errorf("answered UNKNOWN and returned %s — a caller ignoring the flag would read "+
					"that as a market", got.RatString())
			}
		})
	}
}

func mustViewOn(t *testing.T, s *volprofile.Store, v string, asOf time.Time) *marketview.Volume {
	t.Helper()
	out, err := marketview.NewVolume(s, v, asOf)
	if err != nil {
		t.Fatalf("NewVolume: %v", err)
	}
	return out
}

// A MISCONFIGURED VIEW IS REFUSED AT CONSTRUCTION, NOT ANSWERED AS UNKNOWN.
//
// The two are indistinguishable from a refused order — both produce "no volume
// profile" — so an operator would go looking at the market feed while the fault
// was in the composition root. "Nothing configured" and "checked, and nothing is
// known" must not look the same.
func TestNewVolume_RefusesAViewThatCouldNeverAnswer(t *testing.T) {
	s := twoSessionStore(t, 2)
	for _, tt := range []struct {
		name  string
		store *volprofile.Store
		venue string
		asOf  time.Time
	}{
		{"no store", nil, venue, day2},
		{"no venue", s, "", day2},
		{"no as-of", s, venue, time.Time{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := marketview.NewVolume(tt.store, tt.venue, tt.asOf); !errors.Is(err, marketview.ErrConfig) {
				t.Fatalf("err = %v, want ErrConfig", err)
			}
		})
	}
}

// THE VIEW HOLDS NO BOOK AND SAYS SO. A volume profile does not read a price —
// #867's fold ignores it deliberately — so this is the honest answer rather than
// a stub, and an algorithm needing a book refuses on it.
func TestTopOfBook_IsUnknownBecauseAProfileHoldsNoPrices(t *testing.T) {
	bid, ask, known := mustView(t, twoSessionStore(t, 2), day2).TopOfBook(inst)
	if known || bid != nil || ask != nil {
		t.Fatalf("a volume profile answered a book: bid=%v ask=%v known=%v", bid, ask, known)
	}
}

// ===== THE WHOLE PATH: A PROFILE SCHEDULES AN ORDER, OR REFUSES IT =====

// VWAP SCHEDULED AGAINST A REAL PROFILE, AND REFUSED AGAINST AN ABSENT ONE.
//
// This is the claim the retired dark-capability exemption asks for, end to end
// through the seam: algo.Run resolves the order's own algorithm, the algorithm
// asks this view, the view asks Store.Profile, and the verdict decides whether
// there is a schedule at all.
//
// The window is 06:00 to 13:00 on a fixture day, cut into seven hourly slices. The
// only bin inside it carrying any share is the one at noon (3/4 of a session, so
// 300 of the mean 400), and it lands wholly in slice 6 — so the expectation is
// arithmetic nobody has to run the code to check: six slices expect nothing and
// the plan is REFUSED, because a child of zero quantity cannot be placed.
func TestVWAPOverARealProfile(t *testing.T) {
	store := twoSessionStore(t, 2)
	view := mustView(t, store, day2)

	windowStart := day2.Add(11 * time.Hour)
	plan := algo.Plan{
		Algo:         algo.NameVWAP,
		InstrumentID: inst,
		Total:        rat(400),
		Start:        windowStart,
		End:          windowStart.Add(2 * time.Hour),
		Slices:       4, // 30-minute slices: 11:00, 11:30, 12:00, 12:30
	}

	// SLICES 0 AND 1 EXPECT NOTHING, so the plan is refused whole rather than
	// producing two unplaceable children.
	if _, err := algo.Run(plan, algo.ParentState{OrderID: "p1"}, view); !errors.Is(err, algo.ErrNoVolumeInBucket) {
		t.Fatalf("err = %v, want ErrNoVolumeInBucket", err)
	}

	// A WINDOW OVER THE BINS THAT DO TRADE. 00:00-00:30 carries 1/4 and
	// 12:00-12:30 carries 3/4, so a two-slice plan over [00:00, 24:00) works the
	// parent 1/4 then 3/4 — a schedule that is emphatically NOT two equal halves,
	// which is the whole point.
	whole := algo.Plan{
		Algo:         algo.NameVWAP,
		InstrumentID: inst,
		Total:        rat(400),
		Start:        day2,
		End:          day2.Add(volprofile.Session),
		Slices:       2,
	}
	got, err := algo.Run(whole, algo.ParentState{OrderID: "p1"}, view)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, want := range []int64{100, 300} {
		if got[i].Quantity.Cmp(rat(want)) != 0 {
			t.Errorf("slice %d = %s, want %d — the schedule is not the measured shape",
				i, got[i].Quantity.RatString(), want)
		}
	}
	if algo.Sum(got).Cmp(whole.Total) != 0 {
		t.Errorf("children sum to %s, parent is %s", algo.Sum(got).RatString(), whole.Total.RatString())
	}

	// AND THE SAME ORDER ON AN INSTRUMENT NOBODY HAS FOLDED IS REFUSED, never
	// worked against a flat curve.
	absent := whole
	absent.InstrumentID = "ETH-USDT"
	if _, err := algo.Run(absent, algo.ParentState{OrderID: "p1"}, view); !errors.Is(err, algo.ErrVolumeUnknown) {
		t.Fatalf("err = %v, want ErrVolumeUnknown for a series nobody has measured", err)
	}
}

// POV OVER A REAL PROFILE REFUSES A PARENT THAT WOULD BREACH ITS CAP, and works
// the largest one that would not. 400 is the mean session volume, so a 10% cap
// permits at most 40.
func TestPOVOverARealProfile(t *testing.T) {
	view := mustView(t, twoSessionStore(t, 2), day2)

	base := algo.Plan{
		Algo:             algo.NamePOV,
		InstrumentID:     inst,
		Start:            day2,
		End:              day2.Add(volprofile.Session),
		Slices:           2,
		MaxParticipation: new(big.Rat).SetFrac64(1, 10),
	}

	over := base
	over.Total = rat(41)
	if _, err := algo.Run(over, algo.ParentState{OrderID: "p1"}, view); !errors.Is(err, algo.ErrParticipationCapExceeded) {
		t.Fatalf("err = %v, want ErrParticipationCapExceeded", err)
	}

	ok := base
	ok.Total = rat(40)
	got, err := algo.Run(ok, algo.ParentState{OrderID: "p1"}, view)
	if err != nil {
		t.Fatalf("the largest parent the cap permits was refused: %v", err)
	}
	// 10% of each bin: 10 of the 100 expected in the first, 30 of the 300 at noon.
	for i, want := range []int64{10, 30} {
		if got[i].Quantity.Cmp(rat(want)) != 0 {
			t.Errorf("slice %d = %s, want %d", i, got[i].Quantity.RatString(), want)
		}
	}
}

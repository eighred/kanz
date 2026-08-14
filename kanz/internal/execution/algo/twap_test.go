package algo

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// SIZE IS SLIPPAGE (#435).
//
// Until this package existed, every order on this platform went to a venue in one
// message. #435's Verified-when names three assertions; two of them are properties
// of the SCHEDULE and are proved here, closed-form, with no venue and no clock:
//
//	(a) the children sum to exactly N
//	(b) no child exceeds the participation cap
//
// The third — a cancelled parent cancels every unsent child — is a property of the
// order lifecycle, not of the arithmetic, and is proved where that lives.

var (
	start = time.Date(2026, 8, 14, 14, 0, 0, 0, time.UTC)
	end   = time.Date(2026, 8, 14, 15, 0, 0, 0, time.UTC)
)

func plan(total int64, slices int) Plan {
	return Plan{Total: new(big.Rat).SetInt64(total), Start: start, End: end, Slices: slices}
}

// (a) CONSERVATION, AND IT IS THE HARD CASE.
//
// 10 over 3 does not divide. A schedule that rounds each child to any finite
// number of decimal places loses the remainder, and the parent never completes:
// it sits at 9.999999 of 10 filled, forever, with no child left to send and no
// error anywhere. That is the failure this assertion exists to make impossible.
func TestAlgoTWAP_ChildrenSumToTheParentExactlyWhenTheDivisionIsNotClean(t *testing.T) {
	s, err := TWAP(plan(10, 3))
	if err != nil {
		t.Fatalf("TWAP: %v", err)
	}
	if len(s) != 3 {
		t.Fatalf("slices = %d, want 3", len(s))
	}
	want := new(big.Rat).SetInt64(10)
	if got := Sum(s); got.Cmp(want) != 0 {
		t.Fatalf("children sum to %s, want exactly 10 — the remainder was lost, and a parent "+
			"whose children cannot sum to it never completes: it rests part-filled with nothing "+
			"left to send and no error raised anywhere", got.FloatString(20))
	}
	third := big.NewRat(10, 3)
	for _, sl := range s {
		if sl.Quantity.Cmp(third) != 0 {
			t.Errorf("slice %d = %s, want exactly 10/3", sl.Index, sl.Quantity.FloatString(20))
		}
	}
}

// CONSERVATION HOLDS ACROSS SHAPES. A single hand-picked case can pass on an
// implementation that is wrong in general.
func TestAlgoTWAP_ConservationHoldsForManySplits(t *testing.T) {
	for _, total := range []int64{1, 7, 10, 999, 1800} { // 1800: #240's fan-out
		for _, n := range []int{1, 2, 3, 7, 13, 100} {
			s, err := TWAP(plan(total, n))
			if err != nil {
				t.Fatalf("TWAP(%d, %d): %v", total, n, err)
			}
			if got, want := Sum(s), new(big.Rat).SetInt64(total); got.Cmp(want) != 0 {
				t.Errorf("%d over %d slices sums to %s, want exactly %d",
					total, n, got.FloatString(20), total)
			}
		}
	}
}

// (b) NO CHILD EXCEEDS THE CAP.
func TestAlgoTWAP_NoChildExceedsTheCap(t *testing.T) {
	p := plan(100, 10)
	p.MaxSlice = new(big.Rat).SetInt64(10)

	s, err := TWAP(p)
	if err != nil {
		t.Fatalf("TWAP: %v", err)
	}
	for _, sl := range s {
		if sl.Quantity.Cmp(p.MaxSlice) > 0 {
			t.Fatalf("slice %d is %s, over the cap of 10", sl.Index, sl.Quantity.FloatString(8))
		}
	}
}

// A PLAN THAT CANNOT HONOUR THE CAP IS REFUSED WHOLE — no children, and an error
// naming what to change.
//
// The two silent alternatives are both worse than a refusal. Trimming each child
// to the cap leaves the remainder unworked and the parent never completes.
// Quietly raising the slice count executes a schedule nobody chose: a 10-slice
// hour becomes a 100-slice hour, at 10× the message rate, against venue rate
// limits the operator sized for their own plan.
func TestAlgoTWAP_APlanThatCannotHonourTheCapIsRefusedNotTrimmed(t *testing.T) {
	p := plan(1000, 10) // 100 per child
	p.MaxSlice = new(big.Rat).SetInt64(10)

	s, err := TWAP(p)
	if !errors.Is(err, ErrCapUnsatisfiable) {
		t.Fatalf("err = %v, want ErrCapUnsatisfiable — a plan whose children cannot honour the "+
			"cap must be refused, not silently reshaped", err)
	}
	if s != nil {
		t.Errorf("slices = %v, want none: a refused plan must not emit children", s)
	}
	// The refusal must say what would work. "Invalid plan" sends an operator to
	// read this source at the moment they are trying to work an order.
	if !strings.Contains(err.Error(), "at least 100") {
		t.Errorf("err = %q, want it to name the slice count that WOULD honour the cap "+
			"(1000/10 = 100); a refusal an operator cannot act on is a refusal they will "+
			"work around", err)
	}
}

// THE CAP IS INCLUSIVE. A child exactly at the cap is at the cap, not over it —
// and this is the boundary a "must not exceed" bound is decided by. 100 over 10
// slices with a cap of 10 is the single most natural plan an operator can write;
// an off-by-one here refuses it.
func TestAlgoTWAP_AChildExactlyAtTheCapIsAllowed(t *testing.T) {
	p := plan(100, 10)
	p.MaxSlice = new(big.Rat).SetInt64(10)

	if _, err := TWAP(p); err != nil {
		t.Fatalf("a plan whose children are exactly at the cap was refused: %v", err)
	}
}

// THE FIRST SLICE IS DUE AT THE START OF THE WINDOW, and the last is inside it.
//
// Both halves matter. A schedule whose first child is due at start+step does
// nothing for the first interval of a window the operator asked to begin now; a
// schedule whose last child is due AT the end has one child arriving as the
// window closes, which on any real venue means after it.
func TestAlgoTWAP_TheScheduleFillsTheWindowAndStartsAtIt(t *testing.T) {
	s, err := TWAP(plan(60, 6))
	if err != nil {
		t.Fatalf("TWAP: %v", err)
	}
	if !s[0].Due.Equal(start) {
		t.Errorf("first slice due %s, want the window start %s", s[0].Due, start)
	}
	if !s[len(s)-1].Due.Before(end) {
		t.Errorf("last slice due %s, want strictly before the window end %s — a child due as "+
			"the window closes arrives after it", s[len(s)-1].Due, end)
	}
	// Evenly spaced: 6 slices over an hour is one every 10 minutes.
	for i, sl := range s {
		want := start.Add(time.Duration(i) * 10 * time.Minute)
		if !sl.Due.Equal(want) {
			t.Errorf("slice %d due %s, want %s", i, sl.Due, want)
		}
	}
}

// THE SCHEDULE DOES NOT DRIFT OVER A LONG WINDOW.
//
// A step computed once and added repeatedly accumulates its rounding error: a
// day-long window sliced 1,000 ways would push the last children past the close.
// Computing each offset from the START is what keeps the whole schedule inside
// the window the operator asked for.
func TestAlgoTWAP_DoesNotDriftOutOfALongWindow(t *testing.T) {
	p := Plan{
		Total: new(big.Rat).SetInt64(1000),
		Start: start, End: start.Add(24 * time.Hour),
		Slices: 997, // prime: no step divides the span evenly
	}
	s, err := TWAP(p)
	if err != nil {
		t.Fatalf("TWAP: %v", err)
	}
	span := p.End.Sub(p.Start)
	for _, sl := range s {
		if sl.Due.Before(p.Start) || !sl.Due.Before(p.End) {
			t.Fatalf("slice %d due %s, outside the window [%s, %s) — the schedule drifted",
				sl.Index, sl.Due, p.Start, p.End)
		}
		// EXACT, TO THE NANOSECOND. Containment alone is too weak to see this: a
		// truncated step pulls every slice EARLIER, so a drifting schedule stays
		// inside the window while compressing into the front of it — the children
		// finish before the operator's window does, which is the opposite of what
		// they asked for and invisible to a bounds check.
		want := p.Start.Add(time.Duration(int64(span) * int64(sl.Index) / int64(p.Slices)))
		if !sl.Due.Equal(want) {
			t.Fatalf("slice %d due %s, want %s (off by %s) — the offset is being accumulated "+
				"rather than computed from the start, and the error compounds with every slice",
				sl.Index, sl.Due, want, want.Sub(sl.Due))
		}
	}
}

// THE SCHEDULE IS A PURE FUNCTION — the property the whole design rests on.
//
// #435: "An algo that keeps its schedule in memory turns a pod restart into an
// abandoned parent order with children half-sent." The answer here is that there
// is no schedule to lose: a pod that has just booted recomputes byte-identical
// slices from the parent's durable fields. If this ever stops holding, the
// restart story silently stops holding with it.
func TestAlgoTWAP_IsDeterministicSoARestartRecomputesTheSameSchedule(t *testing.T) {
	p := plan(7, 13)
	first, err := TWAP(p)
	if err != nil {
		t.Fatalf("TWAP: %v", err)
	}
	// A DIFFERENT PROCESS, later, holding only what the parent order stores.
	afterRestart, err := TWAP(Plan{
		Total: new(big.Rat).SetInt64(7), Start: start, End: end, Slices: 13,
	})
	if err != nil {
		t.Fatalf("TWAP after restart: %v", err)
	}
	if len(first) != len(afterRestart) {
		t.Fatalf("slice count changed across a restart: %d then %d", len(first), len(afterRestart))
	}
	for i := range first {
		if !first[i].Due.Equal(afterRestart[i].Due) ||
			first[i].Quantity.Cmp(afterRestart[i].Quantity) != 0 {
			t.Fatalf("slice %d differs across a restart: %s@%s then %s@%s — the schedule is not "+
				"derivable from the parent's durable fields, so a restarted pod cannot know what "+
				"the previous one intended to send", i,
				first[i].Quantity.FloatString(8), first[i].Due,
				afterRestart[i].Quantity.FloatString(8), afterRestart[i].Due)
		}
	}
}

// A SLICE IS NOT ALIASED. Handing every child a pointer to one shared big.Rat
// makes a caller that normalises or scales one child silently rewrite all of them
// — and the sum would still assert clean, because it would be recomputed from the
// same mutated value.
func TestAlgoTWAP_SlicesDoNotShareOneQuantity(t *testing.T) {
	s, err := TWAP(plan(100, 4))
	if err != nil {
		t.Fatalf("TWAP: %v", err)
	}
	s[0].Quantity.SetInt64(999)
	if s[1].Quantity.Cmp(new(big.Rat).SetInt64(25)) != 0 {
		t.Fatalf("mutating slice 0 changed slice 1 to %s — the children share one value, so a "+
			"caller that adjusts one child rewrites the whole schedule",
			s[1].Quantity.FloatString(8))
	}
}

// DUE IS A FILTER OVER THE WHOLE SCHEDULE, NOT A CURSOR.
//
// A driver asks "what should have been sent by now" and gets the same answer
// whether it has been running all day or booted a second ago. A cursor would give
// two different answers, and the one from the fresh pod would skip everything the
// old one had already passed.
func TestAlgoDue_ReturnsEverythingUpToNowRegardlessOfHistory(t *testing.T) {
	s, err := TWAP(plan(60, 6)) // one every 10 minutes from 14:00
	if err != nil {
		t.Fatalf("TWAP: %v", err)
	}
	// 14:25 — slices at :00, :10, :20 are due; :30, :40, :50 are not.
	got := Due(s, start.Add(25*time.Minute))
	if len(got) != 3 {
		t.Fatalf("due = %d slices at +25m, want 3", len(got))
	}
	for i, sl := range got {
		if sl.Index != i {
			t.Errorf("due[%d] is slice %d, want %d — Due must not reorder or skip", i, sl.Index, i)
		}
	}
	// A SLICE DUE EXACTLY NOW IS DUE. Excluding it delays every child by one tick
	// of whatever drives this, forever.
	if n := len(Due(s, start)); n != 1 {
		t.Errorf("at exactly the window start, due = %d, want 1: the first slice is due AT the "+
			"start, and excluding it delays every child by one driver tick", n)
	}
	if n := len(Due(s, start.Add(-time.Second))); n != 0 {
		t.Errorf("before the window, due = %d, want 0", n)
	}
	if n := len(Due(s, end.Add(time.Hour))); n != 6 {
		t.Errorf("after the window, due = %d, want all 6", n)
	}
}

// --- refusals ---
//
// Every one of these is a plan that could otherwise put the WHOLE parent in front
// of a venue in one message, which is the exact behaviour #435 exists to end.
// Defaulting any of them is how the fix reintroduces the bug.

func TestAlgoTWAP_RefusesPlansThatCannotBeWorked(t *testing.T) {
	tests := []struct {
		name string
		p    Plan
		want error
	}{
		{"no quantity", Plan{Start: start, End: end, Slices: 4}, ErrEmptyQuantity},
		{"zero quantity", plan(0, 4), ErrEmptyQuantity},
		{"negative quantity", plan(-10, 4), ErrEmptyQuantity},
		{"no slices", plan(10, 0), ErrNoSlices},
		{"negative slices", plan(10, -1), ErrNoSlices},
		{
			// NOT "send it all now". A zero-length window is a schedule nobody
			// specified, and reading it as immediate is how the platform goes back
			// to putting 1,800 BTC in one message.
			"empty window",
			Plan{Total: new(big.Rat).SetInt64(10), Start: start, End: start, Slices: 4},
			ErrEmptyWindow,
		},
		{
			"backwards window",
			Plan{Total: new(big.Rat).SetInt64(10), Start: end, End: start, Slices: 4},
			ErrEmptyWindow,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := TWAP(tt.p)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if s != nil {
				t.Errorf("a refused plan emitted %d children", len(s))
			}
		})
	}
}

// A CAP OF ZERO OR NIL IS NO CAP, and does not refuse everything. An uncapped
// plan is a legitimate one — it is what every order on this platform is today —
// and a cap that defaulted to zero would refuse every plan that omitted it,
// turning the new safety feature into a trading outage.
func TestAlgoTWAP_AnAbsentCapDoesNotRefuseEverything(t *testing.T) {
	for _, cap := range []*big.Rat{nil, new(big.Rat)} {
		p := plan(1000, 2) // 500 per child, which any real cap would refuse
		p.MaxSlice = cap
		if _, err := TWAP(p); err != nil {
			t.Fatalf("an uncapped plan was refused (%v): a cap nobody set must not read as a "+
				"cap of zero", err)
		}
	}
}

// A SINGLE SLICE IS A LEGAL SCHEDULE. It is exactly today's behaviour — the whole
// parent in one child — and it must stay expressible, both so an operator can
// choose it and so the parent/child path can be adopted without changing what
// small orders do.
func TestAlgoTWAP_OneSliceIsTheWholeParent(t *testing.T) {
	s, err := TWAP(plan(10, 1))
	if err != nil {
		t.Fatalf("TWAP: %v", err)
	}
	if len(s) != 1 || s[0].Quantity.Cmp(new(big.Rat).SetInt64(10)) != 0 {
		t.Fatalf("got %d slices %v, want one child of the full 10", len(s), s)
	}
	if !s[0].Due.Equal(start) {
		t.Errorf("the single child is due %s, want the window start", s[0].Due)
	}
}

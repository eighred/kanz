package algo

import (
	"math/big"
	"testing"
	"time"
)

// A LONG SCHEDULE'S DUE TIMES MUST STAY INSIDE ITS WINDOW (#898).
//
// boundary computed `int64(span) * int64(i) / int64(p.Slices)` in 64 bits.
// span is NANOSECONDS, so the product overflows int64 well inside the range of
// the uint32 slice_count that arrives on the wire:
//
//	window     overflows at
//	   1h      i > 2_562_047
//	  24h      i >   106_751
//
// and the threshold falls as the window grows, which is the opposite of the
// intuition that a long window is the safe case.
//
// The consequence is not a slightly-wrong time. The product goes NEGATIVE, the
// offset goes negative, and Start.Add(offset) lands BEFORE the window opens —
// so Due(), which filters on "at or before now", returns EVERY child at once. A
// parent somebody asked to be worked carefully across a day is sent to the venue
// in one pass, which is the failure the window's own proto comment forbids,
// reached by arithmetic rather than by a zero-length window.

// dayWindow is 24h, where the overflow threshold is lowest and most reachable.
func dayWindow() (time.Time, time.Time) {
	start := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	return start, start.Add(24 * time.Hour)
}

func TestALongScheduleDoesNotWrapItsDueTimesBeforeTheWindow(t *testing.T) {
	start, end := dayWindow()
	// 200_000 > the 106_751 threshold for a 24h window, and a plausible ask:
	// a day worked at roughly one child every 0.43s.
	p := Plan{
		Algo:   NameTWAP,
		Total:  big.NewRat(200000, 1),
		Start:  start,
		End:    end,
		Slices: 200000,
	}

	prev := start.Add(-time.Nanosecond)
	for i := range p.Slices {
		got := p.boundary(i)
		if got.Before(start) {
			t.Fatalf("slice %d is due %v, BEFORE the window opens at %v.\n\n"+
				"The nanosecond product overflowed int64 and went negative. Due() filters on "+
				"\"at or before now\", so this child — and every one after it — reads as due "+
				"immediately: the whole parent goes to the venue in one pass.", i, got, start)
		}
		if got.After(end) {
			t.Fatalf("slice %d is due %v, AFTER the window closes at %v", i, got, end)
		}
		if got.Before(prev) {
			t.Fatalf("slice %d is due %v, EARLIER than slice %d at %v — the schedule is not "+
				"monotonic, so children would be sent out of order", i, got, i-1, prev)
		}
		prev = got
	}
}

// THE ENDS ARE EXACT, which is the property boundary's own doc promises and the
// one a 128-bit intermediate could plausibly have broken.
func TestTheBoundaryEndsAreExactOnALongSchedule(t *testing.T) {
	start, end := dayWindow()
	p := Plan{Algo: NameTWAP, Total: big.NewRat(1, 1), Start: start, End: end, Slices: 200000}

	if got := p.boundary(0); !got.Equal(start) {
		t.Errorf("boundary(0) = %v, want the window start %v", got, start)
	}
	if got := p.boundary(p.Slices); !got.Equal(end) {
		t.Errorf("boundary(Slices) = %v, want the window end %v exactly — a schedule whose last "+
			"boundary falls short leaves a sliver of the window unworked", got, end)
	}
}

// THE 128-BIT PATH MUST AGREE WITH THE 64-BIT ONE WHEREVER THE LATTER WAS
// CORRECT. Every schedule this platform has ever derived is in this range, so a
// change of arithmetic that shifted any of them would be a silent re-timing of
// live orders rather than a bug fix.
func TestShortSchedulesAreUnchangedByTheWideningArithmetic(t *testing.T) {
	start := time.Date(2026, 8, 31, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		span   time.Duration
		slices int
	}{
		{"an hour in 12", time.Hour, 12},
		{"an hour in 360 (the 10s driver tick)", time.Hour, 360},
		{"five minutes in 7 (a span that does not divide)", 5 * time.Minute, 7},
		{"a day in 8640", 24 * time.Hour, 8640},
		{"one slice", time.Hour, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Plan{Algo: NameTWAP, Total: big.NewRat(1, 1), Start: start, End: start.Add(tc.span), Slices: tc.slices}
			for i := 0; i <= p.Slices; i++ {
				// The pre-#898 expression, evaluated where it could not overflow.
				want := start.Add(time.Duration(int64(tc.span) * int64(i) / int64(tc.slices))).UTC()
				if got := p.boundary(i); !got.Equal(want) {
					t.Fatalf("boundary(%d) = %v, want %v — the widened arithmetic moved a due "+
						"time that the 64-bit expression computed correctly", i, got, want)
				}
			}
		})
	}
}

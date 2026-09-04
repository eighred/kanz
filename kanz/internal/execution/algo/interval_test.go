package algo

import (
	"math/big"
	"testing"
	"time"
)

// THE INTERVAL A SLICE IS WORKED OVER IS THE SCHEDULE'S OWN (#1007).
//
// Plan.Interval was exported so the realised-participation measurement can name
// the same minutes the schedule used. If it ever drifts from the DUE times the
// algorithms produce, a participation denominator is computed over a window the
// child was not sent in — a plausible number, wrong, and impossible to spot from
// the outside.

var ivStart = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

func ivPlan(slices int, span time.Duration) Plan {
	return Plan{
		Algo:   NameTWAP,
		Total:  new(big.Rat).SetInt64(60),
		Start:  ivStart,
		End:    ivStart.Add(span),
		Slices: slices,
	}
}

// AN INTERVAL IS EXACTLY [due(i), due(i+1)), AND THE LAST ONE ENDS AT THE WINDOW.
//
// Asserted against the SCHEDULE the algorithm actually produced rather than
// against arithmetic restated here, which is the whole point of the method
// existing: a second copy of `start + i*(end-start)/slices` in the OMS would put
// the denominator's window and the child's due time one rounding apart.
func TestPlanInterval_AgreesWithTheSlicesItIsMeasuring(t *testing.T) {
	// A span that does not divide evenly by the slice count, so a drifting
	// implementation cannot coincide with a correct one.
	p := ivPlan(7, 100*time.Minute)
	slices, err := TWAP(p)
	if err != nil {
		t.Fatalf("TWAP: %v", err)
	}

	for i, s := range slices {
		from, to, ok := p.Interval(i)
		if !ok {
			t.Fatalf("slice %d has no interval", i)
		}
		if !from.Equal(s.Due) {
			t.Errorf("interval %d opens at %v, and its child is due at %v — the child would be "+
				"measured against a window it was not sent in", i, from, s.Due)
		}
		if i+1 < len(slices) && !to.Equal(slices[i+1].Due) {
			t.Errorf("interval %d closes at %v and interval %d opens at %v — the gap is volume "+
				"attributed to neither child, or to both", i, to, i+1, slices[i+1].Due)
		}
	}
	_, last, _ := p.Interval(p.Slices - 1)
	if !last.Equal(p.End.UTC()) {
		t.Errorf("the last interval closes at %v, want the window end %v — the tail of the "+
			"working window would be missing from every denominator", last, p.End.UTC())
	}
}

// AN INDEX OUTSIDE THE SCHEDULE HAS NO INTERVAL.
//
// Not a clamped one: a caller asking about a slice that does not exist has lost
// track of the schedule, and answering with the last interval would attribute one
// child's fills to another child's minutes.
func TestPlanInterval_RefusesAnIndexTheScheduleDoesNotHave(t *testing.T) {
	p := ivPlan(4, time.Hour)
	for _, i := range []int{-1, 4, 5} {
		if _, _, ok := p.Interval(i); ok {
			t.Errorf("Interval(%d) answered on a 4-slice plan", i)
		}
	}
	for _, bad := range []Plan{
		ivPlan(0, time.Hour),
		{Algo: NameTWAP, Total: new(big.Rat).SetInt64(60), Start: ivStart, End: ivStart, Slices: 4},
		{Algo: NameTWAP, Total: new(big.Rat).SetInt64(60), Start: ivStart, End: ivStart.Add(-time.Hour), Slices: 4},
	} {
		if _, _, ok := bad.Interval(0); ok {
			t.Errorf("Interval(0) answered on a plan with no window or no slices (%v..%v, %d) — a "+
				"zero-length interval divides a fill by whatever happened in no time at all",
				bad.Start, bad.End, bad.Slices)
		}
	}
}

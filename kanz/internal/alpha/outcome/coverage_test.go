package outcome

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/store"
)

// A MISS IS NOT ASSERTABLE OVER A WINDOW WITH A HOLE IN IT (#416).
//
// This is the defect this file exists for. Before the fix the resolver returned
// ok=true, Hit=false over a ten-minute horizon holding one of its ten minutes:
// a confident verdict about nine minutes nobody could speak for, counted against
// the model in a calibration report.
func TestAMissIsUnresolvedWhenTheHorizonHasAHole(t *testing.T) {
	m := storeWith(t,
		bar(0, 100, 100, 100),
		bar(1, 100, 100, 100),
		bar(2, 100, 100, 100),
		bar(3, 100, 100, 100),
		bar(4, 100, 100, 100),
		// minutes 5..12 absent — a quiet market or a dead feed, indistinguishable.
	)
	at := origin.Add(3 * time.Minute)
	out, reason, err := resolver(t, m).Resolve(
		context.Background(), claim(t, 0.05, 10*time.Minute), "BTC-USDT", at, far)
	if err != nil {
		t.Fatal(err)
	}
	if reason.OK() {
		t.Errorf("marked Hit=%v over a horizon missing 8 of its 10 minutes — a miss over a "+
			"window nobody observed is a claim about a venue outage, not about the model",
			out.Hit)
	}
	if reason != ReasonIncompleteWindow {
		t.Errorf("reason = %q, want %q — an operator reading this count must be able to tell a "+
			"market-data gap from a horizon that has not elapsed", reason, ReasonIncompleteWindow)
	}
}

// A TOUCH SURVIVES A HOLE. The other side of the asymmetry, and it must be
// pinned separately: making the resolver refuse every gapped window would be a
// one-line "fix" that passes the test above and throws away every hit the
// platform can actually prove.
func TestAnObservedTouchResolvesDespiteAHole(t *testing.T) {
	m := storeWith(t,
		bar(0, 100, 100, 100),
		bar(1, 100, 100, 100),
		bar(2, 100, 100, 100),
		bar(3, 100, 100, 100),
		bar(4, 100, 120, 100), // +20% ON THE TAPE
		// minutes 5..12 absent, exactly as above.
	)
	at := origin.Add(3 * time.Minute)
	out, reason, err := resolver(t, m).Resolve(
		context.Background(), claim(t, 0.05, 10*time.Minute), "BTC-USDT", at, far)
	if err != nil {
		t.Fatal(err)
	}
	if !reason.OK() {
		t.Fatalf("reason = %q — a high that is ON THE TAPE cannot be un-observed by a later "+
			"minute being missing", reason)
	}
	if !out.Hit {
		t.Error("Hit = false with a +20% print inside the horizon and a +5% threshold")
	}
}

// A WHOLE WINDOW STILL RESOLVES TO A MISS. Without this the change could be
// satisfied by never resolving a miss at all, which would make every calibration
// report show a base rate of 1.
func TestAWholeWindowStillResolvesAMiss(t *testing.T) {
	m := storeWith(t, flatBars(0, 14)...)
	at := origin.Add(3 * time.Minute)
	out, reason, err := resolver(t, m).Resolve(
		context.Background(), claim(t, 0.05, 10*time.Minute), "BTC-USDT", at, far)
	if err != nil {
		t.Fatal(err)
	}
	if !reason.OK() {
		t.Fatalf("reason = %q over a horizon with every minute present — the market was "+
			"observed and it did not move", reason)
	}
	if out.Hit {
		t.Error("Hit = true: nothing in the fixture reaches +5%")
	}
}

// A HORIZON SHORTER THAN THE SERIES NEVER RESOLVES, AND SAYS SO DISTINCTLY.
//
// store.Window.Whole() is vacuously true over zero buckets, so without its own
// arm this case falls straight through to a confident miss over nothing at all —
// the original defect arriving by the back door.
func TestAHorizonShorterThanOneBarIsNotAMiss(t *testing.T) {
	m := storeWith(t, flatBars(0, 6)...)
	// 30 seconds cannot contain a 1m bucket.
	s := claim(t, 0.05, 30*time.Second)
	_, reason, err := resolver(t, m).Resolve(
		context.Background(), s, "BTC-USDT", origin.Add(3*time.Minute), far)
	if err != nil {
		t.Fatal(err)
	}
	if reason.OK() {
		t.Error("a 30-second claim was graded against a 1-minute series")
	}
	if reason != ReasonHorizonShorterThanSeries {
		t.Errorf("reason = %q, want %q — this model is ungradeable against this series forever, "+
			"which is a different problem from a flaky feed", reason, ReasonHorizonShorterThanSeries)
	}
}

// THE ZERO Reason IS NOT Resolved: a caller that forgets to set one drops the
// score rather than silently grading it.
func TestTheZeroReasonIsNotResolved(t *testing.T) {
	var r Reason
	if r.OK() {
		t.Error("the zero Reason reports OK — an unset reason must fail closed")
	}
}

// flatBars is an unbroken run of minutes lo..hi that never moves enough to touch
// any threshold under test.
func flatBars(lo, hi int) []store.Bar {
	out := make([]store.Bar, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, bar(i, 100, 100.5, 99.5))
	}
	return out
}

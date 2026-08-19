package store

import (
	"testing"
	"time"
)

var gapOrigin = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func minuteBar(i int) Bar {
	return Bar{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m,
		BucketStart: gapOrigin.Add(time.Duration(i) * time.Minute),
	}
}

func minuteBars(idx ...int) []Bar {
	out := make([]Bar, 0, len(idx))
	for _, i := range idx {
		out = append(out, minuteBar(i))
	}
	return out
}

func TestWindowOfCountsWholeBucketsAndWhatIsPresent(t *testing.T) {
	from, to := gapOrigin, gapOrigin.Add(10*time.Minute)

	w, ok := WindowOf(minuteBars(0, 1, 2, 3, 4, 5, 6, 7, 8, 9), from, to, Resolution1m)
	if !ok {
		t.Fatal("WindowOf refused a 1m window")
	}
	if w.Buckets != 10 || w.Observed != 10 {
		t.Fatalf("Window = %+v, want 10/10", w)
	}
	if !w.Whole() || w.Missing() != 0 {
		t.Errorf("a full window reports Missing=%d Whole=%v", w.Missing(), w.Whole())
	}

	w, _ = WindowOf(minuteBars(0, 1, 9), from, to, Resolution1m)
	if w.Buckets != 10 || w.Observed != 3 {
		t.Fatalf("Window = %+v, want 10 buckets and 3 observed", w)
	}
	if w.Whole() || w.Missing() != 7 {
		t.Errorf("a gapped window reports Missing=%d Whole=%v, want 7/false", w.Missing(), w.Whole())
	}
}

// A PARTIAL INTERVAL AT THE EDGE IS NOT A BUCKET. A window of 90 seconds holds
// one whole minute, not two — counting the half would make every such window
// permanently incomplete and every miss over it permanently unresolvable.
func TestWindowOfCountsOnlyWholeBuckets(t *testing.T) {
	w, ok := WindowOf(minuteBars(1), gapOrigin.Add(30*time.Second),
		gapOrigin.Add(2*time.Minute), Resolution1m)
	if !ok {
		t.Fatal("refused")
	}
	if w.Buckets != 1 || w.Observed != 1 {
		t.Fatalf("Window = %+v, want 1/1 — only [12:01,12:02) lies wholly inside", w)
	}
}

// ZERO BUCKETS IS Whole(), AND THAT IS WHY CALLERS MUST CHECK Buckets. Pinned
// because the vacuity is load-bearing: outcome.Resolve relies on this being true
// and handles it with its own arm, so a later "fix" making it false would move
// the decision somewhere nobody is looking.
func TestAWindowTooSmallForABucketIsVacuouslyWhole(t *testing.T) {
	w, ok := WindowOf(nil, gapOrigin, gapOrigin.Add(30*time.Second), Resolution1m)
	if !ok {
		t.Fatal("refused")
	}
	if w.Buckets != 0 {
		t.Fatalf("Buckets = %d, want 0", w.Buckets)
	}
	if !w.Whole() {
		t.Error("Whole() is false over zero buckets — outcome.Resolve's ReasonHorizonShorter" +
			"ThanSeries arm exists because it is TRUE; changing that silently moves the decision")
	}
}

func TestWindowOfRefusesAnUnknownResolution(t *testing.T) {
	if _, ok := WindowOf(nil, gapOrigin, gapOrigin.Add(time.Hour), Resolution("7m")); ok {
		t.Error("WindowOf accepted a resolution this platform does not store")
	}
}

// BARS OF ANOTHER SERIES DO NOT COUNT AS COVERAGE. A 1h bar sitting in the slice
// must not make a 1m window look whole.
func TestWindowOfIgnoresAnotherResolution(t *testing.T) {
	coarse := Bar{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1h,
		BucketStart: gapOrigin}
	w, _ := WindowOf([]Bar{coarse}, gapOrigin, gapOrigin.Add(10*time.Minute), Resolution1m)
	if w.Observed != 0 {
		t.Errorf("Observed = %d, want 0 — an hourly bar does not cover a minute bucket", w.Observed)
	}
}

func TestContiguousSuffixTrimsToTheUnbrokenTail(t *testing.T) {
	// 0,1,2 then a hole, then 7,8,9 — the tail is the last three.
	got := ContiguousSuffix(minuteBars(0, 1, 2, 7, 8, 9))
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if !got[0].BucketStart.Equal(gapOrigin.Add(7 * time.Minute)) {
		t.Errorf("suffix starts at %s, want 12:07", got[0].BucketStart)
	}
}

func TestContiguousSuffixKeepsAnUnbrokenSeriesWhole(t *testing.T) {
	in := minuteBars(0, 1, 2, 3, 4)
	if got := ContiguousSuffix(in); len(got) != len(in) {
		t.Errorf("len = %d, want %d — an unbroken series must not be trimmed", len(got), len(in))
	}
}

func TestContiguousSuffixEdgeCases(t *testing.T) {
	if got := ContiguousSuffix(nil); got != nil {
		t.Errorf("nil input gave %v", got)
	}
	if got := ContiguousSuffix(minuteBars(3)); len(got) != 1 {
		t.Errorf("a single bar is a run of one, got %d", len(got))
	}
	// A resolution with no interval has no spacing to check, so vouching for the
	// slice would be vouching for something never examined.
	odd := []Bar{{InstrumentID: "X", Venue: "V", Resolution: Resolution("7m"), BucketStart: gapOrigin}}
	if got := ContiguousSuffix(odd); got != nil {
		t.Errorf("an unknown resolution gave %v, want nil", got)
	}
}

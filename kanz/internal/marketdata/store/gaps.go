package store

import "time"

// GAP ACCOUNTING FOR THE BAR SERIES: the one answer to "is there a hole here",
// and the one place the answer's LIMITS are written down (#416).
//
// # Why this is a package-level concept and not each reader's business
//
// The producer hands this question to its callers explicitly. bars/fold.go:
//
//	A minute with no trades produces NO BAR. [...] this process cannot tell
//	"nothing traded" from "the feed was down" or "we were rolling pods" — they
//	look identical from here. [...] A gap is honest and is the caller's to
//	interpret.
//
// Seven call sites read this store. Each one re-deriving "what is a gap" is how
// one concept becomes seven answers, and six of them being right is not the
// outcome that pattern has produced here before.
//
// # THE SIGNAL IS SOUND IN EXACTLY ONE DIRECTION, AND THAT IS THE WHOLE CONTRACT
//
// rollup/driver.go already ruled on the other direction, and it is right:
//
//	THE OBVIOUS TEST DOES NOT WORK. "1440 constituents" is not completeness: a
//	minute in which nothing traded produces NO BAR AT ALL, so a genuinely quiet
//	day is indistinguishable from a day whose ingestion died.
//
// So a WHOLE window does NOT prove the platform was observing. What a MISSING
// bucket proves is the useful half:
//
//	missing bucket  ⇒  we cannot assert what happened in it.        SOUND.
//	whole window    ⇒  the platform was up and observing.           NOT SOUND.
//
// Every use of this type must rest on the first line only. Reading Whole() as
// "the feed was healthy" reintroduces exactly the conflation the fold refuses to
// make, one layer up, where it is harder to see.
//
// # What would make the other direction sound, and it does not exist
//
// An INGESTION-COVERAGE RECORD stating which intervals were actually OBSERVED —
// "we were connected to XBIN for BTC-USDT from 12:00 to 13:00, and 12:37 was
// genuinely quiet". rollup/driver.go names it as the missing piece in the same
// breath, and it is missing still. Until it exists a hole is UNKNOWN rather than
// QUIET, and the counters built on this type are what make the cost of that
// visible instead of silently priced into a calibration report.
//
// Coverage is recorded going FORWARD only: nothing can reconstruct whether a feed
// was live last July. That is why the record is the next thing to land and not
// the last.

// Window reports how much of a bar window was actually present.
//
// IT IS NOT v1.InputCoverage, deliberately. That type is the same doctrine —
// "the difference between a MEASURED zero and a CONFIDENT one" — over POSITIONS,
// and it lives in the risk engine's API package. Market data importing the risk
// API to describe its own series would invert the dependency, and the two
// domains count different things: one names the instruments left out, this one
// counts the intervals nobody can speak for.
type Window struct {
	// Buckets is how many COMPLETE buckets the queried window contains. A window
	// that does not divide evenly contributes only its whole buckets — a partial
	// interval at either end is not a bucket the series was ever going to hold.
	Buckets int
	// Observed is how many of those buckets have a bar.
	Observed int
}

// Missing is how many buckets in the window hold no bar. Non-zero means the
// window cannot support a claim about ABSENCE — see the package note above.
func (w Window) Missing() int {
	if w.Observed >= w.Buckets {
		return 0
	}
	return w.Buckets - w.Observed
}

// Whole reports that every bucket in the window has a bar.
//
// READ IT AS "nothing is missing", NEVER AS "the feed was healthy". A window of
// zero buckets is Whole — vacuously, and callers that gate on this must decide
// what an empty window means for them rather than inheriting an answer from here.
func (w Window) Whole() bool { return w.Missing() == 0 }

// WindowOf counts the complete buckets in [from, to) and how many of them bars
// covers.
//
// ok=false when res has no interval. Bars outside the window, and bars of another
// resolution, are ignored rather than refused: the caller queried one series and
// this counts that series.
func WindowOf(bars []Bar, from, to time.Time, res Resolution) (Window, bool) {
	interval, ok := res.Interval()
	if !ok || interval <= 0 {
		return Window{}, false
	}
	from, to = from.UTC(), to.UTC()

	// The first bucket START at or after `from` — a bucket that began before the
	// window is not inside it. Aligned to the interval, because that is where the
	// series' buckets are.
	first := from.Truncate(interval)
	if first.Before(from) {
		first = first.Add(interval)
	}
	// A bucket [s, s+interval) is inside the window only when it ENDS at or before
	// `to`. If not even the first one does, the window holds no whole bucket.
	if first.Add(interval).After(to) {
		return Window{}, true
	}

	w := Window{Buckets: int(to.Sub(first) / interval)}
	seen := make(map[int64]bool, len(bars))
	for i := range bars {
		if bars[i].Resolution != res {
			continue
		}
		s := bars[i].BucketStart.UTC()
		if s.Before(first) || s.Add(interval).After(to) {
			continue
		}
		if !seen[s.UnixNano()] {
			seen[s.UnixNano()] = true
			w.Observed++
		}
	}
	return w, true
}

// ContiguousSuffix returns the longest run of bars ending at the LAST one whose
// buckets are spaced exactly one interval apart.
//
// # Why the SUFFIX and not the whole slice
//
// Every indicator reads the most RECENT n bars, so the question that decides
// whether a reading is what it claims is "are the last n contiguous", not "is the
// series perfect". A hole three hours back does not make the last twenty minutes
// any less of a twenty-minute window, and refusing the whole series for it would
// blank every reading on any instrument that has ever been quiet.
//
// # What it prevents
//
// Without it a period is an INDEX, not a duration: SMA(closes, 20) averages the
// last twenty ELEMENTS, and over a gapped series those twenty elements can span
// any amount of wall-clock time. The number stays in range and plausible while
// silently answering a different question — the failure this estate refuses
// everywhere else.
//
// bars must be ascending by BucketStart, as BarStore.Bars returns them. The
// interval comes from the LAST bar's own Resolution; a bar of another resolution
// ends the run rather than joining it, because the spacing that defines
// contiguity is only meaningful within one series.
//
// A bar whose resolution has no interval yields nil — there is no spacing to
// check, and returning the slice unexamined would vouch for something never
// looked at.
func ContiguousSuffix(bars []Bar) []Bar {
	if len(bars) == 0 {
		return nil
	}
	res := bars[len(bars)-1].Resolution
	interval, ok := res.Interval()
	if !ok || interval <= 0 {
		return nil
	}
	start := len(bars) - 1
	for i := len(bars) - 1; i > 0; i-- {
		if bars[i-1].Resolution != res {
			break
		}
		if !bars[i-1].BucketStart.UTC().Add(interval).Equal(bars[i].BucketStart.UTC()) {
			break
		}
		start = i - 1
	}
	return bars[start:]
}

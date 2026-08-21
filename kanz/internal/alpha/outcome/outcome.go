// Package outcome resolves whether an alpha score's claim came true (#416 C2).
//
// It is the join that closes the calibration loop. internal/alpha/score defines
// the claim — P(return >= threshold within horizon) — and records it on the
// StrategySignal FACT. This answers the other half: given the OHLCV series, did
// the return reach the threshold inside the horizon? Without it, every score on
// the audit root is a claim nobody ever marked.
//
// # Three decisions, each of which silently changes what calibration measures
//
// THE REFERENCE PRICE IS THE LAST COMPLETED BAR'S CLOSE, not the close of the
// bar containing the signal. That bar had not finished when the signal fired, so
// its close was not knowable — using it is lookahead, and it is the flattering
// kind: a strategy that fires mid-bar on a move gets measured from a price that
// already includes the move it reacted to, which makes its claims look better
// than they were.
//
// "WITHIN THE HORIZON" IS A TOUCH, resolved against the bar HIGHS (or lows, for a
// negative threshold), not the terminal close. "Return >= X within H" says the
// return reached X at some point in H; the terminal reading answers a different
// question ("was it still there at the end") and would mark a claim wrong that
// was right and then reverted. The two differ by a lot on a mean-reverting
// instrument, so the choice cannot be left implicit — and this is the one that
// matches the words.
//
// A CLAIM THAT CANNOT YET BE MARKED IS UNRESOLVED, NOT A MISS. The horizon may
// not have elapsed; the series may have a gap. Counting either as a miss biases
// calibration downward and does it worst for the newest scores, which are exactly
// the ones a live model is being judged on. Resolve returns ok=false, and
// Calibrate never sees the score at all.
//
// # A HIT IS PROVABLE FROM A PARTIAL WINDOW. A MISS IS NOT.
//
// This asymmetry is the reason Reason exists, and it was worth 30 lines because
// the code originally had it wrong in the expensive direction: a horizon with
// nine of its ten minutes missing returned ok=true, Hit=false — a confident
// verdict over a window nobody could speak for.
//
// The logic is one-sided, and only one side is sound:
//
//	a touch OBSERVED          ⇒ the claim came true. Holes elsewhere cannot
//	                            un-observe it.                         SOUND.
//	no touch, window WHOLE    ⇒ every minute was looked at; it did not
//	                            happen.                                SOUND.
//	no touch, buckets MISSING ⇒ it may have touched in a minute we do not
//	                            hold. UNRESOLVED.                      NOT A MISS.
//
// # What that costs, and why paying it is the point
//
// The strict arm fires on any instrument quiet enough to skip a minute, so an
// illiquid series will be largely unresolvable and will say so. That is the
// honest reading: store.Window's own doc records that a missing bucket cannot be
// told from a quiet one without an ingestion-coverage record this platform does
// not have, and the unresolved counts are what make the absence of that record
// cost something visible instead of being priced silently into a calibration
// report nobody can audit.
//
// The bias it removes ran the wrong way for judging a model: unresolved-as-miss
// understates the realized hit rate, so a sound model reads as OVERCONFIDENT and
// gets sized down under "constraints gate, scores size" — and two models become
// incomparable, because the grade partly measures which one ran during the
// feed's bad days rather than which one was right.
//
// # It is about the INSTRUMENT, not the position
//
// The claim is a statement about the market: "this instrument returns at least X".
// A short signal's profit is the negative of that, and translating one into the
// other is the sizing layer's business (#416 E, "constraints gate, scores size").
// Doing it here would mean a threshold's sign meant something different depending
// on an action this package cannot see, and two engines would disagree about what
// their own scores claimed.
package outcome

import (
	"context"
	"fmt"
	decutil "github.com/eighred/kanz/internal/dec"
	"time"

	"github.com/eighred/kanz/internal/alpha/score"
	"github.com/eighred/kanz/internal/marketdata/store"
)

// Resolver marks scores against the durable bar series.
type Resolver struct {
	bars store.BarStore
	cfg  Config
}

// Config binds the resolver to one venue's series.
type Config struct {
	// Venue is the MIC whose candles decide the outcome. REQUIRED: a bar's venue
	// is part of its identity, and marking a claim against a composite series
	// would mark it against a price nobody could trade.
	Venue string
	// Resolution defaults to 1m, the base series.
	Resolution store.Resolution
}

// NewResolver wraps a bar store.
func NewResolver(bars store.BarStore, cfg Config) (*Resolver, error) {
	if bars == nil {
		return nil, fmt.Errorf("outcome: nil bar store")
	}
	if cfg.Venue == "" {
		return nil, fmt.Errorf("outcome: venue is required — marking a claim against a " +
			"composite series marks it against a price nobody could trade")
	}
	if cfg.Resolution == "" {
		cfg.Resolution = store.Resolution1m
	}
	if !cfg.Resolution.Valid() {
		return nil, fmt.Errorf("outcome: %q is not a stored resolution", cfg.Resolution)
	}
	return &Resolver{bars: bars, cfg: cfg}, nil
}

// Reason says why a score was or was not marked.
//
// STRING-VALUED, matching liquiditysource's refusal reasons, because these are
// metric label values: "the model is being graded on a tenth of its scores" and
// "the horizon has not elapsed yet" are both non-zero unresolved counts and need
// entirely different people. #566 established that distinction one tree over,
// where a limit refusing because the engine went quiet and one refusing because
// reference data was missing were indistinguishable to an operator.
//
// THE ZERO VALUE IS NOT Resolved, on purpose: a Reason nobody set reads as
// unresolved, so a caller that forgets to assign one drops the score rather than
// silently grading it.
type Reason string

const (
	// Resolved means the outcome is marked and may be calibrated against.
	Resolved Reason = "resolved"
	// ReasonHorizonOpen: the claim's window had not finished within asOf.
	ReasonHorizonOpen Reason = "horizon_open"
	// ReasonNoReferencePrice: no bar had completed at or before the assertion, so
	// there is no price the return could be measured FROM.
	ReasonNoReferencePrice Reason = "no_reference_price"
	// ReasonIncompleteWindow: no touch was observed, and the horizon is missing
	// at least one bucket — so the absence cannot be asserted. See the package
	// note; this is the arm that used to return a confident miss.
	ReasonIncompleteWindow Reason = "incomplete_window"
	// ReasonHorizonShorterThanSeries: the claim's horizon does not contain one
	// whole bar, so no bucket could ever fall inside it.
	//
	// SEPARATE FROM ReasonIncompleteWindow because it is a CONFIGURATION defect,
	// not a data one, and it never heals: a model claiming a 30-second horizon
	// graded against a 1m series resolves nothing today and nothing next year,
	// while an incomplete window resolves as soon as the feed does. Folding the
	// two would hide a permanently ungradeable model inside a count that
	// operators are trained to read as "the venue was flaky".
	ReasonHorizonShorterThanSeries Reason = "horizon_shorter_than_series"
)

// OK reports that the outcome may be counted.
func (r Reason) OK() bool { return r == Resolved }

// Resolve marks one score for instrumentID, asserted at `at`, using knowledge
// available at `asOf`.
//
// A Reason other than Resolved means UNRESOLVED. It is NOT a miss, and the caller
// must drop it rather than count it — see the package note on why a partial
// window can prove a hit and cannot prove a miss.
//
// asOf is separate from `at`+horizon so a calibration run is REPRODUCIBLE: the
// bar store is bitemporal, and a run pinned to a knowledge horizon reads the same
// bars next month even if a venue restates a candle in between. Zero ⇒ now.
func (r *Resolver) Resolve(ctx context.Context, s score.Score, instrumentID string, at, asOf time.Time) (score.Outcome, Reason, error) {
	if err := s.Validate(); err != nil {
		return score.Outcome{}, ReasonHorizonOpen, err
	}
	interval, ok := r.cfg.Resolution.Interval()
	if !ok {
		return score.Outcome{}, ReasonHorizonOpen,
			fmt.Errorf("outcome: resolution %q has no interval", r.cfg.Resolution)
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	deadline := at.Add(s.Horizon())

	// THE HORIZON MUST HAVE FINISHED WITHIN WHAT WE KNOW. A claim whose window is
	// still open cannot be marked, and marking it early is how a live model's
	// most recent scores all become misses.
	if deadline.After(asOf) {
		return score.Outcome{}, ReasonHorizonOpen, nil
	}

	// One read covering both the reference bar and the whole horizon. The
	// reference is the last bar to COMPLETE at or before `at`, so the window
	// starts one interval earlier than it otherwise would.
	bars, err := r.bars.Bars(ctx, store.BarQuery{
		InstrumentID: instrumentID,
		Venue:        r.cfg.Venue,
		Resolution:   r.cfg.Resolution,
		From:         at.Add(-2 * interval),
		To:           deadline,
		AsOf:         asOf,
	})
	if err != nil {
		return score.Outcome{}, ReasonHorizonOpen, err
	}

	ref, refOK := referencePrice(bars, at, interval)
	if !refOK {
		return score.Outcome{}, ReasonNoReferencePrice, nil
	}

	window := barsIn(bars, at, deadline, interval)

	// A TOUCH IS PROVABLE FROM WHATEVER WE HOLD. Check it BEFORE coverage: a hole
	// elsewhere in the horizon cannot un-observe a high that is on the tape, and
	// refusing a demonstrated hit for a missing minute would throw away the one
	// verdict that never needed the full window.
	if touched(window, ref, s.Threshold()) {
		return score.Outcome{Score: s, Hit: true}, Resolved, nil
	}

	// NO TOUCH IS ONLY A MISS OVER A WHOLE WINDOW. store.WindowOf counts the
	// buckets the horizon should hold against the ones that are there; anything
	// short and the claim may have come true in a minute this platform cannot
	// speak for. The empty window is not special-cased — it is simply the case
	// where every bucket is missing, and it reaches the same refusal.
	cov, covOK := store.WindowOf(window, at, deadline, r.cfg.Resolution)
	if !covOK {
		return score.Outcome{}, ReasonIncompleteWindow, nil
	}
	// WHOLE IS VACUOUS OVER ZERO BUCKETS, and store.Window's own doc says so. A
	// horizon too short to contain one bar would otherwise fall straight through
	// to a confident miss over nothing observed at all — the same defect this
	// change exists to remove, arriving by the back door.
	if cov.Buckets == 0 {
		return score.Outcome{}, ReasonHorizonShorterThanSeries, nil
	}
	if !cov.Whole() {
		return score.Outcome{}, ReasonIncompleteWindow, nil
	}
	return score.Outcome{Score: s, Hit: false}, Resolved, nil
}

// referencePrice is the close of the last bar to COMPLETE at or before `at`.
//
// A bar with BucketStart B on an interval I completes at B+I. The bar CONTAINING
// `at` has not completed, so its close was not knowable at `at` — see the package
// doc on lookahead.
func referencePrice(bars []store.Bar, at time.Time, interval time.Duration) (float64, bool) {
	var best store.Bar
	found := false
	for _, b := range bars {
		if !b.BucketStart.Add(interval).After(at) { // completed at or before `at`
			if !found || b.BucketStart.After(best.BucketStart) {
				best, found = b, true
			}
		}
	}
	if !found {
		return 0, false
	}
	return decutil.Float64(best.Close)
}

// barsIn returns the bars whose interval lies inside (at, deadline] — the
// horizon. A bar that merely CONTAINS `at` is excluded: part of its range
// happened before the claim was made, and crediting the claim with it is the same
// lookahead the reference price avoids.
func barsIn(bars []store.Bar, at, deadline time.Time, interval time.Duration) []store.Bar {
	var out []store.Bar
	for _, b := range bars {
		start := b.BucketStart
		end := start.Add(interval)
		if !start.Before(at) && !end.After(deadline) {
			out = append(out, b)
		}
	}
	return out
}

// touched reports whether the return reached the threshold at any point in the
// window.
//
// A POSITIVE threshold is tested against the HIGHS and a NEGATIVE one against the
// LOWS. Both are "did the return reach X", read in the direction X points — using
// highs for both would make a negative threshold true the instant the price rose,
// which inverts the claim a stop-loss model makes.
func touched(window []store.Bar, ref, threshold float64) bool {
	if ref <= 0 {
		return false
	}
	target := ref * (1 + threshold)
	for _, b := range window {
		if threshold >= 0 {
			if h, ok := decutil.Float64(b.High); ok && h >= target {
				return true
			}
			continue
		}
		if l, ok := decutil.Float64(b.Low); ok && l <= target {
			return true
		}
	}
	return false
}

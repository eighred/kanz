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
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

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

// Resolve marks one score for instrumentID, asserted at `at`, using knowledge
// available at `asOf`.
//
// ok=false means UNRESOLVED — no reference price, or the horizon has not finished
// inside the knowledge available. It is not a miss, and the caller must drop it
// rather than count it.
//
// asOf is separate from `at`+horizon so a calibration run is REPRODUCIBLE: the
// bar store is bitemporal, and a run pinned to a knowledge horizon reads the same
// bars next month even if a venue restates a candle in between. Zero ⇒ now.
func (r *Resolver) Resolve(ctx context.Context, s score.Score, instrumentID string, at, asOf time.Time) (score.Outcome, bool, error) {
	if err := s.Validate(); err != nil {
		return score.Outcome{}, false, err
	}
	interval, ok := r.cfg.Resolution.Interval()
	if !ok {
		return score.Outcome{}, false, fmt.Errorf("outcome: resolution %q has no interval", r.cfg.Resolution)
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	deadline := at.Add(s.Horizon())

	// THE HORIZON MUST HAVE FINISHED WITHIN WHAT WE KNOW. A claim whose window is
	// still open cannot be marked, and marking it early is how a live model's
	// most recent scores all become misses.
	if deadline.After(asOf) {
		return score.Outcome{}, false, nil
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
		return score.Outcome{}, false, err
	}

	ref, refOK := referencePrice(bars, at, interval)
	if !refOK {
		return score.Outcome{}, false, nil
	}

	window := barsIn(bars, at, deadline, interval)
	if len(window) == 0 {
		// The horizon elapsed and the series has nothing in it. That is a data
		// gap, not a market that did not move — and treating it as a miss would
		// attribute a venue outage to the model.
		return score.Outcome{}, false, nil
	}
	return score.Outcome{Score: s, Hit: touched(window, ref, s.Threshold())}, true, nil
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
	return decimalFloat(best.Close)
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
			if h, ok := decimalFloat(b.High); ok && h >= target {
				return true
			}
			continue
		}
		if l, ok := decimalFloat(b.Low); ok && l <= target {
			return true
		}
	}
	return false
}

// decimalFloat converts an exact Decimal for analytic use, dividing by the power
// of ten rather than multiplying by its inexact reciprocal.
func decimalFloat(d *commonpb.Decimal) (float64, bool) {
	if d == nil {
		return 0, false
	}
	exp := int(d.GetExponent())
	var f float64
	if exp >= 0 {
		f = float64(d.GetCoefficient()) * math.Pow10(exp)
	} else {
		f = float64(d.GetCoefficient()) / math.Pow10(-exp)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

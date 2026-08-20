// Package barview implements alpha.BarView: the point-in-time read of the
// durable OHLCV series an alpha engine decides from (#416, owner ruling
// 2026-08-20).
//
// pkg/alpha carries the seam and the reasoning behind it. This is the half that
// has to be right, because every property that seam claims is enforced here and
// nowhere else.
//
// # THE THREE WAYS A POINT-IN-TIME READ LEAKS, AND WHAT CLOSES EACH
//
// None of them fails loudly. A leaked read returns numbers in range, from real
// bars, over a plausible window — and the backtest built on it reports a strategy
// that could not have existed, in the flattering direction, which is the one
// nobody audits.
//
//  1. THE BAR CONTAINING THE DECISION TIME. It had not completed, so its close
//     was not knowable. `To` is the START of that bucket, so the bucket is
//     excluded — BarQuery.To is exclusive on BucketStart, and a bar starting at
//     `to` is the in-flight one.
//
//     This is the leak that flatters most and shows least: an engine firing
//     mid-bar on a move would be handed a close that already contains the move it
//     reacted to. internal/alpha/outcome refuses it on the grading side for the
//     same reason, and the two MUST agree — a model graded from a price the
//     engine never saw is being graded on somebody else's decision.
//
//  2. A BAR STARTING AFTER IT. Same bound; it never enters the window.
//
//  3. A CORRECTION THAT ARRIVED LATER. The store is bitemporal and a venue
//     restating a candle writes a NEW knowledge_time rather than overwriting, so
//     the observation window alone does not bound this: BarQuery.AsOf does, and
//     it is set to the SAME instant. Bounding one without the other still leaks,
//     and leaks invisibly — the bars are from the right minutes and simply say
//     what the venue decided later that they should have said.
//
// # THE DECISION TIME IS REQUIRED, WITH NO DEFAULT TO NOW
//
// A view that defaulted to time.Now() would be correct for a live engine and
// catastrophic for a backtest — every read would answer from today's knowledge
// over history, which is total look-ahead, and it would do so while looking like
// the obvious convenience. So New refuses a zero DecisionTime. "Nothing
// configured" and "checked, and fine" must never look the same.
//
// # CONCURRENCY: THERE IS NONE, ON PURPOSE
//
// A View is immutable after New and holds no goroutine, no channel and no
// mutable field. Its decision time cannot be advanced in place, so a live engine
// builds one per tick. That is deliberate rather than incidental: Evaluate runs
// on market-ingest's tick loop beside the fold goroutines mutating the books, and
// a "just make DecisionTime a var so it can be advanced" convenience would put
// shared mutable state on exactly that path — where -race does not run on the
// usual box and CI is the only detector.
package barview

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/indicator"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/pkg/alpha"
)

// Config binds a view to one series and one decision time.
type Config struct {
	// InstrumentID is the canonical Kanz instrument. Required.
	InstrumentID string
	// Venue is the MIC whose candles to read. REQUIRED, and there is no "any"
	// value: reading across venues would build a composite candle this platform
	// deliberately does not have, and an engine executing against one book would
	// have decided from a price nobody could trade.
	Venue string
	// Resolution defaults to 1m, the base series. The coarser two are rollups.
	Resolution store.Resolution
	// Lookback is how many buckets back from the decision time to read. Zero uses
	// indicator.DefaultLookback, which is sized so the recursive averages have
	// decayed their seed rather than merely having enough elements.
	Lookback int
	// DecisionTime is the horizon this view refuses to read past. REQUIRED — see
	// the package note on why there is no default.
	DecisionTime time.Time
}

// View reads one instrument's candles on one venue, as of one decision time.
type View struct {
	bars     store.BarStore
	coverage store.CoverageStore
	cfg      Config
	interval time.Duration
}

var _ alpha.BarView = (*View)(nil)

// New builds a view over the bar series and the ingestion-coverage record.
//
// BOTH STORES ARE REQUIRED. A view wired without coverage would still return
// readings, and every window would come back Sound — because Unknown would always
// be zero for want of anything to compare against. That is the shape this estate
// refuses everywhere: "nothing configured" answering exactly like "checked, and
// fine", on the one number an engine consults to know whether it may make a claim
// about the whole window.
func New(bars store.BarStore, coverage store.CoverageStore, cfg Config) (*View, error) {
	if bars == nil {
		return nil, errors.New("barview: nil bar store")
	}
	if coverage == nil {
		return nil, errors.New("barview: nil coverage store — a view without the " +
			"ingestion-coverage record reports every window as Sound, because there is nothing " +
			"for an absence to fail against")
	}
	if cfg.InstrumentID == "" {
		return nil, errors.New("barview: instrument_id is required")
	}
	if cfg.Venue == "" {
		return nil, errors.New("barview: venue is required — a bar's venue is part of its " +
			"identity, and reading across venues builds a composite candle nobody could trade")
	}
	if cfg.Resolution == "" {
		cfg.Resolution = store.Resolution1m
	}
	interval, ok := cfg.Resolution.Interval()
	if !ok || interval <= 0 {
		return nil, fmt.Errorf("barview: %q is not a stored resolution", cfg.Resolution)
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = indicator.DefaultLookback
	}
	if cfg.DecisionTime.IsZero() {
		return nil, errors.New("barview: decision_time is required and does not default to now — " +
			"a view that defaulted would answer a backtest from today's knowledge over history, " +
			"which is total look-ahead wearing the shape of a convenience")
	}
	cfg.DecisionTime = cfg.DecisionTime.UTC()
	return &View{bars: bars, coverage: coverage, cfg: cfg, interval: interval}, nil
}

// InstrumentID implements alpha.BarView.
func (v *View) InstrumentID() string { return v.cfg.InstrumentID }

// MIC implements alpha.BarView.
func (v *View) MIC() string { return v.cfg.Venue }

// DecisionTime implements alpha.BarView.
func (v *View) DecisionTime() time.Time { return v.cfg.DecisionTime }

// At returns the reading for market time `at`.
//
// A REQUEST PAST THE DECISION TIME IS REFUSED, NOT TRUNCATED. Truncating would
// answer with the window the caller did not ask for and give it no way to know —
// the reading would be well-formed, in range, and about a different moment.
func (v *View) At(ctx context.Context, at time.Time) (alpha.Reading, error) {
	if at.IsZero() {
		return alpha.Reading{}, errors.New("barview: zero read time")
	}
	at = at.UTC()
	if at.After(v.cfg.DecisionTime) {
		return alpha.Reading{}, fmt.Errorf("%w: read at %s is past the view's decision time %s",
			alpha.ErrLookahead, at.Format(time.RFC3339Nano), v.cfg.DecisionTime.Format(time.RFC3339Nano))
	}

	// THE WINDOW ENDS AT THE LAST COMPLETED BUCKET. `to` is the start of the
	// bucket containing `at`, and BarQuery.To is exclusive on BucketStart, so the
	// in-flight bucket is excluded. When `at` falls exactly on a boundary the
	// bucket starting there has not completed either, and the same bound excludes
	// it — the two cases are one expression rather than a special case somebody
	// later removes.
	to := at.Truncate(v.interval)
	from := to.Add(-time.Duration(v.cfg.Lookback) * v.interval)

	bars, err := v.bars.Bars(ctx, store.BarQuery{
		InstrumentID: v.cfg.InstrumentID,
		Venue:        v.cfg.Venue,
		Resolution:   v.cfg.Resolution,
		From:         from,
		To:           to,
		// BOTH AXES AT `at`. Bounding the observation window without the knowledge
		// horizon reads corrections that arrived afterwards: the bars are from the
		// right minutes and say what the venue decided later they should have said.
		AsOf: at,
	})
	if err != nil {
		return alpha.Reading{}, fmt.Errorf("barview: bars: %w", err)
	}

	cov, err := v.coverage.Coverage(ctx, store.CoverageQuery{
		InstrumentID: v.cfg.InstrumentID,
		Venue:        v.cfg.Venue,
		Resolution:   v.cfg.Resolution,
		From:         from,
		To:           to,
	})
	if err != nil {
		return alpha.Reading{}, fmt.Errorf("barview: coverage: %w", err)
	}

	// THE OBSERVABLE THING FIRST, COVERAGE SECOND — the #594 asymmetry, and the
	// order is load-bearing rather than stylistic. Readings over the unbroken run
	// are provable from what IS there; gating them on a whole window would throw
	// away every signal on any instrument quiet enough to skip a minute, which is
	// most of them. store.ContiguousSuffix trims to that run and indicator.
	// Features applies the identical trim internally, so the count below is the
	// count the readings were computed over rather than a second opinion about it.
	run := store.ContiguousSuffix(bars)
	reading := alpha.Reading{
		At:         at,
		Indicators: indicator.Features(bars),
		Contiguous: len(run),
	}
	if len(run) > 0 {
		reading.Close, reading.CloseOK = decimalFloat(run[len(run)-1].Close)
	}

	// Coverage is REPORTED, never a gate. AttestedWindowOf is the one place the
	// gap arithmetic lives; this carries its counts across the seam.
	att, ok := store.AttestedWindowOf(bars, cov, from, to, v.cfg.Resolution)
	if !ok {
		return alpha.Reading{}, fmt.Errorf("barview: resolution %q has no interval", v.cfg.Resolution)
	}
	reading.Coverage = alpha.Coverage{
		Buckets:  att.Buckets,
		Observed: att.Observed,
		Quiet:    att.Quiet,
		Unknown:  att.Unknown,
	}
	return reading, nil
}

// decimalFloat converts an exact Decimal for analytic use, dividing by the power
// of ten rather than multiplying by its inexact reciprocal — the same conversion
// internal/alpha/outcome performs, for the same reason (#514's one-ULP drift).
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

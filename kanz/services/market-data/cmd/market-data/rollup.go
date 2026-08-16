package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/marketdata/rollup"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/schedule"
	"github.com/eighred/kanz/services/market-data/internal/config"
)

// THE 1h AND 1d SERIES GET A PRODUCER (#509).
//
// # What was wrong
//
// store.Resolution declares 1m, 1h and 1d; the table, its index and every query
// support all three; and ingest.go states the design outright — "ONLY 1m IS
// INGESTED. The coarser series are ROLLUPS derived from this one". Nothing
// derived them. Resolution1h had ZERO producers.
//
// So the coarse series were empty, and every consumer silently read 1-minute
// bars instead. That is not merely slow: a 28-day ADV window is roughly 40,000
// rows per instrument per call, and internal/risk/compute walks the book twice
// per risk request, so a 500-name portfolio is tens of millions of rows for one
// number. It is the reason the liquidity measures could not be wired.
//
// # Why it runs HERE
//
// market-data owns the bar store — the same argument the VWAP watcher beside it
// makes. The rollup is a read-modify-write on a table this service already holds
// a pool for, so hosting it anywhere else means a second pool, a second
// deployment artifact and a second thing to schedule. The estate has no CronJob
// anywhere, so a CLI would have joined kanz-backfill in the set of tools nothing
// runs.
//
// # The two clocks, which are not the same clock
//
// WATERMARK is observation time: has the market finished happening. AsOf is
// knowledge time: what had we learned. The rollup takes both because conflating
// them is the mistake that produces a wrong bar — a bucket folded before its last
// minutes landed looks finished and is short, and the store is append-only, so
// the error is then immutable at that knowledge time and can only be corrected by
// writing a restatement beside it.
//
// The watermark is now minus a configured lag. AsOf is left zero — "everything
// known now" — which is what a routine run wants: it should incorporate a
// restatement of a 1-minute bar as soon as it knows about one.
//
// # Why it looks BACK rather than only at what is newly due
//
// Each run re-derives a window of recent buckets rather than the single bucket
// that just became eligible. Run is idempotent — an unchanged bucket writes
// nothing — so the cost of the overlap is a read, and the benefit is that a
// missed run, a restarted pod or a restated minute all heal on the next tick
// without anyone noticing they happened. A job that only ever looked at the
// newest bucket would leave a permanent hole after one bad hour.

// Rollup window and cadence.
const (
	// rollupLookback1h re-derives two days of hourly buckets each run.
	rollupLookback1h = 48 * time.Hour
	// rollupLookback1d re-derives a week of daily buckets. Longer than the hourly
	// window because a daily bar depends on 1,440 minutes and therefore has 1,440
	// chances to be restated — and because a week of daily buckets is 7 folds,
	// which costs nothing.
	rollupLookback1d = 7 * 24 * time.Hour
)

// rollupJobs builds the scheduler jobs that derive the coarse series, or returns
// nil when no series are configured.
//
// A nil return is REPORTED by the caller, not silent: with no rollup the coarse
// series stay empty and every consumer keeps reading 1-minute bars, which is the
// state this exists to end.
func rollupJobs(cfg config.Config, st store.BarStore, reg prometheus.Registerer, logger *slog.Logger) ([]schedule.Job, error) {
	series, err := parseRollupSeries(cfg.RollupSeries)
	if err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return nil, nil
	}
	roller, err := rollup.New(st)
	if err != nil {
		return nil, err
	}

	written := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_market_data_rollup_bars_written_total",
		Help: "Coarse OHLCV bars written by the rollup, by target resolution. Zero for a " +
			"resolution over a period the base series covered means the coarse series is not " +
			"being produced and every consumer is reading 1-minute bars (#509).",
	}, []string{"resolution"})
	restated := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_market_data_rollup_bars_restated_total",
		Help: "Coarse bars the rollup DISAGREED with and rewrote — a constituent minute was " +
			"restated after the bucket was first derived. The previous derived bar is still in " +
			"the store to compare against. This is the number worth alerting on: a healthy " +
			"series restates rarely, and a rising rate means the base series is being corrected " +
			"underneath consumers who already read it.",
	}, []string{"resolution"})
	incomplete := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_market_data_rollup_buckets_incomplete_total",
		Help: "Buckets the watermark said were unfinished, skipped rather than folded short. " +
			"Steady non-zero is normal — the newest bucket is always unfinished. A LARGE value " +
			"means the watermark lag is bigger than it needs to be and the coarse series is " +
			"lagging the base one.",
	}, []string{"resolution"})
	empty := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_market_data_rollup_buckets_empty_total",
		Help: "Finished buckets that held no 1-minute bar at all, so no coarse bar could be " +
			"built. On a liquid instrument this is a GAP IN THE BASE SERIES, not a quiet " +
			"market — the coarse series ends up with a hole exactly where the 1m one has one.",
	}, []string{"resolution"})
	reg.MustRegister(written, restated, incomplete, empty)

	targets := []struct {
		res      store.Resolution
		lookback time.Duration
	}{
		{store.Resolution1h, rollupLookback1h},
		{store.Resolution1d, rollupLookback1d},
	}

	var jobs []schedule.Job
	for _, tgt := range targets {
		for _, s := range series {
			jobs = append(jobs, schedule.Job{
				Name:     "rollup/" + string(tgt.res) + "/" + s.InstrumentID + "@" + s.Venue,
				Interval: cfg.RollupInterval,
				Refresh: rollupRefresh(roller, s, tgt.res, tgt.lookback, cfg.RollupWatermarkLag,
					written, restated, incomplete, empty, logger),
			})
		}
	}
	return jobs, nil
}

// rollupRefresh is one series-and-resolution's periodic fold.
func rollupRefresh(
	roller *rollup.Roller, s rollup.Series, target store.Resolution,
	lookback, watermarkLag time.Duration,
	written, restated, incomplete, empty *prometheus.CounterVec,
	logger *slog.Logger,
) schedule.RefreshFunc {
	return func(ctx context.Context, asOf time.Time) error {
		interval, ok := target.Interval()
		if !ok {
			return fmt.Errorf("rollup: %q has no interval", target)
		}
		// THE WINDOW IS SNAPPED TO BUCKET BOUNDARIES because Request refuses one
		// that cuts a bucket in half — deliberately, so a caller who asked about
		// 12:30 is never handed a bar covering 12:00.
		watermark := asOf.Add(-watermarkLag).Truncate(interval)
		from := watermark.Add(-lookback).Truncate(interval)

		res, err := roller.Run(ctx, rollup.Request{
			Series:    s,
			Target:    target,
			From:      from,
			To:        watermark,
			Watermark: watermark,
			// ZERO ON PURPOSE: everything known now. A routine run should pick up
			// a restated minute the moment it learns of one — pinning a knowledge
			// horizon here is for a reproducible backfill, not for the job that
			// keeps the series current.
		})
		if err != nil {
			return err
		}

		label := string(target)
		written.WithLabelValues(label).Add(float64(res.Written))
		restated.WithLabelValues(label).Add(float64(res.Restated))
		incomplete.WithLabelValues(label).Add(float64(res.Incomplete))
		empty.WithLabelValues(label).Add(float64(res.Empty))

		// A RESTATEMENT IS LOGGED, not just counted. It means a bar some consumer
		// has already read was wrong, and the log is where an operator finds which
		// series and when.
		if res.Restated > 0 {
			logger.Warn("rollup rewrote coarse bars a restated minute had invalidated",
				"instrument", s.InstrumentID, "venue", s.Venue, "resolution", label,
				"restated", res.Restated, "written", res.Written,
				"note", "the previous derived bar is still in the store at its own knowledge time")
		}
		return nil
	}
}

// parseRollupSeries parses "BTC-USDT@XBIN,ETH-USDT@XBIN".
//
// It REFUSES a malformed entry rather than skipping it. A skipped series is a
// series nobody rolls up, and it would look identical to one nobody configured —
// so a typo would quietly leave one instrument reading 1-minute bars forever.
func parseRollupSeries(spec string) ([]rollup.Series, error) {
	var out []rollup.Series
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		instrument, venue, ok := strings.Cut(part, "@")
		instrument, venue = strings.TrimSpace(instrument), strings.TrimSpace(venue)
		if !ok || instrument == "" || venue == "" {
			return nil, fmt.Errorf("MARKET_DATA_ROLLUP_SERIES: %q is not instrument@venue — a bar's "+
				"venue is part of its identity and this platform has no composite series, so a "+
				"series with no venue names nothing", part)
		}
		out = append(out, rollup.Series{InstrumentID: instrument, Venue: venue})
	}
	return out, nil
}

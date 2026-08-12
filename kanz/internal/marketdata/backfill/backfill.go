// Package backfill loads historical OHLCV from a venue into the bar store (#425
// A3).
//
// # Why it exists
//
// The bar producer folds live trades into candles from the moment it starts, so
// the series begins the day it is deployed. Training a model or evaluating a
// strategy needs years, and the venues already have them: Binance and OKX both
// serve their own kline history.
//
// # Taken from the VENUE we trade, never an aggregator
//
// A bar from an aggregator is not a price we can trade at. A model trained on a
// composite candle and executed against one exchange's book shows a
// backtest/live divergence that reads as "the strategy stopped working" rather
// than as a data defect. Same-venue history means the price the model learned
// from is the price the adapter sends orders against.
//
// # What knowledge_time means here, and why it is NOT the candle's close
//
// This is the decision that matters most in this package.
//
// A bar backfilled today for a minute last year became known to Kanz TODAY. That
// is what knowledge_time records, and stamping it with the candle's own close
// would assert the platform knew the price live — the exact claim #427 refused
// on the ingest path, for the exact reason it matters here: every point-in-time
// read would then treat a value obtained afterwards as though it had been
// available at the time.
//
// The consequence is worth stating plainly rather than discovering: a backtest
// AS OF a date BEFORE the backfill ran sees none of this history, and that is
// correct. You cannot reconstruct what you would have known in real time from
// data you only obtained later. What a backtest over backfilled history honestly
// says is "using everything we know now" — and its AsOf must therefore be at or
// after the run that loaded it.
//
// What the bitemporal axis still buys, and it is not nothing: when a venue
// RESTATES a candle, a later backfill records the new value under a later
// knowledge_time beside the old one, so the correction is visible as a
// correction instead of overwriting the evidence.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/store"
)

// Source fetches completed candles from one venue.
//
// It returns store.Bar with KnowledgeTime UNSET: when Kanz learned a candle is
// this package's decision, not the venue's, and a Source that stamped it would
// be asserting something it cannot know.
type Source interface {
	// Klines returns 1-minute candles whose BucketStart falls in [from, to),
	// ascending. A window with no trading returns none — that is data, not an
	// error.
	Klines(ctx context.Context, symbol string, from, to time.Time) ([]store.Bar, error)
}

// Result reports what a run did.
type Result struct {
	// Fetched is how many candles the venue returned.
	Fetched int
	// Written is how many were new or changed.
	Written int
	// Unchanged is how many the store already agreed with. On a re-run of an
	// unchanged range this equals Fetched and Written is zero.
	Unchanged int
	// Restated is how many the venue answered DIFFERENTLY from what was already
	// stored — a subset of Written, and the number worth alerting on.
	//
	// A NON-ZERO RESTATED COUNT IS NOT ROUTINE. It means history changed under a
	// model that may already have trained on it, and the old value is still
	// there to compare against precisely so that can be investigated.
	Restated int
}

// Backfiller loads history into the bar store.
type Backfiller struct {
	store store.BarStore
	now   func() time.Time
}

// New returns a Backfiller. A nil clock uses time.Now.
func New(st store.BarStore, now func() time.Time) (*Backfiller, error) {
	if st == nil {
		return nil, errors.New("backfill: store is required")
	}
	if now == nil {
		now = time.Now
	}
	return &Backfiller{store: st, now: now}, nil
}

// Run loads [from, to) for one series and writes only what the store does not
// already agree with.
//
// WRITE-IF-CHANGED, RATHER THAN WRITE-ALWAYS, and the reason is the knowledge
// axis. Every write here carries the run's own timestamp, so writing
// unconditionally would give each re-run a fresh version of every candle it
// touched — a table that doubles on every invocation, and a restatement history
// full of entries that restate nothing. Comparing first makes a re-run a no-op
// and makes a genuine venue correction stand out as the only thing that moved.
func (b *Backfiller) Run(ctx context.Context, src Source, s Series, from, to time.Time) (Result, error) {
	var res Result
	if err := s.validate(); err != nil {
		return res, err
	}
	if !from.Before(to) {
		return res, fmt.Errorf("backfill: window [%s, %s) is empty or inverted", from, to)
	}

	fetched, err := src.Klines(ctx, s.Symbol, from, to)
	if err != nil {
		return res, fmt.Errorf("backfill: fetch %s %s: %w", s.Venue, s.Symbol, err)
	}
	res.Fetched = len(fetched)
	if len(fetched) == 0 {
		return res, nil
	}

	// The knowledge horizon is UNSET on purpose: this compares against the latest
	// version of each bucket, whenever it was learned, because the question being
	// asked is "does the store already agree with the venue" and a horizon would
	// hide a more recent answer.
	known, err := b.store.Bars(ctx, store.BarQuery{
		InstrumentID: s.InstrumentID,
		Venue:        s.Venue,
		Resolution:   store.Resolution1m,
		From:         from,
		To:           to,
	})
	if err != nil {
		return res, fmt.Errorf("backfill: read existing %s %s: %w", s.Venue, s.InstrumentID, err)
	}
	byBucket := make(map[int64]store.Bar, len(known))
	for _, k := range known {
		byBucket[k.BucketStart.UTC().UnixNano()] = k
	}

	learnedAt := b.now().UTC()
	var write []store.Bar
	for _, f := range fetched {
		f.InstrumentID = s.InstrumentID
		f.Venue = s.Venue
		f.Resolution = store.Resolution1m
		f.KnowledgeTime = learnedAt

		prev, seen := byBucket[f.BucketStart.UTC().UnixNano()]
		switch {
		case !seen:
			write = append(write, f)
		case sameCandle(prev, f):
			res.Unchanged++
		default:
			res.Restated++
			write = append(write, f)
		}
	}
	if len(write) == 0 {
		return res, nil
	}
	if err := b.store.PutBars(ctx, write); err != nil {
		return res, fmt.Errorf("backfill: write %s %s: %w", s.Venue, s.InstrumentID, err)
	}
	res.Written = len(write)
	return res, nil
}

// Series names one candle stream to load.
type Series struct {
	// InstrumentID is the platform's canonical id — what an order carries.
	InstrumentID string
	// Symbol is what the EXCHANGE calls it, which is what the request uses. The
	// two differ, and #407 is the record of what it costs to conflate them.
	Symbol string
	// Venue is the ISO 10383 MIC, part of the series' identity.
	Venue string
}

func (s Series) validate() error {
	switch {
	case s.InstrumentID == "":
		return errors.New("backfill: instrument_id is required")
	case s.Symbol == "":
		return errors.New("backfill: venue symbol is required — the request needs the exchange's " +
			"name for the instrument, not the platform's")
	case s.Venue == "":
		return errors.New("backfill: venue is required — a candle series is per venue")
	}
	return nil
}

// sameCandle reports whether the venue's answer matches what is stored.
//
// COMPARED EXACTLY, through dec.Cmp rather than a float conversion: these are
// base-10 decimals and the whole point of storing them that way is not to leave
// the domain in order to compare them. A comparison that rounded would call a
// restatement unchanged, which is the one outcome this function exists to
// prevent.
func sameCandle(a, b store.Bar) bool {
	return dec.Cmp(a.Open, b.Open) == 0 &&
		dec.Cmp(a.High, b.High) == 0 &&
		dec.Cmp(a.Low, b.Low) == 0 &&
		dec.Cmp(a.Close, b.Close) == 0 &&
		dec.Cmp(a.Volume, b.Volume) == 0 &&
		a.TradeCount == b.TradeCount
}

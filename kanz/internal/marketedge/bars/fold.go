// Package bars folds a live trade stream into 1-minute OHLCV candles (#425).
//
// # Why this exists
//
// The bar store landed with nothing feeding it. market.v1.Bar is stored by
// internal/marketdata/ingest, but the only publisher of one is a synthetic feed
// adapter that is switched off in production — so ohlcv_bars was an empty table.
// Meanwhile four full-rate trade tapes run in market-ingest and are discarded
// after a minute. This is the join: the ticks the platform already receives
// become the candles it already knows how to store.
//
// # Why folding, and not scanning the tape
//
// internal/marketedge/trades.Tape retains a WINDOW (1 minute by default) and
// prunes on every Add against the newest event time. So at the moment a minute
// boundary is crossed, the start of that minute has ALREADY been pruned — a bar
// built by scanning the tape at the boundary is silently short, and short in a
// way that looks like a real candle. Accumulating as trades arrive is correct at
// any retention, including none.
//
// # A minute with no trades produces NO BAR
//
// It is tempting to emit a zero-volume candle: the market was quiet, and a gap
// in the series is awkward for an indicator. The problem is that this process
// cannot tell "nothing traded" from "the feed was down" or "we were rolling
// pods" — they look identical from here. A fabricated flat candle asserts the
// first, and an indicator that reads it computes a real number from a claim
// nobody checked. A gap is honest and is the caller's to interpret; liveness is
// a separate signal, and it belongs to whatever is watching the feed.
package bars

import (
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/marketedge/trades"
)

// Resolution is the only interval this folds to.
//
// ONE MINUTE, and not configurable here. store.Resolution1m is the only
// resolution the platform ingests — the coarser series are rollups derived from
// it, because two sources for one candle is two answers to one question. Making
// this a parameter would be the seam through which a second source arrives.
const Resolution = time.Minute

// Series identifies one candle stream. A bar from one venue is not a bar from
// another, so the venue is part of the identity rather than a label on it.
type Series struct {
	InstrumentID string
	Venue        string
}

// Bar is a completed candle, in exact arithmetic.
//
// It is deliberately NOT store.Bar: this package folds market state and knows
// nothing about persistence, knowledge time or the wire. The caller maps it.
type Bar struct {
	Series      Series
	BucketStart time.Time
	Open        *big.Rat
	High        *big.Rat
	Low         *big.Rat
	Close       *big.Rat
	Volume      *big.Rat
	TradeCount  int64
}

// accum is a candle under construction.
type accum struct {
	bucketStart time.Time
	open        *big.Rat
	high        *big.Rat
	low         *big.Rat
	close       *big.Rat
	volume      *big.Rat
	trades      int64
}

// Fold accumulates trades into candles, one per series.
//
// NOT SAFE FOR CONCURRENT USE. Each series is fed by its own feed goroutine in
// market-ingest and the caller owns the serialisation, exactly as trades.Tape
// leaves locking to its own mutex and the runner owns the fold order. Adding a
// mutex here would invite the assumption that Add and Flush can race, which they
// cannot: a Flush interleaved mid-Add would emit a candle missing the trade
// being applied.
type Fold struct {
	open map[Series]*accum
}

// New returns an empty Fold.
func New() *Fold { return &Fold{open: map[Series]*accum{}} }

// Add folds one trade and returns any candle it CLOSED.
//
// A trade closes the previous candle when it belongs to a later bucket — the
// boundary is discovered from the data rather than from a clock, so a replay
// produces exactly the same candles as a live run. Flush is what closes the
// final bucket when no later trade is coming.
//
// A trade with no price or a non-positive size is ignored, matching trades.Tape:
// a zero-size print is noise, not an execution.
//
// OUT-OF-ORDER TRADES ARE DROPPED, NOT BACKDATED. A print belonging to a bucket
// already closed cannot be folded without restating a candle the caller may have
// published, and a silent restatement is the one thing the bitemporal store
// exists to make impossible. The count is returned so the caller can surface it
// rather than discover it as a discrepancy later.
func (f *Fold) Add(s Series, tr trades.Trade) (closed *Bar, late bool) {
	if tr.Price == nil || tr.Price.Sign() <= 0 || tr.Size == nil || tr.Size.Sign() <= 0 {
		return nil, false
	}
	bucket := tr.EventTime.UTC().Truncate(Resolution)

	cur, ok := f.open[s]
	if !ok {
		f.open[s] = newAccum(bucket, tr)
		return nil, false
	}
	switch {
	case bucket.Equal(cur.bucketStart):
		cur.apply(tr)
		return nil, false
	case bucket.After(cur.bucketStart):
		done := cur.complete(s)
		f.open[s] = newAccum(bucket, tr)
		return done, false
	default:
		// Belongs to a bucket already emitted.
		return nil, true
	}
}

// Flush closes every candle whose bucket ended before now, and returns them.
//
// IT IS HOW A QUIET SERIES CLOSES. Add only discovers a boundary when a later
// trade arrives, so without this the last candle of a lull would sit open
// indefinitely — and on a thin instrument that is the normal case, not an edge
// one. The caller ticks this from the same loop that publishes book snapshots.
//
// A bucket is closed only once it is strictly in the past: a candle for the
// minute now in progress is incomplete, and publishing it would put a partial
// bar into a store whose whole contract is that a candle is what happened.
func (f *Fold) Flush(now time.Time) []Bar {
	cutoff := now.UTC().Truncate(Resolution)
	var out []Bar
	for s, cur := range f.open {
		if cur.bucketStart.Before(cutoff) {
			out = append(out, *cur.complete(s))
			delete(f.open, s)
		}
	}
	return out
}

func newAccum(bucket time.Time, tr trades.Trade) *accum {
	p := new(big.Rat).Set(tr.Price)
	return &accum{
		bucketStart: bucket,
		open:        new(big.Rat).Set(p),
		high:        new(big.Rat).Set(p),
		low:         new(big.Rat).Set(p),
		close:       new(big.Rat).Set(p),
		volume:      new(big.Rat).Set(tr.Size),
		trades:      1,
	}
}

func (a *accum) apply(tr trades.Trade) {
	if tr.Price.Cmp(a.high) > 0 {
		a.high.Set(tr.Price)
	}
	if tr.Price.Cmp(a.low) < 0 {
		a.low.Set(tr.Price)
	}
	a.close.Set(tr.Price)
	a.volume.Add(a.volume, tr.Size)
	a.trades++
}

func (a *accum) complete(s Series) *Bar {
	return &Bar{
		Series:      s,
		BucketStart: a.bucketStart,
		Open:        a.open,
		High:        a.high,
		Low:         a.low,
		Close:       a.close,
		Volume:      a.volume,
		TradeCount:  a.trades,
	}
}

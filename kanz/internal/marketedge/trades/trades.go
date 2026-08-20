// Package trades is the in-memory trade tape — the second half of the off-bus hot
// path, beside the L2 book.
//
// L2 depth says what is RESTING; the tape says what actually TRADED and who was
// the aggressor. That distinction is what volume delta is built on, and it cannot
// be recovered from depth: a level shrinking in the book could equally be a
// cancel or a fill, and only the trade stream tells you which. So the native-alpha
// edge folds both.
//
// Like the book, the tape lives and dies inside the process: the full-rate trade
// stream is NEVER republished on the bus. Prices and sizes are exact big.Rat —
// no float ever touches a number a trading decision reads.
package trades

import (
	"context"
	"math/big"
	"sync"
	"time"
)

// Side is the AGGRESSOR side of a trade — who crossed the spread. It is the
// taker, not the maker: a trade whose taker bought is buy-side volume. Volume
// delta is the imbalance between the two, so getting this backwards inverts every
// signal built on it.
type Side int

const (
	// SideUnknown means the venue did not report an aggressor.
	SideUnknown Side = iota
	// SideBuy: the taker bought (lifted the offer).
	SideBuy
	// SideSell: the taker sold (hit the bid).
	SideSell
)

// Trade is one execution on one venue.
type Trade struct {
	// Price and Size are exact.
	Price *big.Rat
	Size  *big.Rat
	// TakerSide is the aggressor.
	TakerSide Side
	// EventTime is the venue timestamp — authoritative for windowing.
	EventTime time.Time
}

// TradeSource is the trade-feed transport seam, the tape's analog of
// depth.DepthSource. Recv blocks until the next trade or ctx cancellation.
// Implementations own reconnect; on an unrecoverable stream error they return it
// and the caller rebinds.
type TradeSource interface {
	Recv(ctx context.Context) (Trade, error)
}

// Liveness is how a subscription reports that it is ALIVE, and it is the seam
// the ingestion-coverage record is built on (#591).
//
// # Why a source reports this at all, when it already returns trades
//
// A trade proves the subscription was live. The absence of a trade proves
// nothing — fold.go's whole point is that "nothing traded" and "the feed was
// down" are indistinguishable from the data. So a liveness signal derived from
// TRADES would be uninformative in exactly the case it is needed, and a coverage
// record built on it would report a quiet minute as an unobserved one.
//
// WHAT MUST BE REPORTED IS A HEARTBEAT: a websocket ping that came back, a venue
// keepalive, a control frame — evidence from the TRANSPORT that the socket is
// round-tripping, which is available whether or not the market is doing anything.
// That is why this seam lives on the source and not on the fold: only the thing
// holding the socket can see it.
//
// A nil Liveness is valid and means NOTHING IS ATTESTED. The intervals that
// subscription covers will read downstream as UNKNOWN — which is the honest
// answer for a feed nobody is vouching for, and is deliberately distinct from an
// attestation that says the feed was down.
type Liveness interface {
	// Live records that the subscription round-tripped a frame at `at`.
	Live(at time.Time)
	// Down records that the subscription FAILED at `at`.
	Down(at time.Time, err error)
}

// HeartbeatInterval is how often a source proves its socket is still round-
// tripping when the market is quiet.
//
// 20 SECONDS BECAUSE OKX DROPS AN IDLE STREAM AT 30 (see OKXSource.keepAlive,
// which has pinged at this cadence since before coverage existed). Binance now
// matches it rather than picking its own number: the coverage recorder's silence
// tolerance is one value for every feed, and two cadences would mean it was
// either too slack for one or too tight for the other.
const HeartbeatInterval = 20 * time.Second

// HeartbeatTimeout bounds one heartbeat round trip.
//
// A ping that never returns must not wedge the keepalive goroutine forever: the
// point of the heartbeat is to STOP reporting liveness when the socket is dead,
// and a blocked Ping reports neither Live nor Down — it just goes quiet, which
// the recorder reads as an uncredited gap. That is the safe direction, but a
// bounded timeout gets the connection torn down and reconnected instead.
const HeartbeatTimeout = 10 * time.Second

// Tape is a bounded, time-windowed rolling record of one instrument's trades on
// one venue. Safe for concurrent use: the fold goroutine appends while engine
// ticks read.
//
// It is deliberately bounded by a retention window rather than a trade count: an
// unbounded tape on a busy instrument is a memory leak in a long-lived edge
// process, and a count-bounded one silently changes its time horizon with market
// activity — the same window would mean 10 minutes in a quiet market and 4 seconds
// in a volatile one, which is precisely when the number matters.
type Tape struct {
	instrumentID string
	mic          string
	retention    time.Duration

	mu     sync.RWMutex
	trades []Trade // append-ordered by arrival; pruned to the retention window
	last   *big.Rat
}

// New returns an empty tape retaining trades for the given window (<=0 ⇒ 1m).
func New(instrumentID, mic string, retention time.Duration) *Tape {
	if retention <= 0 {
		retention = time.Minute
	}
	return &Tape{instrumentID: instrumentID, mic: mic, retention: retention}
}

// Add folds one trade and prunes anything past the retention window.
func (t *Tape) Add(tr Trade) {
	if tr.Size == nil || tr.Size.Sign() <= 0 {
		return // a zero-size trade is noise, not an execution
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.trades = append(t.trades, tr)
	if tr.Price != nil && tr.Price.Sign() > 0 {
		t.last = new(big.Rat).Set(tr.Price)
	}
	t.pruneLocked(tr.EventTime)
}

// pruneLocked drops trades older than the retention window, measured from the
// newest event time (not wall-clock, so a replayed or lagging feed prunes against
// its own clock rather than silently emptying the tape).
func (t *Tape) pruneLocked(newest time.Time) {
	if newest.IsZero() {
		return
	}
	cutoff := newest.Add(-t.retention)
	i := 0
	for i < len(t.trades) && t.trades[i].EventTime.Before(cutoff) {
		i++
	}
	if i > 0 {
		t.trades = append(t.trades[:0], t.trades[i:]...)
	}
}

// Volumes returns the exact aggressive buy and sell volume within `window` of the
// most recent trade. A window longer than the tape's retention is clamped to it —
// the tape never invents volume it no longer holds.
//
// It returns the two sides SEPARATELY rather than a single delta: buy/sell is
// strictly more information, and what a strategy does with the imbalance is the
// strategy's business, not the tape's.
func (t *Tape) Volumes(window time.Duration) (buy, sell *big.Rat) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	buy, sell = new(big.Rat), new(big.Rat)
	if len(t.trades) == 0 {
		return buy, sell
	}
	if window <= 0 || window > t.retention {
		window = t.retention
	}
	cutoff := t.trades[len(t.trades)-1].EventTime.Add(-window)
	for i := len(t.trades) - 1; i >= 0; i-- {
		tr := t.trades[i]
		if tr.EventTime.Before(cutoff) {
			break
		}
		switch tr.TakerSide {
		case SideBuy:
			buy.Add(buy, tr.Size)
		case SideSell:
			sell.Add(sell, tr.Size)
		}
		// An unknown aggressor contributes to NEITHER side. Attributing it to one
		// would fabricate directional pressure that the venue never reported.
	}
	return buy, sell
}

// LastPrice returns the most recent trade price, or nil if nothing has traded.
func (t *Tape) LastPrice() *big.Rat {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.last == nil {
		return nil
	}
	return new(big.Rat).Set(t.last)
}

// Len reports the number of retained trades (observability).
func (t *Tape) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.trades)
}

// InstrumentID / MIC identify the tape.
func (t *Tape) InstrumentID() string { return t.instrumentID }
func (t *Tape) MIC() string          { return t.mic }

// Fold drives src into the tape until ctx is cancelled, returning the first fatal
// source error. It is the trade-side analog of the depth fold loop.
func (t *Tape) Fold(ctx context.Context, src TradeSource) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		tr, err := src.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		t.Add(tr)
	}
}

// Package alpha is the BOUNDARY between Kanz's open platform and its proprietary
// native-alpha engines. It is the only surface the restricted alpha layer imports.
//
// # What lives here, and what deliberately does not
//
// The quantitative strategy math — order-book imbalance, microprice, volume-delta
// thresholds, cross-venue arbitrage — does NOT live in this repository (house
// rule; see KANZ_BRAIN.md). What lives here is everything AROUND that math:
//
//   - MarketView: the read-seam over the off-bus, in-memory book + trade tape.
//     Raw, exact market state. No derived indicators.
//   - Engine: the interface the restricted layer implements. It receives the
//     views and returns intents. Kanz never sees inside it.
//   - Runner: owns the edge — folds depth and trades into memory, ticks the
//     engines, and emits what they decide.
//   - The emit port: an engine's Intent becomes a signal.v1.StrategySignal FACT
//     stamped SIGNAL_SOURCE_NATIVE_ENGINE plus the order commands the OMS
//     executes, through the SAME shared translator the TradingView webhook path
//     uses. There is exactly one execution path; native alpha does not get a
//     private one that could drift away from the pre-trade gates.
//
// # Why the engine runs in-process
//
// The whole point of the market-ingest edge is that raw L2 depth never rides the
// bus at full rate. An engine reading the book over the network would be reading
// a bounded, periodic snapshot — it would lose exactly the per-tick latency it
// exists for. So the engine is linked into the edge binary and reads the live
// in-memory book directly. The restricted layer therefore imports THIS package and
// builds its own edge binary; Kanz's open binary runs the same Runner with no
// engines registered.
package alpha

import (
	"context"
	"math/big"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
)

// Level is one aggregated L2 price level. Exact — no float ever touches depth a
// pricing decision reads.
type Level struct {
	Price *big.Rat
	Size  *big.Rat
}

// MarketView is the read-seam: one instrument's live state on ONE venue, as the
// edge holds it in memory right now. It is raw market state, not indicators — the
// engine derives whatever it wants from this.
//
// Views are per-venue and never merged: a cross-venue engine receives one view per
// venue and compares them itself. Merging books across venues in the platform
// would destroy exactly the spread the arbitrage engine exists to see.
type MarketView interface {
	// InstrumentID is the canonical Kanz instrument.
	InstrumentID() string
	// MIC is the venue this view is of (e.g. "BINANCE", "OKX").
	MIC() string

	// BestBid / BestAsk are the top of book, or nil when that side is empty.
	BestBid() *big.Rat
	BestAsk() *big.Rat
	// Bids / Asks are the top-n levels, best first. n <= 0 ⇒ the full book.
	Bids(n int) []Level
	Asks(n int) []Level

	// Volumes returns the exact AGGRESSIVE buy and sell volume traded within
	// `window` of the last trade — the volume-delta input. The two sides come back
	// separately; what a strategy does with the imbalance is the strategy's
	// business. A window beyond the tape's retention is clamped to it: the edge
	// never invents volume it no longer holds.
	Volumes(window time.Duration) (buy, sell *big.Rat)
	// LastPrice is the most recent trade price, or nil if nothing has traded.
	LastPrice() *big.Rat
}

// Intent is what an engine decided: a trading intent, not an order. It is
// deliberately the same shape the TradingView path produces, because both are
// advisory intent that the OMS's pre-trade gates still adjudicate.
//
// Sizing is exact (big.Rat) and deny-by-default: an intent with no positive size,
// no direction, or no fund never reaches a venue.
type Intent struct {
	// StrategyID names the engine that fired (e.g. "obi-v1"). It becomes the
	// command issuer principal, "strategy:{id}", and is what an audit traces back
	// to. Required.
	StrategyID string
	// FundID is the fund whose capital this trades. Required.
	FundID string
	// InstrumentID is the canonical instrument. Required.
	InstrumentID string

	// Action is the directional intent. Required.
	Action signalpb.SignalAction
	// Size is interpreted per SizeType, strictly positive (except for CLOSE, which
	// flattens whatever exists and carries no size).
	Size     *big.Rat
	SizeType signalpb.SizeType

	// OrderType / LimitPrice price the legs.
	OrderType  orderpb.OrderType
	LimitPrice *big.Rat

	// TimeInForce governs how a leg rests.
	//
	// A CROSS-VENUE ARBITRAGE LEG MUST BE IOC. The two legs of an arb are only
	// market-neutral together: if one fills and the other rests unfilled, the
	// hedge is gone and what is left is naked directional exposure at the exact
	// moment the strategy believed itself flat. IOC makes an unfillable leg
	// disappear instead of quietly becoming a position.
	//
	// Zero ⇒ DAY, which is correct for a directional signal and WRONG for an arb
	// leg — an arbitrage engine must set this explicitly.
	TimeInForce orderpb.TimeInForce

	// Nonce makes the derived signal_id deterministic and unique per decision. Two
	// ticks that produce the same nonce dedup to one signal and one fan-out rather
	// than double-trading — so an engine that re-fires on an unchanged book must
	// reuse the nonce, and one that makes a genuinely new decision must not.
	// Required.
	Nonce string
}

// Engine is what the restricted alpha layer implements. Kanz calls Evaluate on
// every tick with the current view of every tracked (instrument, venue); the
// engine returns whatever it wants to trade, or nothing.
//
// Evaluate MUST NOT block: it runs on the edge's tick loop, and a slow engine
// stalls the very hot path it was built for. It must also be safe to call
// concurrently with the fold goroutines that are mutating the books beneath it —
// the views handle that locking, but any state the engine keeps is its own to
// protect.
type Engine interface {
	// Name identifies the engine in logs and metrics.
	Name() string
	// Evaluate inspects the current market and returns zero or more intents.
	// Returning nothing is the overwhelmingly common case and is not an error.
	Evaluate(ctx context.Context, views []MarketView) []Intent
}

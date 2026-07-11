package alpha

import (
	"time"

	"github.com/kanz-eng/kanz/internal/marketedge/depth"
	"github.com/kanz-eng/kanz/internal/marketedge/trades"
)

// The edge's transport seams, re-exported under the public boundary.
//
// These are type ALIASES, not wrappers: exact type identity, no conversion. They
// exist because Go forbids an external module from importing kanz/internal/... —
// so without them the restricted alpha layer could reference alpha.Feed but could
// never NAME the type of its Depth field, and therefore could neither supply its
// own source nor construct a live venue feed. The boundary would compile and be
// useless.
//
// With the aliases, the restricted layer implements alpha.DepthSource /
// alpha.TradeSource directly and never imports a kanz internal package.
type (
	// DepthSource is the L2 depth transport seam. Recv blocks until the next update.
	DepthSource = depth.DepthSource
	// DepthUpdate is one depth message: exactly one of Snapshot or Delta is set.
	DepthUpdate = depth.Update

	// TradeSource is the trade-tape transport seam.
	TradeSource = trades.TradeSource
	// Trade is one execution, carrying the aggressor side.
	Trade = trades.Trade
	// TradeSide is the AGGRESSOR (taker) side of a trade.
	TradeSide = trades.Side
)

// Aggressor sides. Volume delta is the imbalance between TakerBuy and TakerSell,
// so an UNKNOWN aggressor contributes to neither — never guessed into a direction.
const (
	TakerUnknown = trades.SideUnknown
	TakerBuy     = trades.SideBuy
	TakerSell    = trades.SideSell
)

// SimFeed returns a deterministic, vendor-free feed for an instrument — the
// default when no exchange is compiled in. It is a SIMULATION (mic "SIM") and is
// never presented as real market data; it exists so the fold → view → engine →
// emit path is exercisable end to end without a network.
func SimFeed(instrumentID string) Feed {
	return Feed{
		InstrumentID: instrumentID,
		MIC:          "SIM",
		Depth:        depth.NewSimSource(depth.SimConfig{InstrumentID: instrumentID, Symbol: instrumentID}),
		Trades:       trades.NewSimSource(trades.SimConfig{}),
	}
}

// VenueFeedConfig configures a live exchange feed. Exchange depth and trades are
// PUBLIC market data, so — unlike the OMS connectors — there is no API key here.
type VenueFeedConfig struct {
	// InstrumentID is the canonical Kanz instrument.
	InstrumentID string
	// Symbol is the venue's identifier for it (BTCUSDT on Binance, BTC-USDT on OKX).
	Symbol string
	// MIC is the venue code stamped on the book and every resulting order.
	MIC string
	// WSBase / WSURL is the websocket origin; RESTBase is Binance's snapshot anchor.
	WSBase   string
	RESTBase string
	// DepthLimit is the REST snapshot depth (Binance). <=0 ⇒ 1000.
	DepthLimit int
	// DNSTTL is the DNS-bypass cache TTL on the live exchange path.
	DNSTTL time.Duration
}

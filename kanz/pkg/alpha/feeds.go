package alpha

import (
	"time"

	"github.com/eighred/kanz/internal/marketedge/coverage"
	"github.com/eighred/kanz/internal/marketedge/depth"
	"github.com/eighred/kanz/internal/marketedge/trades"
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

	// CoverageRecorder is the ingestion-coverage record: a first-hand statement
	// of which intervals a feed subscription was demonstrably live for (#591).
	CoverageRecorder = coverage.Recorder
	// CoverageConfig configures a CoverageRecorder.
	CoverageConfig = coverage.Config
)

// NewCoverageRecorder builds the ingestion-coverage record.
//
// RE-EXPORTED, NOT JUST ALIASED, and that is the difference between a boundary
// that works and one that compiles. Without a constructor the restricted layer
// could NAME VenueFeedConfig.Coverage and never obtain one — so its only legal
// value would be nil, and a nil recorder attests nothing while every feed keeps
// running. The whole estate would silently lose the record on the one binary
// that is not this repository's.
func NewCoverageRecorder(cfg CoverageConfig) (*CoverageRecorder, error) {
	return coverage.NewRecorder(cfg)
}

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

	// Coverage is the ingestion-coverage record this feed attests to (#591).
	//
	// NIL ATTESTS NOTHING, and that is a real state rather than a disabled
	// feature: every interval this subscription covers reads downstream as
	// UNKNOWN, which is honest for a feed nobody is vouching for and is
	// deliberately distinct from an attestation saying the feed was down.
	//
	// It is a SEPARATE field from anything the feed needs to function, so turning
	// the record on is a deliberate act at the composition root — the same shape
	// alpha.Config.Bars takes, and for the same reason: a record this platform
	// depends on must not arrive as a side effect of having wired a bus.
	Coverage *coverage.Recorder
}

// coverageObserver binds this feed's subscription to the record, if one is
// configured.
//
// THE VENUE IS RESOLVED FIRST, by the caller, because coverage is per venue and
// a series keyed on an empty MIC would vouch for a feed that does not exist.
func (c VenueFeedConfig) coverageObserver(mic, attestor string) trades.Liveness {
	if c.Coverage == nil {
		// A TYPED NIL WOULD NOT BE NIL. Returning c.Coverage.Observe(...) on a nil
		// recorder, or a nil *coverage.Observer through this interface, both give
		// the source a non-nil Liveness whose calls do nothing — and the source
		// cannot tell that from a working one. Nil here means nil there.
		return nil
	}
	return c.Coverage.Observe(coverage.Series{InstrumentID: c.InstrumentID, Venue: mic}, attestor)
}

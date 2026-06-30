package feed

import (
	"context"
	"fmt"
	"time"
)

// PARITY-01b — Bloomberg (B-PIPE / BLPAPI) market-data adapter. BLPAPI delivers
// field/value subscription messages keyed by a Bloomberg security string
// ("AAPL US Equity"); this maps that native shape onto the canonical event via
// the shared normalizer. The real BLPAPI session implements BloombergSource at
// the composition root (`cmd/market-data`); the decode + lifecycle below are
// vendor-SDK-free and conformance-tested here.

// BloombergTick is the subset of a BLPAPI market-data message the adapter reads.
// Prices are the native float64 BLPAPI delivers (bridged to exact Decimal at
// priceExp).
type BloombergTick struct {
	Security string // Bloomberg ticker, e.g. "AAPL US Equity" (SchemeBloomberg)
	MIC      string // venue MIC, e.g. "XNAS"
	Type     string // "TRADE" | "QUOTE" (the MKTDATA_EVENT_TYPE)
	Time     time.Time
	Seq      uint64 // BLPAPI sequence, for source-level gap detection

	LastPrice float64 // LAST_PRICE (trade)
	LastSize  float64 // SIZE_LAST_TRADE (trade)
	TradeID   string

	Bid, Ask         float64 // BID / ASK (quote)
	BidSize, AskSize float64 // BID_SIZE / ASK_SIZE (quote)
}

// BloombergSource is the BLPAPI transport seam: subscribe to securities and
// stream their ticks until the session drops (the channel closes) or ctx is
// canceled. The real B-PIPE session implements this at the composition root.
type BloombergSource interface {
	Subscribe(ctx context.Context, securities []string) (<-chan BloombergTick, error)
}

func decodeBloomberg(t BloombergTick) (RawTick, error) {
	r := RawTick{VendorSymbol: t.Security, MIC: t.MIC, EventTime: t.Time, SourceSequence: t.Seq}
	switch t.Type {
	case "TRADE":
		r.Kind = KindTrade
		r.Price = DecimalFromFloat(t.LastPrice, priceExp)
		r.Size = DecimalFromFloat(t.LastSize, 0)
		r.TradeID = t.TradeID
	case "QUOTE":
		r.Kind = KindQuote
		r.BidPrice = DecimalFromFloat(t.Bid, priceExp)
		r.BidSize = DecimalFromFloat(t.BidSize, 0)
		r.AskPrice = DecimalFromFloat(t.Ask, priceExp)
		r.AskSize = DecimalFromFloat(t.AskSize, 0)
	default:
		return RawTick{}, fmt.Errorf("bloomberg: unknown event type %q", t.Type)
	}
	return r, nil
}

// NewBloomberg builds the Bloomberg adapter over a BLPAPI source + the symbology
// crosswalk (Bloomberg ticker ↔ canonical instrument_id).
func NewBloomberg(src BloombergSource, xwalk Crosswalk) *Driver[BloombergTick] {
	return &Driver[BloombergTick]{
		vendor:  "BLOOMBERG",
		scheme:  SchemeBloomberg,
		connect: src.Subscribe,
		decode:  decodeBloomberg,
		xwalk:   xwalk,
		base:    defaultBase,
		max:     defaultMax,
	}
}

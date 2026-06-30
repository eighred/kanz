package feed

import (
	"context"
	"fmt"
	"time"
)

// PARITY-01d — ICE Consolidated Feed market-data adapter. The ICE feed handler
// delivers compact messages keyed by an ICE symbol with a single-char message
// type ("T" trade, "Q" quote) and an exchange code that maps to the MIC; it
// passes venue condition codes through. The real feed handler implements
// ICESource at the composition root; the decode + lifecycle are vendor-SDK-free
// and conformance-tested here, on the same shared Driver.

// ICETick is the subset of an ICE Consolidated Feed message the adapter reads.
type ICETick struct {
	Symbol  string // ICE Consolidated Feed symbol (SchemeICE)
	MIC     string // resolved from the ICE exchange code
	MsgType string // "T" (trade) | "Q" (quote)
	Time    time.Time
	SeqNum  uint64

	Price   float64 // trade price
	Size    float64 // trade size
	TradeID string

	BidPx, AskPx float64 // quote
	BidSz, AskSz float64

	Conditions []string // venue condition codes, passed through verbatim
}

// ICESource is the ICE feed-handler transport seam.
type ICESource interface {
	Subscribe(ctx context.Context, symbols []string) (<-chan ICETick, error)
}

func decodeICE(t ICETick) (RawTick, error) {
	r := RawTick{VendorSymbol: t.Symbol, MIC: t.MIC, EventTime: t.Time, SourceSequence: t.SeqNum}
	switch t.MsgType {
	case "T":
		r.Kind = KindTrade
		r.Price = DecimalFromFloat(t.Price, priceExp)
		r.Size = DecimalFromFloat(t.Size, 0)
		r.TradeID = t.TradeID
	case "Q":
		r.Kind = KindQuote
		r.BidPrice = DecimalFromFloat(t.BidPx, priceExp)
		r.BidSize = DecimalFromFloat(t.BidSz, 0)
		r.AskPrice = DecimalFromFloat(t.AskPx, priceExp)
		r.AskSize = DecimalFromFloat(t.AskSz, 0)
	default:
		return RawTick{}, fmt.Errorf("ice: unknown message type %q", t.MsgType)
	}
	return r, nil
}

// NewICE builds the ICE adapter over an ICE feed-handler source + the ICE
// symbol ↔ instrument_id crosswalk.
func NewICE(src ICESource, xwalk Crosswalk) *Driver[ICETick] {
	return &Driver[ICETick]{
		vendor:  "ICE",
		scheme:  SchemeICE,
		connect: src.Subscribe,
		decode:  decodeICE,
		xwalk:   xwalk,
		base:    defaultBase,
		max:     defaultMax,
	}
}

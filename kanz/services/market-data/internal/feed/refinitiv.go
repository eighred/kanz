package feed

import (
	"context"
	"fmt"
	"time"
)

// PARITY-01c — Refinitiv / LSEG (RTSDK / Elektron) market-data adapter. RTSDK
// delivers field-list updates keyed by a RIC ("AAPL.O"), with trade fields
// (TRDPRC_1/TRDVOL_1) and quote fields (BID/ASK/BIDSIZE/ASKSIZE). The real
// OMM consumer implements RefinitivSource at the composition root; the decode +
// lifecycle are vendor-SDK-free and conformance-tested here. Same shared Driver
// as Bloomberg — only the native shape, the field mapping, and the symbology
// (RIC) differ.

// RefinitivTick is the subset of an RTSDK field-list update the adapter reads.
type RefinitivTick struct {
	RIC    string // Refinitiv Instrument Code, e.g. "AAPL.O" (SchemeRIC)
	MIC    string
	Update string // "TRADE" | "QUOTE" (the update-type the OMM domain maps to)
	Time   time.Time
	Seq    uint64

	TrdPrice float64 // TRDPRC_1 (trade)
	TrdVol   float64 // TRDVOL_1 (trade)
	TradeID  string

	Bid, Ask         float64 // BID / ASK (quote)
	BidSize, AskSize float64 // BIDSIZE / ASKSIZE (quote)
}

// RefinitivSource is the RTSDK transport seam.
type RefinitivSource interface {
	Subscribe(ctx context.Context, rics []string) (<-chan RefinitivTick, error)
}

func decodeRefinitiv(t RefinitivTick) (RawTick, error) {
	r := RawTick{VendorSymbol: t.RIC, MIC: t.MIC, EventTime: t.Time, SourceSequence: t.Seq}
	switch t.Update {
	case "TRADE":
		r.Kind = KindTrade
		r.Price = DecimalFromFloat(t.TrdPrice, priceExp)
		r.Size = DecimalFromFloat(t.TrdVol, 0)
		r.TradeID = t.TradeID
	case "QUOTE":
		r.Kind = KindQuote
		r.BidPrice = DecimalFromFloat(t.Bid, priceExp)
		r.BidSize = DecimalFromFloat(t.BidSize, 0)
		r.AskPrice = DecimalFromFloat(t.Ask, priceExp)
		r.AskSize = DecimalFromFloat(t.AskSize, 0)
	default:
		return RawTick{}, fmt.Errorf("refinitiv: unknown update type %q", t.Update)
	}
	return r, nil
}

// NewRefinitiv builds the Refinitiv adapter over an RTSDK source + the RIC ↔
// instrument_id crosswalk.
func NewRefinitiv(src RefinitivSource, xwalk Crosswalk) *Driver[RefinitivTick] {
	return &Driver[RefinitivTick]{
		vendor:  "REFINITIV",
		scheme:  SchemeRIC,
		connect: src.Subscribe,
		decode:  decodeRefinitiv,
		xwalk:   xwalk,
		base:    defaultBase,
		max:     defaultMax,
	}
}

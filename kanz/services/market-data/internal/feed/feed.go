// Package feed is the market-data ingestion seam (PARITY-01a): the vendor-
// agnostic contract a market-data source implements, plus the shared, validating
// normalizer that turns a vendor tick into a canonical market.v1.MarketDataEvent.
//
// The market-data SERVICE today only CONSUMES market.v1 events off the bus
// (MODEL-01b) and folds them into the price-history store — it assumes the
// events already exist. This package closes that gap: an Adapter is a live
// vendor connection that streams NORMALIZED events to a Sink (the composition
// root binds the Sink to a bus producer publishing market.v1, where the existing
// consumer picks them up). The concrete Bloomberg / Refinitiv / ICE adapters
// (PARITY-01b-d) implement Adapter behind their proprietary SDKs at the
// composition root; the dependency-free SimAdapter is the default for tests and
// local boot — the SimVenue/SimFeed stance every external integration takes.
//
// One normalizer, conformance-tested: every adapter builds events through the
// Trade/Quote/Bar constructors here, which enforce the market.v1 "Required"
// invariants (envelope-policy §7) so a malformed vendor tick is rejected at the
// edge rather than published downstream.
package feed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Sink receives a normalized event. Publish is called SYNCHRONOUSLY by the
// adapter, so a slow sink naturally backpressures the source — an adapter must
// not unbounded-buffer ahead of the sink. A returned error is a publish/
// backpressure failure the adapter surfaces (retry or fail per its policy). The
// composition root binds this to a bus producer; tests bind a capturing sink.
type Sink interface {
	Publish(ctx context.Context, ev *marketpb.MarketDataEvent) error
}

// SinkFunc adapts a function to a Sink.
type SinkFunc func(ctx context.Context, ev *marketpb.MarketDataEvent) error

// Publish calls f.
func (f SinkFunc) Publish(ctx context.Context, ev *marketpb.MarketDataEvent) error {
	return f(ctx, ev)
}

// Adapter is a live vendor market-data source. Run subscribes to instruments
// and streams normalized events to sink until ctx is canceled; it reconnects
// internally on transient faults (see Reconnect) and returns only on ctx
// cancellation (nil) or an unrecoverable error. Vendor is the source name, for
// telemetry + the source label.
type Adapter interface {
	Vendor() string
	Run(ctx context.Context, instruments []string, sink Sink) error
}

// Errors returned by the normalizer when a vendor tick violates a market.v1
// "Required" invariant.
var (
	ErrNoInstrument = errors.New("feed: instrument_id is required")
	ErrNoSymbol     = errors.New("feed: symbol is required")
	ErrNoMIC        = errors.New("feed: mic is required")
	ErrNoEventTime  = errors.New("feed: event_time is required")
	ErrNoData       = errors.New("feed: event carries no data variant")
	ErrBadDecimal   = errors.New("feed: price/size is required and must be present")
)

// Meta is the envelope every normalized event carries — the fields common to
// trades, quotes, and bars. The instrument_id is the canonical Kanz id (the
// vendor adapter resolves its native symbol via the MASTER crosswalk, PARITY-01e).
type Meta struct {
	InstrumentID   string
	Symbol         string
	MIC            string
	EventTime      time.Time
	SourceSequence uint64 // venue sequence for source-level gap detection; 0 if none
}

// Trade builds and validates a canonical trade event. price and size are
// required (a trade without them is malformed).
func Trade(m Meta, price, size *commonpb.Decimal, tradeID string) (*marketpb.MarketDataEvent, error) {
	if price == nil || size == nil {
		return nil, ErrBadDecimal
	}
	ev, err := base(m)
	if err != nil {
		return nil, err
	}
	ev.Data = &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
		Price: price, Size: size, TradeId: tradeID,
	}}
	return ev, nil
}

// Quote builds and validates a canonical top-of-book quote event. All four
// bid/ask legs are required.
func Quote(m Meta, bidPrice, bidSize, askPrice, askSize *commonpb.Decimal) (*marketpb.MarketDataEvent, error) {
	if bidPrice == nil || bidSize == nil || askPrice == nil || askSize == nil {
		return nil, ErrBadDecimal
	}
	ev, err := base(m)
	if err != nil {
		return nil, err
	}
	ev.Data = &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
		BidPrice: bidPrice, BidSize: bidSize, AskPrice: askPrice, AskSize: askSize,
	}}
	return ev, nil
}

// Bar builds and validates a canonical OHLCV bar event. The enclosing
// event_time is the bar close (market.v1 convention).
func Bar(m Meta, open, high, low, close_, volume *commonpb.Decimal, openTime time.Time, tradeCount uint64) (*marketpb.MarketDataEvent, error) {
	if open == nil || high == nil || low == nil || close_ == nil || volume == nil {
		return nil, ErrBadDecimal
	}
	ev, err := base(m)
	if err != nil {
		return nil, err
	}
	ev.Data = &marketpb.MarketDataEvent_Bar{Bar: &marketpb.Bar{
		OpenTime:   timestamppb.New(openTime),
		CloseTime:  ev.EventTime,
		Open:       open,
		High:       high,
		Low:        low,
		Close:      close_,
		Volume:     volume,
		TradeCount: tradeCount,
	}}
	return ev, nil
}

// base validates the envelope fields shared by every variant.
func base(m Meta) (*marketpb.MarketDataEvent, error) {
	switch {
	case m.InstrumentID == "":
		return nil, ErrNoInstrument
	case m.Symbol == "":
		return nil, ErrNoSymbol
	case m.MIC == "":
		return nil, ErrNoMIC
	case m.EventTime.IsZero():
		return nil, ErrNoEventTime
	}
	return &marketpb.MarketDataEvent{
		InstrumentId:   m.InstrumentID,
		Symbol:         m.Symbol,
		Mic:            m.MIC,
		EventTime:      timestamppb.New(m.EventTime),
		SourceSequence: m.SourceSequence,
	}, nil
}

// Validate reports whether an event satisfies the market.v1 required-field
// invariants — the gate the conformance harness (and the live DQ gate, PARITY-
// 01g) runs over a stream. nil ⇒ valid.
func Validate(ev *marketpb.MarketDataEvent) error {
	if ev == nil {
		return ErrNoData
	}
	switch {
	case ev.GetInstrumentId() == "":
		return ErrNoInstrument
	case ev.GetSymbol() == "":
		return ErrNoSymbol
	case ev.GetMic() == "":
		return ErrNoMIC
	case ev.GetEventTime() == nil:
		return ErrNoEventTime
	}
	switch d := ev.GetData().(type) {
	case *marketpb.MarketDataEvent_Trade:
		if d.Trade.GetPrice() == nil || d.Trade.GetSize() == nil {
			return ErrBadDecimal
		}
	case *marketpb.MarketDataEvent_Quote:
		q := d.Quote
		if q.GetBidPrice() == nil || q.GetBidSize() == nil || q.GetAskPrice() == nil || q.GetAskSize() == nil {
			return ErrBadDecimal
		}
	case *marketpb.MarketDataEvent_Bar:
		b := d.Bar
		if b.GetOpen() == nil || b.GetHigh() == nil || b.GetLow() == nil || b.GetClose() == nil || b.GetVolume() == nil {
			return ErrBadDecimal
		}
	default:
		return ErrNoData
	}
	return nil
}

// DecimalFromFloat builds a common.v1.Decimal at the given exponent (value =
// coefficient·10^exponent), rounding to nearest — the float→exact-money bridge a
// vendor adapter uses when its native tick is a float. Vendors that deliver a
// decimal string should parse it exactly rather than going through float.
func DecimalFromFloat(f float64, exponent int32) *commonpb.Decimal {
	scale := math.Pow(10, float64(-exponent))
	return &commonpb.Decimal{Coefficient: int64(math.Round(f * scale)), Exponent: exponent}
}

// Gap reports a per-instrument source-sequence break in a stream: for each
// instrument, sequences (when non-zero) must increase by exactly 1. A jump is a
// dropped-message gap; a repeat/decrease is a duplicate/out-of-order. Returns a
// human-readable description per break, empty when the stream is gap-free.
func Gap(events []*marketpb.MarketDataEvent) []string {
	last := map[string]uint64{}
	var breaks []string
	for _, ev := range events {
		seq := ev.GetSourceSequence()
		if seq == 0 {
			continue // venue provides no sequence — nothing to check
		}
		id := ev.GetInstrumentId()
		if prev, ok := last[id]; ok && seq != prev+1 {
			breaks = append(breaks, fmt.Sprintf("%s: sequence %d follows %d (expected %d)", id, seq, prev, prev+1))
		}
		last[id] = seq
	}
	return breaks
}

// OutOfOrder reports per-instrument event_time inversions (a tick older than its
// predecessor for the same instrument) — the ordering invariant a per-instrument
// partition must hold. Empty when ordered.
func OutOfOrder(events []*marketpb.MarketDataEvent) []string {
	last := map[string]time.Time{}
	var bad []string
	for _, ev := range events {
		id := ev.GetInstrumentId()
		t := ev.GetEventTime().AsTime()
		if prev, ok := last[id]; ok && t.Before(prev) {
			bad = append(bad, fmt.Sprintf("%s: event_time %s precedes %s", id, t.UTC().Format(time.RFC3339Nano), prev.UTC().Format(time.RFC3339Nano)))
		}
		last[id] = t
	}
	return bad
}

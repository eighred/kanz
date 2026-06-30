package feed

import (
	"context"
	"errors"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
)

// Vendor-adapter engine (PARITY-01b–d). The three vendor adapters
// (Bloomberg/Refinitiv/ICE) share one lifecycle — connect → receive native
// ticks → decode → resolve symbology → normalize → publish, reconnecting on a
// transient disconnect — and differ ONLY in (a) the native tick shape, (b) the
// field mapping (a per-vendor Decoder), and (c) the symbology scheme. The shared
// part is the generic Driver here; the per-vendor part is small (a native type +
// a Source seam + a decode func), so there is no parallel re-implementation.
//
// The proprietary vendor SDK (BLPAPI / Refinitiv RTSDK / ICE feed handler) is the
// Source seam — the only piece that wires at the composition root; the decode +
// crosswalk + normalize + reconnect logic is built and conformance-tested here
// against a fake Source.

// IDScheme is the symbology a vendor speaks; the Crosswalk maps it to/from the
// canonical instrument_id.
type IDScheme string

const (
	SchemeBloomberg IDScheme = "BLOOMBERG" // Bloomberg ticker, e.g. "AAPL US Equity"
	SchemeRIC       IDScheme = "RIC"       // Refinitiv Instrument Code, e.g. "AAPL.O"
	SchemeICE       IDScheme = "ICE"       // ICE Consolidated Feed symbol
)

// Crosswalk resolves vendor symbology to the canonical instrument_id and back —
// the bidirectional view the adapter needs: forward (incoming tick symbol →
// canonical id) and reverse (subscribe to a canonical instrument by its vendor
// symbol). The production impl is fed by the MASTER security master (PARITY-01e);
// StaticCrosswalk is the in-memory test/local default.
type Crosswalk interface {
	ToInstrument(scheme IDScheme, symbol string) (instrumentID string, ok bool)
	ToVendor(scheme IDScheme, instrumentID string) (symbol string, ok bool)
}

// StaticCrosswalk is an in-memory bidirectional crosswalk. Keyed per scheme.
type StaticCrosswalk struct {
	toInstrument map[IDScheme]map[string]string
	toVendor     map[IDScheme]map[string]string
}

// NewStaticCrosswalk builds an empty crosswalk.
func NewStaticCrosswalk() *StaticCrosswalk {
	return &StaticCrosswalk{
		toInstrument: map[IDScheme]map[string]string{},
		toVendor:     map[IDScheme]map[string]string{},
	}
}

// Add registers a symbol↔instrument mapping for a scheme.
func (c *StaticCrosswalk) Add(scheme IDScheme, symbol, instrumentID string) *StaticCrosswalk {
	if c.toInstrument[scheme] == nil {
		c.toInstrument[scheme] = map[string]string{}
		c.toVendor[scheme] = map[string]string{}
	}
	c.toInstrument[scheme][symbol] = instrumentID
	c.toVendor[scheme][instrumentID] = symbol
	return c
}

// ToInstrument resolves a vendor symbol to its canonical id.
func (c *StaticCrosswalk) ToInstrument(scheme IDScheme, symbol string) (string, bool) {
	id, ok := c.toInstrument[scheme][symbol]
	return id, ok
}

// ToVendor resolves a canonical id to its vendor symbol in scheme.
func (c *StaticCrosswalk) ToVendor(scheme IDScheme, instrumentID string) (string, bool) {
	s, ok := c.toVendor[scheme][instrumentID]
	return s, ok
}

// TickKind selects the market.v1 data variant a decoded tick maps to.
type TickKind int

const (
	KindTrade TickKind = iota
	KindQuote
	KindBar
)

// RawTick is the vendor-neutral decoded tick a Decoder produces — the canonical
// envelope + the decoded variant fields, addressed by the VendorSymbol (not yet
// resolved to a canonical id; the Driver does that via the Crosswalk). Prices are
// already common.v1.Decimal (the decoder bridges the vendor's native float via
// DecimalFromFloat at the vendor's known precision).
type RawTick struct {
	VendorSymbol   string
	MIC            string
	EventTime      time.Time
	SourceSequence uint64
	Kind           TickKind

	Price, Size *commonpb.Decimal // Trade
	TradeID     string

	BidPrice, BidSize, AskPrice, AskSize *commonpb.Decimal // Quote

	Open, High, Low, Close, Volume *commonpb.Decimal // Bar
	OpenTime                       time.Time
	TradeCount                     uint64
}

// errDisconnected signals a transient vendor disconnect (the native channel
// closed) — the Driver reconnects with backoff. ctx cancellation is a clean
// stop; a sink error is fatal.
var errDisconnected = errors.New("feed: vendor stream disconnected")

// Driver is the shared vendor-adapter engine, generic over the vendor's native
// tick type N. It implements feed.Adapter. The vendor binds `connect` (its
// Source's subscribe, returning a native-tick channel) and `decode` (native →
// RawTick); the Driver owns the reconnect loop, the symbology crosswalk, the
// shared normalizer, and the drop accounting.
type Driver[N any] struct {
	vendor    string
	scheme    IDScheme
	connect   func(ctx context.Context, vendorSymbols []string) (<-chan N, error)
	decode    func(N) (RawTick, error)
	xwalk     Crosswalk
	base, max time.Duration

	// OnDrop is called when a tick is dropped (unmapped symbol, decode failure,
	// or a tick that fails the canonical validation) — the hook the composition
	// root wires to a metric + the PARITY-01g data-quality exception. Default
	// no-op. Reason ∈ {"no_vendor_symbol","decode","unmapped","invalid"}.
	OnDrop func(reason, detail string)
}

// Vendor returns the source name.
func (d *Driver[N]) Vendor() string { return d.vendor }

// Run reverse-resolves the requested canonical instruments to vendor symbols,
// subscribes, and streams normalized events to sink until ctx is canceled,
// reconnecting (capped exponential backoff) on a transient disconnect. A sink
// error is fatal (returned); ctx cancellation returns nil.
func (d *Driver[N]) Run(ctx context.Context, instruments []string, sink Sink) error {
	symbols := make([]string, 0, len(instruments))
	for _, id := range instruments {
		if sym, ok := d.xwalk.ToVendor(d.scheme, id); ok {
			symbols = append(symbols, sym)
		} else {
			d.drop("no_vendor_symbol", id)
		}
	}

	delay := d.base
	for {
		if ctx.Err() != nil {
			return nil
		}
		ticks, err := d.connect(ctx, symbols)
		if err != nil {
			if !backoffSleep(ctx, &delay, d.base, d.max) {
				return nil
			}
			continue
		}
		delay = d.base // connected — reset backoff
		switch perr := d.pump(ctx, ticks, sink); {
		case perr == nil:
			return nil // ctx canceled — clean stop
		case errors.Is(perr, errDisconnected):
			if !backoffSleep(ctx, &delay, d.base, d.max) {
				return nil
			}
		default:
			return perr // sink failure — stop
		}
	}
}

// pump drains the native channel, handling each tick, until ctx cancellation
// (nil), the channel closing (errDisconnected), or a sink error.
func (d *Driver[N]) pump(ctx context.Context, ticks <-chan N, sink Sink) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case n, ok := <-ticks:
			if !ok {
				return errDisconnected
			}
			if err := d.handle(ctx, n, sink); err != nil {
				return err
			}
		}
	}
}

// handle decodes, crosswalks, normalizes, and publishes one native tick. A
// decode failure, an unmapped symbol, or a tick that fails canonical validation
// is dropped + counted (returns nil — bad data must not stop the stream); only a
// sink error propagates.
func (d *Driver[N]) handle(ctx context.Context, n N, sink Sink) error {
	raw, err := d.decode(n)
	if err != nil {
		d.drop("decode", err.Error())
		return nil
	}
	id, ok := d.xwalk.ToInstrument(d.scheme, raw.VendorSymbol)
	if !ok {
		d.drop("unmapped", raw.VendorSymbol)
		return nil
	}
	ev, err := buildEvent(id, raw)
	if err != nil {
		d.drop("invalid", err.Error())
		return nil
	}
	return sink.Publish(ctx, ev)
}

func (d *Driver[N]) drop(reason, detail string) {
	if d.OnDrop != nil {
		d.OnDrop(reason, detail)
	}
}

// buildEvent normalizes a resolved RawTick into a canonical market.v1 event via
// the shared validating builders — the one normalizer every vendor flows through.
func buildEvent(instrumentID string, r RawTick) (*marketpb.MarketDataEvent, error) {
	m := Meta{InstrumentID: instrumentID, Symbol: r.VendorSymbol, MIC: r.MIC, EventTime: r.EventTime, SourceSequence: r.SourceSequence}
	switch r.Kind {
	case KindTrade:
		return Trade(m, r.Price, r.Size, r.TradeID)
	case KindQuote:
		return Quote(m, r.BidPrice, r.BidSize, r.AskPrice, r.AskSize)
	case KindBar:
		return Bar(m, r.Open, r.High, r.Low, r.Close, r.Volume, r.OpenTime, r.TradeCount)
	default:
		return nil, ErrNoData
	}
}

// backoffSleep sleeps the current delay (ctx-aware), then doubles it toward max.
// Returns false if ctx was canceled during the sleep (caller stops).
func backoffSleep(ctx context.Context, delay *time.Duration, base, max time.Duration) bool {
	if *delay <= 0 {
		*delay = base
	}
	t := time.NewTimer(*delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	}
	if *delay < max {
		*delay *= 2
		if *delay > max {
			*delay = max
		}
	}
	return true
}

// defaultBase / defaultMax are the reconnect backoff bounds shared by the vendor
// adapters.
const (
	defaultBase = 200 * time.Millisecond
	defaultMax  = 30 * time.Second
	// priceExp is the fixed scale a vendor float price is bridged to Decimal at
	// (4 dp — equity/most-asset tick precision); a vendor delivering a decimal
	// string should parse it exactly instead of via float.
	priceExp = -4
)

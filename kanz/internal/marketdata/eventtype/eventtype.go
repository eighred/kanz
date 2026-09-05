// Package eventtype answers, for one envelope, WHICH market.v1.MarketDataEvent
// payload arm the event_type announces — and it answers before the payload is
// unmarshalled, because after that the question has no answer left.
//
// THE `market.` DOMAIN CARRIES MORE THAN ONE MESSAGE AND THE SHAPES OVERLAP.
// market.v1.OrderBookSnapshot, published on market.book.snapshot by
// internal/marketedge/ingest, is WIRE-COMPATIBLE with MarketDataEvent by
// construction: instrument_id, symbol, mic and event_time share fields 1-4,
// field 5 is a uint64 in both, `repeated PriceLevel bids` = 6 parses into the
// `Trade trade` = 6 oneof arm and `asks` = 7 into `Quote quote` = 7, with
// PriceLevel.price landing on Trade.price and on Quote.bid_price. Repeated
// fields merge last-wins, so a book snapshot decodes CLEANLY into a
// MarketDataEvent carrying the DEEPEST resting level — a plausible price,
// several ticks the wrong side of mid, with nothing on the decoded message
// saying it is not a print. proto.Unmarshal returns no error to act on.
//
// EVERY FOLD ON THE `market.>` WILDCARD NEEDS THIS SAME ANSWER, which is why it
// is one function rather than one `if` per fold. internal/marketdata/mark folds
// the spine into the live mark the OMS values a MARKET order against;
// internal/risk/pricing/livequote folds it into the calibration rate a discount
// curve is built from. The rule they share — a book snapshot is not a
// price-bearing event — held in two places is two chances to disagree, and the
// second copy is where a fix stops spreading.
package eventtype

import "strings"

// Variant is the market.v1.MarketDataEvent oneof arm an event_type announces.
//
// None is the FAIL-CLOSED answer, and it covers two cases a fold must treat
// identically: the event_type names no MarketDataEvent at all
// (market.book.snapshot, market.crypto.ingestion_coverage), and it names a
// variant token this platform does not publish. A fold that switches on this
// refuses an event_type it does not recognise instead of decoding it hopefully,
// which is the only order that works — by the time the bytes are decoded a
// book snapshot and a genuine print are indistinguishable.
type Variant uint8

const (
	// None means the payload must not be unmarshalled as a MarketDataEvent.
	None Variant = iota
	// Trade is `market.<assetClass>.trade`, carrying an executed print.
	Trade
	// Quote is `market.<assetClass>.quote`, carrying top-of-book bid/ask.
	Quote
	// Bar is `market.<assetClass>.bar`, carrying an OHLCV aggregate.
	Bar
)

// domain is the first event_type token every MarketDataEvent shares.
const domain = "market."

// Of classifies eventType against the taxonomy the producer stamps.
//
// services/market-data/internal/feed/bussink.go emits every MarketDataEvent as
// `market.<assetClass>.<variant>`, where the variant token is chosen from the
// oneof arm actually set — so the token set here is that oneof's, and
// TestEveryOneofArmHasAVariant derives it from the descriptor rather than
// trusting this switch to have been updated. Anything that is not exactly three
// tokens with `market` first and a known variant last is None: that is how
// market.book.snapshot (`snapshot` is no arm) and market.crypto.volume_profile
// are refused without being decoded.
//
// It runs once per market tick on both folds, so it walks the string in place —
// no Split, no map, no allocation. The substrings share eventType's backing
// array and the comparisons are length-checked before any byte is read.
func Of(eventType string) Variant {
	if !strings.HasPrefix(eventType, domain) {
		return None
	}
	rest := eventType[len(domain):]
	i := strings.IndexByte(rest, '.')
	if i < 0 {
		return None
	}
	// A fourth token means this is not the shape bussink stamps, whatever the
	// third one happens to say.
	variant := rest[i+1:]
	if strings.IndexByte(variant, '.') >= 0 {
		return None
	}
	switch variant {
	case "trade":
		return Trade
	case "quote":
		return Quote
	case "bar":
		return Bar
	default:
		return None
	}
}

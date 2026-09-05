package livequote

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// bookSnapshotBytes marshals a market.v1.OrderBookSnapshot — the message
// market.book.snapshot actually carries, and the one the risk engine's
// `market.>` subscription delivers here alongside the quotes it wants.
//
// OrderBookSnapshot is WIRE-COMPATIBLE with MarketDataEvent by construction:
// fields 1-4 (instrument_id, symbol, mic, event_time) share names and types,
// field 5 is a uint64 in both (last_update_sequence vs source_sequence),
// `repeated PriceLevel bids` = 6 parses into the `Trade trade` = 6 oneof arm
// and `asks` = 7 into `Quote quote` = 7, with PriceLevel.price landing on
// Quote.bid_price. Repeated fields merge and the last value wins, and a
// two-sided book writes field 7 after field 6 — so the decoded event is a
// Quote holding the DEEPEST ASK level as its bid, with no ask at all.
func bookSnapshotBytes(t *testing.T, instrument string, bids, asks []*marketpb.PriceLevel) []byte {
	t.Helper()
	b, err := proto.Marshal(&marketpb.OrderBookSnapshot{
		InstrumentId: instrument,
		Mic:          "BINANCE",
		EventTime:    timestamppb.New(time.Unix(1_700_000_000, 0)),
		Bids:         bids,
		Asks:         asks,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func level(coeff int64, exp int32) *marketpb.PriceLevel {
	return &marketpb.PriceLevel{Price: decv(coeff, exp), Size: decv(1, 0)}
}

// TestBookSnapshotYieldsNoCalibrationRate is the poisoning case, closed (#1021).
//
// The strip's own instrument is quoted at 4.4/4.6, and the book behind it is
// five levels deep to 5.2. Before the event-type refusal this fold cached the
// snapshot as a Quote and the curve was calibrated at 5.2% — not a wrong number
// that looks wrong, a plausible one whose error is the depth of the book.
func TestBookSnapshotYieldsNoCalibrationRate(t *testing.T) {
	instruments := []RateInstrument{
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
	}
	payload := bookSnapshotBytes(t, "USD-DEP-3M",
		[]*marketpb.PriceLevel{level(44, -3), level(43, -3)},
		[]*marketpb.PriceLevel{level(46, -3), level(52, -3)}) // deepest ask, 5.2%

	// NON-VACUITY, AND IT IS THE POINT OF THE TEST. If OrderBookSnapshot ever
	// stops decoding into a rate-bearing MarketDataEvent, the assertions below
	// would pass for a reason that has nothing to do with the guard, and the
	// guard could be deleted without a single test going red. So the decode is
	// asserted first: these bytes DO yield 0.052, the deepest ask, as a rate.
	var decoded marketpb.MarketDataEvent
	if err := proto.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("an OrderBookSnapshot no longer decodes as a MarketDataEvent (%v) — the wire "+
			"compatibility this guard exists for is gone, and the guard's premise needs re-reading", err)
	}
	rate, ok := midPrice(&decoded)
	if !ok || !approx(rate, 0.052) {
		t.Fatalf("the decoded book snapshot yields midPrice = (%v, %v), want (0.052, true) — the "+
			"deepest ask level read as a bid. Without that this test asserts nothing about the guard",
			rate, ok)
	}

	q := New(instruments)
	env := &envelopepb.Envelope{EventType: "market.book.snapshot"}
	if err := q.Handler(context.Background(), env, payload); err != nil {
		t.Fatalf("Handler: %v — a book snapshot rides the same `market.>` wildcard as the quotes "+
			"this fold wants, so it must be acked, never nacked into the market spine's DLQ", err)
	}
	if _, ok := q.Latest("USD-DEP-3M"); ok {
		t.Fatalf("a market.book.snapshot payload was cached as the latest quote for USD-DEP-3M. "+
			"OrderBookSnapshot is wire-compatible with MarketDataEvent, so this is a Quote whose "+
			"bid_price is the DEEPEST ASK (%v) — the curve would calibrate off the far end of the book", rate)
	}

	// The reader's own answer, not just the cache's: a strip with no quote must
	// report the instrument MISSING rather than carry a rate nobody quoted.
	src := NewSnapshotRateSource(q, instruments)
	strip, err := src.RateQuotes(context.Background(), "USD", time.Now())
	if err != nil {
		t.Fatalf("RateQuotes: %v", err)
	}
	if len(strip.Quotes) != 0 {
		t.Fatalf("the strip carries %+v after only a book snapshot arrived — a calibration rate "+
			"derived from resting depth is not a rate this platform observed", strip.Quotes)
	}
	if len(strip.Coverage.Missing) != 1 || strip.Coverage.Missing[0].Reason != curve.MissingNoQuote {
		t.Fatalf("Coverage.Missing = %+v, want the one configured instrument reported as never "+
			"quoted — the refusal must leave the strip visibly short, not silently complete",
			strip.Coverage.Missing)
	}
}

// THE REFUSAL MUST NOT TAKE THE BAR PATH WITH IT. livequote admits one more
// variant than the mark fold does — midPrice reads a Bar's close — so a
// classifier narrowed to the mark fold's two variants would silently stop
// folding every market.<assetClass>.bar the spine carries, and a calibration
// instrument priced only by candles would go permanently unquoted with nothing
// anywhere saying why.
func TestGenuineBarStillFoldsIntoTheStrip(t *testing.T) {
	instruments := []RateInstrument{
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5},
	}
	q := New(instruments)
	ev := &marketpb.MarketDataEvent{
		InstrumentId: "USD-SWAP-5Y",
		Data: &marketpb.MarketDataEvent_Bar{Bar: &marketpb.Bar{
			Open: decv(48, -3), High: decv(51, -3), Low: decv(47, -3), Close: decv(50, -3),
		}},
	}
	payload, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env := &envelopepb.Envelope{EventType: "market.rate.bar"}
	if err := q.Handler(context.Background(), env, payload); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	strip, err := NewSnapshotRateSource(q, instruments).RateQuotes(context.Background(), "USD", time.Now())
	if err != nil {
		t.Fatalf("RateQuotes: %v", err)
	}
	if len(strip.Quotes) != 1 || !approx(strip.Quotes[0].Value, 0.050) {
		t.Fatalf("the strip is %+v after a genuine market.rate.bar — the candle close is a "+
			"calibration observation this fold reads, and the event-type refusal must not eat it",
			strip.Quotes)
	}
}

// A refused delivery must not be counted as observed either: the cache holds
// nothing at all, so the composition root's "what has ticked" reading does not
// improve because a book snapshot arrived.
func TestBookSnapshotLeavesTheCacheEmpty(t *testing.T) {
	q := New([]RateInstrument{{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25}})
	payload := bookSnapshotBytes(t, "USD-DEP-3M",
		[]*marketpb.PriceLevel{level(44, -3)}, []*marketpb.PriceLevel{level(46, -3)})
	if err := q.Handler(context.Background(), &envelopepb.Envelope{EventType: "market.book.snapshot"}, payload); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if got := len(q.latest); got != 0 {
		t.Fatalf("the cache holds %d entry after a book snapshot — the refusal happens before "+
			"Update, so nothing may be recorded", got)
	}
}

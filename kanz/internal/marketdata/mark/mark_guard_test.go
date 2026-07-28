package mark_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/marketdata/mark"
)

// bookSnapshotBytes marshals a market.v1.OrderBookSnapshot — the message
// market.book.snapshot actually carries. OrderBookSnapshot is
// WIRE-COMPATIBLE with MarketDataEvent by construction: fields 1-4
// (instrument_id, symbol, mic, event_time) share names and types, field 5 is
// a uint64 in both (last_update_sequence vs source_sequence), and field 6
// (repeated PriceLevel bids) parses into MarketDataEvent's field-6 oneof
// (Trade trade) — PriceLevel.price and Trade.price are both field-1
// common.v1.Decimal. So a bids-only snapshot decodes as a MarketDataEvent
// carrying a Trade at the DEEPEST bid level (repeated fields merge, last
// value wins), and Handle would record that resting bid as the mark.
func bookSnapshotBytes(t *testing.T, instrument string, bids []*marketpb.PriceLevel, at time.Time) []byte {
	t.Helper()
	b, err := proto.Marshal(&marketpb.OrderBookSnapshot{
		InstrumentId: instrument,
		EventTime:    timestamppb.New(at),
		Bids:         bids,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func level(price int64, exp int32) *marketpb.PriceLevel {
	return &marketpb.PriceLevel{
		Price: &commonpb.Decimal{Coefficient: price, Exponent: exp},
		Size:  &commonpb.Decimal{Coefficient: 1, Exponent: 0},
	}
}

// TestBidsOnlyBookSnapshotRecordsNoMark is the poisoning case, closed. A
// bids-only OrderBookSnapshot on market.book.snapshot must never become a
// mark — before Fix 2 (the EventType guard in Handle) this recorded
// 42000.00, the deepest resting bid, as the live mark for BTC-USD.
func TestBidsOnlyBookSnapshotRecordsNoMark(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	payload := bookSnapshotBytes(t, "BTC-USD", []*marketpb.PriceLevel{
		level(4210000, -2), // best bid, 42100.00
		level(4200000, -2), // deepest bid, 42000.00 — repeated fields merge, last wins
	}, base)
	if err := s.Handle(context.Background(), env("market.book.snapshot"), payload); err != nil {
		t.Fatalf("Handle: %v — a book snapshot must be acked, never wedge the partition", err)
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — a market.book.snapshot payload must never become a mark "+
			"(OrderBookSnapshot is wire-compatible with MarketDataEvent; this would be the deepest resting bid)", got)
	}
}

// TestGenuineTradeStillRecordsMarkGivenItsEventType is one half of the
// guard's non-vacuity check: a real market.crypto.trade must still set the
// mark. If the guard were too strict — rejecting everything, not just
// book.snapshot — this would fail alongside the poisoning test passing.
func TestGenuineTradeStillRecordsMarkGivenItsEventType(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(4210050, -2), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := s.Mark("BTC-USD")
	if got == nil || got.Cmp(big.NewRat(4210050, 100)) != 0 {
		t.Fatalf("Mark = %v, want 42100.50", got)
	}
}

// TestGenuineQuoteStillRecordsMidGivenItsEventType is the other half of the
// non-vacuity check, for the quote path and a different asset class token —
// the guard must accept ANY market.<assetClass>.quote, not just crypto.
func TestGenuineQuoteStillRecordsMidGivenItsEventType(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.equity.quote"), quoteEvent(t, "AAPL", dec(100, 0), dec(102, 0), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := s.Mark("AAPL")
	if got == nil || got.Cmp(big.NewRat(101, 1)) != 0 {
		t.Fatalf("Mark = %v, want 101 (mid of 100/102)", got)
	}
}

// TestUnexpectedEventTypeIsRejectedBeforeUnmarshal covers a shape the guard
// must reject that isn't the headline book.snapshot case: an event type with
// no recognizable variant at all. It must be acked, not nacked — same
// contract as a malformed payload.
func TestUnexpectedEventTypeIsRejectedBeforeUnmarshal(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.bar"), tradeEvent(t, "BTC-USD", dec(100, 0), base)); err != nil {
		t.Fatalf("Handle: %v — an unrecognized event type must be acked, never wedge the partition", err)
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil for EventType market.crypto.bar", got)
	}
}

package mark_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/marketdata/mark"
)

var base = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

func dec(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

// tradeEvent builds a MarketDataEvent carrying a trade at price, stamped at at.
func tradeEvent(t *testing.T, instrument string, price *commonpb.Decimal, at time.Time) []byte {
	t.Helper()
	b, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    timestamppb.New(at),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: price}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// quoteEvent builds a MarketDataEvent carrying a two-sided quote.
func quoteEvent(t *testing.T, instrument string, bid, ask *commonpb.Decimal, at time.Time) []byte {
	t.Helper()
	b, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    timestamppb.New(at),
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
			BidPrice: bid, AskPrice: ask,
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func env() *envelopepb.Envelope { return &envelopepb.Envelope{EventTime: timestamppb.New(base)} }

func TestTradeSetsTheMark(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env(), tradeEvent(t, "BTC-USD", dec(4210050, -2), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := s.Mark("BTC-USD")
	if got == nil || got.Cmp(big.NewRat(4210050, 100)) != 0 {
		t.Fatalf("Mark = %v, want 42100.50", got)
	}
}

func TestQuoteSetsTheMid(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env(), quoteEvent(t, "BTC-USD", dec(100, 0), dec(102, 0), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := s.Mark("BTC-USD")
	if got == nil || got.Cmp(big.NewRat(101, 1)) != 0 {
		t.Fatalf("Mark = %v, want 101 (mid of 100/102)", got)
	}
}

func TestUnknownInstrumentIsNil(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if got := s.Mark("NOPE"); got != nil {
		t.Fatalf("Mark = %v, want nil for an instrument never seen", got)
	}
}

func TestAMarkOlderThanMaxAgeIsNil(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second)
	// Stamped at base, read 31s later.
	if err := s.Handle(context.Background(), env(), tradeEvent(t, "BTC-USD", dec(100, 0), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	now = base.Add(31 * time.Second)
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — the mark is 31s old under a 30s bound", got)
	}
	// NON-VACUITY: a fresher mark under the same bound must survive, or this test
	// would pass against a Mark that always returned nil.
	now = base.Add(29 * time.Second)
	if got := s.Mark("BTC-USD"); got == nil {
		t.Fatal("Mark = nil at 29s under a 30s bound — the bound is refusing everything")
	}
}

func TestZeroMaxAgeNeverExpires(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 0)
	if err := s.Handle(context.Background(), env(), tradeEvent(t, "BTC-USD", dec(100, 0), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	now = base.Add(72 * time.Hour)
	if got := s.Mark("BTC-USD"); got == nil {
		t.Fatal("Mark = nil after 72h with maxAge 0 — 0 must mean 'never expires' (tv-sync relies on it)")
	}
}

func TestLookupSeparatesNeverSeenFromExpired(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second)
	if err := s.Handle(context.Background(), env(), tradeEvent(t, "BTC-USD", dec(100, 0), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	now = base.Add(time.Hour)

	if _, _, seen := s.Lookup("NEVER"); seen {
		t.Fatal("Lookup reported an instrument we never saw as seen")
	}
	price, asOf, seen := s.Lookup("BTC-USD")
	if !seen {
		t.Fatal("Lookup reported an expired-but-seen instrument as never seen — " +
			"a stalled feed would be indistinguishable from a cold map")
	}
	if price == nil || !asOf.Equal(base) {
		t.Fatalf("Lookup = (%v, %v), want the stored price at its original event time", price, asOf)
	}
	// And Mark still refuses it: Lookup is diagnostic, Mark is the safe accessor.
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — Lookup must not soften Mark", got)
	}
}

func TestMalformedPayloadIsAckedAndChangesNothing(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env(), []byte("not a protobuf")); err != nil {
		t.Fatalf("Handle returned %v — a malformed event must be acked, never wedge the partition", err)
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil", got)
	}
}

func TestNonPositivePriceIsIgnored(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env(), tradeEvent(t, "BTC-USD", dec(0, 0), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — a zero price must never become a mark", got)
	}
}

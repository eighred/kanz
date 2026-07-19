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

// tradeEventNoTime builds a MarketDataEvent carrying a trade at price with no EventTime set.
func tradeEventNoTime(t *testing.T, instrument string, price *commonpb.Decimal) []byte {
	t.Helper()
	b, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    nil,
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

// env builds an envelope carrying eventType — the field Handle's guard
// (isMarkBearingEventType in mark.go) checks before it trusts the payload
// bytes enough to unmarshal them as a MarketDataEvent. Every call site below
// names a real market.<assetClass>.trade or .quote variant, mirroring what
// bussink.go actually stamps on the wire
// (services/market-data/internal/feed/bussink.go) — an envelope with no
// EventType at all does not occur in production, so it is not a fixture worth
// preserving now that Handle looks at it.
func env(eventType string) *envelopepb.Envelope {
	return &envelopepb.Envelope{EventTime: timestamppb.New(base), EventType: eventType}
}

func TestTradeSetsTheMark(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(4210050, -2), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := s.Mark("BTC-USD")
	if got == nil || got.Cmp(big.NewRat(4210050, 100)) != 0 {
		t.Fatalf("Mark = %v, want 42100.50", got)
	}
}

func TestQuoteSetsTheMid(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.quote"), quoteEvent(t, "BTC-USD", dec(100, 0), dec(102, 0), base)); err != nil {
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
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(100, 0), base)); err != nil {
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
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(100, 0), base)); err != nil {
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
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(100, 0), base)); err != nil {
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
	if err := s.Handle(context.Background(), env("market.crypto.trade"), []byte("not a protobuf")); err != nil {
		t.Fatalf("Handle returned %v — a malformed event must be acked, never wedge the partition", err)
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil", got)
	}
}

func TestNonPositivePriceIsIgnored(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(0, 0), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — a zero price must never become a mark", got)
	}
}

// TestOutOfDomainTradePriceIsIgnored (Finding 1): dec.FromProto materialises
// 10^abs(exponent) with no bound. A MarketDataEvent trade price off the
// wire at an absurd exponent would hang the fold. Handle must ignore it
// exactly as a malformed or non-positive price already is — one more
// unusable case, not a new failure mode — and must never fabricate zero as
// a fallback price.
func TestOutOfDomainTradePriceIsIgnored(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(1, 2000000000), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — an out-of-domain price must never become a mark", got)
	}
}

// TestOutOfDomainQuotePriceIsIgnored covers the second FromProto call site:
// a quote's bid/ask mid.
func TestOutOfDomainQuotePriceIsIgnored(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.quote"), quoteEvent(t, "BTC-USD", dec(1, 2000000000), dec(100, 0), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — an out-of-domain bid must never become a mark", got)
	}
}

// TestOutOfDomainPriceLeavesThePreviousMarkUntouched: a bad event must be
// ignored outright, not fabricate a value or otherwise disturb an existing
// mark — a zero-valued mark is never a safe substitute anywhere on this
// platform.
func TestOutOfDomainPriceLeavesThePreviousMarkUntouched(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(4210050, -2), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(1, 2000000000), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := s.Mark("BTC-USD")
	if got == nil || got.Cmp(big.NewRat(4210050, 100)) != 0 {
		t.Fatalf("Mark = %v, want 42100.50 unchanged — the out-of-domain event must not touch the previous mark", got)
	}
}

func TestEnvelopeTimeFallbackWhenEventTimeIsNil(t *testing.T) {
	envelopeTime := base
	now := envelopeTime
	s := mark.New(func() time.Time { return now }, 30*time.Second)

	// Create an envelope with a specific time.
	e := &envelopepb.Envelope{EventTime: timestamppb.New(envelopeTime), EventType: "market.crypto.trade"}

	// Handle a trade event with no EventTime set (event's own time is nil).
	// The mark should use the envelope's time as its asOf timestamp.
	if err := s.Handle(context.Background(), e, tradeEventNoTime(t, "BTC-USD", dec(100, 0))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Verify the mark was stored with the envelope's time, not skipped.
	price, asOf, seen := s.Lookup("BTC-USD")
	if !seen {
		t.Fatal("Lookup: instrument not seen — fallback failed to store the mark")
	}
	if !asOf.Equal(envelopeTime) {
		t.Fatalf("Lookup asOf = %v, want envelope time %v", asOf, envelopeTime)
	}
	if price == nil || price.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("Lookup price = %v, want 100", price)
	}

	// Verify the fallback time is enforced by staleness: advance time past maxAge.
	now = envelopeTime.Add(31 * time.Second)
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — mark is 31s old under 30s bound", got)
	}
	// Confirm staleness is measured from the envelope time, not from now.
	if _, asOf, seen := s.Lookup("BTC-USD"); !seen || !asOf.Equal(envelopeTime) {
		t.Fatal("Lookup after staleness: must still report the original envelope time")
	}
}

func TestNegativePriceIsIgnored(t *testing.T) {
	s := mark.New(func() time.Time { return base }, time.Minute)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(-100, 0), base)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — a negative price must never become a mark", got)
	}
}

// TestAFutureDatedMarkStillExpires (finding 1): staleness was computed as
// now - asOf > maxAge, so an asOf AHEAD of the local clock gave a NEGATIVE age
// that can never exceed the bound — the mark was permanently fresh. A venue or
// gateway with a skewed clock could therefore pin a price into the OMS gate
// forever, and MARKET orders would be valued off it long after the feed died.
func TestAFutureDatedMarkStillExpires(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second)
	// A producer 24h ahead of us.
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(100, 0), base.Add(24*time.Hour))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := s.Mark("BTC-USD"); got == nil {
		t.Fatal("Mark = nil immediately after the fold — a skewed mark must still be USABLE, just not immortal")
	}
	now = base.Add(31 * time.Second)
	if got := s.Mark("BTC-USD"); got != nil {
		t.Fatalf("Mark = %v, want nil — 31s of local time passed with no new tick under a 30s bound; "+
			"a future-dated asOf must not make the mark permanently fresh", got)
	}
}

// TestSmallForwardSkewIsTolerated: honest hosts disagree by milliseconds to a
// couple of seconds. A mark a hair ahead of us must keep its own event time and
// age from it, not be treated as an incident.
func TestSmallForwardSkewIsTolerated(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(100, 0), base.Add(time.Second))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_, asOf, seen := s.Lookup("BTC-USD")
	if !seen {
		t.Fatal("Lookup: not seen")
	}
	if !asOf.Equal(base.Add(time.Second)) {
		t.Fatalf("asOf = %v, want %v — a 1s skew is within tolerance and the venue timestamp must be kept as-is",
			asOf, base.Add(time.Second))
	}
	now = base.Add(20 * time.Second)
	if got := s.Mark("BTC-USD"); got == nil {
		t.Fatal("Mark = nil 20s in under a 30s bound — a small forward skew must not shorten the mark's life")
	}
}

// TestLargeForwardSkewIsClampedToNow: beyond tolerance we stop trusting the
// producer's clock and age the mark from OUR receive time.
func TestLargeForwardSkewIsClampedToNow(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 30*time.Second)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(100, 0), base.Add(time.Hour))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_, asOf, seen := s.Lookup("BTC-USD")
	if !seen {
		t.Fatal("Lookup: not seen")
	}
	if !asOf.Equal(base) {
		t.Fatalf("asOf = %v, want %v — an asOf an hour ahead of the local clock must be clamped to now", asOf, base)
	}
}

// TestZeroMaxAgeStillNeverExpiresWithASkewedMark: tv-sync passes maxAge=0 and
// depends on marks never expiring. Clamping forward skew must not change that.
func TestZeroMaxAgeStillNeverExpiresWithASkewedMark(t *testing.T) {
	now := base
	s := mark.New(func() time.Time { return now }, 0)
	if err := s.Handle(context.Background(), env("market.crypto.trade"), tradeEvent(t, "BTC-USD", dec(100, 0), base.Add(24*time.Hour))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	now = base.Add(365 * 24 * time.Hour)
	if got := s.Mark("BTC-USD"); got == nil {
		t.Fatal("Mark = nil under maxAge=0 — marks must NEVER expire when no bound is set")
	}
}

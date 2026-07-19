// Package mark folds the market price spine into a live last-mark source.
//
// It is the price side of the market-data domain: internal/marketdata/ingest
// writes observations to a store for analytics, while this holds only the
// LATEST mark per instrument, in memory, for callers that need to value
// something right now — tv-sync's unrealized P&L, and the OMS pre-trade gate
// valuing a MARKET order at admission.
//
// Lifted from services/tv-sync/internal/markfeed, which could not be shared:
// Go's internal rule makes a package under services/tv-sync/internal reachable
// only from tv-sync, so the OMS could not have reused it where it sat.
//
// STALENESS IS THE PART THAT IS NEW, and it is why this is not just a move. A
// mark with no expiry is fine for a P&L display and wrong for an admission
// gate: valuing an order against a price from a feed that died an hour ago is
// exactly the kind of silent, confident wrongness the pre-trade gate exists to
// prevent. Age is measured from the EVENT's own timestamp, not our receive
// time — that is what is true about the quote rather than about our plumbing,
// and replayed events cannot poison it because the live-mode validator
// hard-rejects QUALITY_FLAG_REPLAYED before dispatch (pkg/bus/consumer.go).
// A FUTURE-DATED event time still can, though — see clampSkew.
package mark

import (
	"context"
	"math/big"
	"sync"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/internal/dec"
)

// maxForwardSkew is how far AHEAD of our own clock a producer's timestamp may
// sit and still be taken at face value. Minor disagreement between honest hosts
// is normal, so some tolerance is required or ordinary NTP jitter would look
// like an incident.
const maxForwardSkew = 5 * time.Second

type entry struct {
	price *big.Rat
	asOf  time.Time
}

// Source holds the latest mark per instrument, folded from the price spine.
// Safe for concurrent folds (Handle) and reads (Mark, Lookup).
type Source struct {
	mu     sync.RWMutex
	prices map[string]entry
	now    func() time.Time
	maxAge time.Duration
}

// New returns an empty mark source.
//
// maxAge == 0 means marks NEVER expire. That is not a default so much as a
// deliberate choice a caller has to make out loud: tv-sync passes 0 because a
// P&L display degrades gracefully on a stale mark, while the OMS passes a real
// bound because an admission decision does not.
func New(now func() time.Time, maxAge time.Duration) *Source {
	if now == nil {
		now = time.Now
	}
	return &Source{prices: make(map[string]entry), now: now, maxAge: maxAge}
}

// Mark returns the latest non-expired mark for an instrument, or nil when none
// is known or the newest one is older than maxAge. nil is the caller's signal
// to degrade — the projection omits unrealized P&L, the gate refuses the order.
// It never fabricates.
func (s *Source) Mark(instrument string) *big.Rat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.prices[instrument]
	if !ok || s.expired(e) {
		return nil
	}
	return new(big.Rat).Set(e.price)
}

// Lookup returns the stored entry REGARDLESS of expiry, so a caller can tell
// "never seen" (seen == false) from "seen but expired" (seen == true, asOf
// old). Those are different incidents — a cold or thin instrument versus a
// feed that stalled — and a single refusal count cannot distinguish them.
//
// This is the DIAGNOSTIC accessor. Mark is the safe one. Nothing on a decision
// path may call Lookup, or the expiry bound is one `if` away from being lost.
func (s *Source) Lookup(instrument string) (price *big.Rat, asOf time.Time, seen bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.prices[instrument]
	if !ok {
		return nil, time.Time{}, false
	}
	return new(big.Rat).Set(e.price), e.asOf, true
}

func (s *Source) expired(e entry) bool {
	if s.maxAge <= 0 {
		return false
	}
	return s.now().Sub(e.asOf) > s.maxAge
}

// Handle is the bus.EventHandler: it folds one MarketDataEvent's price. A Trade
// updates the mark to the last trade price; a Quote to the bid/ask mid. Other
// payloads (Bar) are ignored. Malformed events, non-positive prices, and prices
// with an out-of-domain exponent (dec.FromProtoChecked) are all acked and
// folded as no-ops — a price monitor never wedges the partition, and it never
// fabricates a price it could not safely compute.
func (s *Source) Handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	var ev marketpb.MarketDataEvent
	if proto.Unmarshal(payload, &ev) != nil || ev.GetInstrumentId() == "" {
		return nil
	}
	var price *big.Rat
	switch {
	case ev.GetTrade() != nil && ev.GetTrade().GetPrice() != nil:
		p, ok := dec.FromProtoChecked(ev.GetTrade().GetPrice())
		if !ok {
			return nil // out-of-domain exponent: unusable, same as malformed input — never fabricate a price
		}
		price = p
	case ev.GetQuote() != nil && ev.GetQuote().GetBidPrice() != nil && ev.GetQuote().GetAskPrice() != nil:
		bid, ok := dec.FromProtoChecked(ev.GetQuote().GetBidPrice())
		if !ok {
			return nil
		}
		ask, ok := dec.FromProtoChecked(ev.GetQuote().GetAskPrice())
		if !ok {
			return nil
		}
		mid := new(big.Rat).Add(bid, ask)
		price = mid.Quo(mid, big.NewRat(2, 1))
	default:
		return nil
	}
	if price == nil || price.Sign() <= 0 {
		return nil
	}
	s.mu.Lock()
	s.prices[ev.GetInstrumentId()] = entry{price: price, asOf: s.clampSkew(eventTime(env, &ev))}
	s.mu.Unlock()
	return nil
}

// clampSkew refuses to trust an asOf that is AHEAD of our own clock by more
// than maxForwardSkew, and ages the mark from receive time instead.
//
// Without this, staleness (now - asOf > maxAge) is UNREACHABLE for a
// future-dated mark: the age is negative, so it can never exceed the bound and
// the mark is fresh forever. Age is taken from the event's own timestamp
// (that is what is true about the quote rather than about our plumbing) and
// nothing upstream bounds it — pkg/bus/validate.go only checks that the
// envelope's event_time PARSES, and the value actually preferred here is the
// payload's MarketDataEvent.event_time, which is not validated at all. Replay
// is hard-rejected on the live path, but replay is not the only way event-time
// goes wrong: A SKEWED VENUE OR GATEWAY CLOCK IS. That is the one route by
// which the OMS pre-trade gate can value a MARKET order off a price from a feed
// that died hours ago, which is exactly what the staleness bound exists to stop.
//
// CLAMPING, NOT REFUSING. Both close the hole. Refusing a skewed mark discards
// the only price we have for that instrument, so a single misconfigured
// producer turns every MARKET order on it into PRICE_UNAVAILABLE — a trading
// outage caused by a clock. Clamping keeps the mark usable and merely makes it
// age normally from when we received it, which is the weaker but still honest
// statement "we knew this price at least by now". On a trading path an operator
// would rather have a slightly conservatively-aged price than no price.
func (s *Source) clampSkew(asOf time.Time) time.Time {
	now := s.now()
	if asOf.After(now.Add(maxForwardSkew)) {
		return now
	}
	return asOf
}

// eventTime prefers the MarketDataEvent's own event_time — it is the venue
// timestamp for THIS event and stays correct inside a batch, where one envelope
// carries many events with different times. The envelope is the fallback.
func eventTime(env *envelopepb.Envelope, ev *marketpb.MarketDataEvent) time.Time {
	if ts := ev.GetEventTime(); ts != nil {
		return ts.AsTime()
	}
	return env.GetEventTime().AsTime()
}

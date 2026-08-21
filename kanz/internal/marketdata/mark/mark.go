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
	"strings"
	"sync"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"

	decutil "github.com/eighred/kanz/internal/dec"
)

// maxForwardSkew is how far AHEAD of our own clock a producer's timestamp may
// sit and still be taken at face value. Minor disagreement between honest hosts
// is normal, so some tolerance is required or ordinary NTP jitter would look
// like an incident.
const maxForwardSkew = 5 * time.Second

// sweepInterval bounds how often Handle walks the map to tombstone expired
// entries. The sweep is O(instruments) and runs under the write lock Handle
// already holds, so it is amortised across folds rather than paid per tick: a
// price spine delivering thousands of ticks a second must not walk the whole map
// on each one. Memory is therefore held for at most maxAge + sweepInterval,
// which is the bound this buys.
const sweepInterval = 30 * time.Second

// entry is the latest mark for one instrument.
//
// price == nil is a TOMBSTONE: the mark expired, its value was released, and the
// fact that this instrument was once seen was deliberately kept (#96).
//
// That distinction is not bookkeeping. Lookup exists so a caller can tell "never
// seen" from "seen but expired" — a cold or thin instrument versus a feed that
// stalled — and the OMS branches on exactly that to emit two different metrics
// and two different operator messages (services/oms/cmd/oms/main.go). Deleting
// the entry outright would have bounded memory by silently reporting every
// stalled feed as a cold instrument, converting an outage signal into a warm-up
// signal. Releasing the *big.Rat while keeping asOf bounds the heavy part and
// keeps the diagnosis.
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
	// lastSweep is when the expired-entry sweep last ran. Guarded by mu.
	lastSweep time.Time
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
	if !ok || e.price == nil || s.expired(e) {
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
// A TOMBSTONED entry returns seen == true with a real asOf and a NIL price: the
// mark expired and its value was released (#96). That is the correct answer for
// every caller of this accessor — it is the diagnostic one, and an expired price
// is not a price anything may act on. Callers that need a usable value must use
// Mark, which is the safe accessor and already refuses expired marks.
func (s *Source) Lookup(instrument string) (price *big.Rat, asOf time.Time, seen bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.prices[instrument]
	if !ok {
		return nil, time.Time{}, false
	}
	if e.price == nil {
		return nil, e.asOf, true
	}
	return new(big.Rat).Set(e.price), e.asOf, true
}

// Stats reports how many instruments this fold is holding and how many of those
// still carry a usable price.
//
// held is the map's cardinality — every instrument ever folded, including
// tombstones. live is the subset with a price that has not expired. The gap
// between them is the tombstone population.
//
// It exists because "the fold's volume is unbounded" (#96) was an assertion
// nobody could check: nothing reported how many instruments were being held, so
// the choice between accepting the growth and bounding it by an instrument
// allowlist — which would refuse live orders on a config omission — had no
// measurement behind it. A gauge is smaller than either answer.
func (s *Source) Stats() (held, live int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.prices {
		if e.price != nil && !s.expired(e) {
			live++
		}
	}
	return len(s.prices), live
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
// with an out-of-domain exponent (decutil.FromProtoChecked) are all acked and
// folded as no-ops — a price monitor never wedges the partition, and it never
// fabricates a price it could not safely compute.
//
// The envelope's EventType is checked BEFORE the payload is unmarshalled at
// all (isMarkBearingEventType). This is not redundant with the price checks
// below: market.v1.OrderBookSnapshot — published on market.book.snapshot — is
// WIRE-COMPATIBLE with MarketDataEvent by construction (see the package doc
// and mark_guard_test.go), so a one-sided, bids-only book snapshot unmarshals
// cleanly into a "Trade" at the deepest resting bid price and would otherwise
// poison the mark below mid. Two callers folding this Source (the OMS and
// tv-sync) each narrow their own subscription to exclude market.book.snapshot,
// but that is two configs that can drift apart — this check is the one place
// that cannot drift, because it does not depend on which subjects a caller
// chose to subscribe to.
func (s *Source) Handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	if !isMarkBearingEventType(env.GetEventType()) {
		return nil
	}
	var ev marketpb.MarketDataEvent
	if proto.Unmarshal(payload, &ev) != nil || ev.GetInstrumentId() == "" {
		return nil
	}
	var price *big.Rat
	switch {
	case ev.GetTrade() != nil && ev.GetTrade().GetPrice() != nil:
		p, ok := decutil.FromProtoChecked(ev.GetTrade().GetPrice())
		if !ok {
			return nil // out-of-domain exponent: unusable, same as malformed input — never fabricate a price
		}
		price = p
	case ev.GetQuote() != nil && ev.GetQuote().GetBidPrice() != nil && ev.GetQuote().GetAskPrice() != nil:
		bid, ok := decutil.FromProtoChecked(ev.GetQuote().GetBidPrice())
		if !ok {
			return nil
		}
		ask, ok := decutil.FromProtoChecked(ev.GetQuote().GetAskPrice())
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
	s.sweepLocked()
	s.mu.Unlock()
	return nil
}

// sweepLocked releases the price of every expired entry, keeping its asOf. The
// caller holds the write lock.
//
// It is a no-op when maxAge <= 0, because that caller (tv-sync) chose marks that
// never expire — there is nothing to tombstone, and sweeping would be a walk
// that can never free anything.
func (s *Source) sweepLocked() {
	if s.maxAge <= 0 {
		return
	}
	now := s.now()
	if !s.lastSweep.IsZero() && now.Sub(s.lastSweep) < sweepInterval {
		return
	}
	s.lastSweep = now
	for id, e := range s.prices {
		if e.price != nil && s.expired(e) {
			s.prices[id] = entry{price: nil, asOf: e.asOf}
		}
	}
}

// isMarkBearingEventType reports whether eventType announces a payload this
// fold can safely unmarshal as a market.v1.MarketDataEvent.
//
// services/market-data/internal/feed/bussink.go stamps every MarketDataEvent
// it publishes as market.<assetClass>.<variant>, where variant comes from the
// oneof actually set: "trade" or "quote" for the two variants this fold uses,
// "bar" for the one it doesn't. market.book.snapshot (a DIFFERENT publisher,
// internal/marketedge/ingest/engine.go) uses the bare subject as its
// EventType too, with no third segment — a shape this check also rejects, on
// top of "snapshot" not being a variant this fold understands.
//
// Anything that is not exactly "market.<assetClass>.trade" or
// "market.<assetClass>.quote" is refused here, before Unmarshal ever runs —
// by the time the bytes are decoded, a bids-only OrderBookSnapshot and a
// genuine Trade are indistinguishable (see the Handle doc comment).
func isMarkBearingEventType(eventType string) bool {
	parts := strings.Split(eventType, ".")
	if len(parts) != 3 || parts[0] != domainMarket {
		return false
	}
	switch parts[2] {
	case "trade", "quote":
		return true
	default:
		return false
	}
}

// domainMarket is the first EventType segment every mark-bearing event
// shares — market.<assetClass>.<variant>.
const domainMarket = "market"

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

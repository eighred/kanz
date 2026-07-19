# COMP-M2 Reference Price Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the OMS pre-trade gate a fresh, staleness-bounded reference price so MARKET and STOP orders can be valued at admission instead of being refused `PRICE_UNAVAILABLE`.

**Architecture:** Lift tv-sync's existing last-mark fold into `internal/marketdata/mark`, add an `asOf` per instrument and a max-age bound, and have the OMS fold `market.>` into it. The gate change is a price *supply*, not a new decision path: an unknown or expired mark yields nil, and `internal/compliance/gate.go:245` already refuses a non-positive price as `Unpriced`.

**Tech Stack:** Go 1.26, NATS/JetStream via `pkg/bus`, protobuf (`kanz-schemas-go`), `math/big.Rat` for money.

## Global Constraints

- **No floats on the price path.** `*big.Rat` internally, `*commonpb.Decimal` at the compliance boundary via `internal/dec`. Never `float64`, never a hard-coded cent exponent.
- **The refusal must survive.** An unavailable or expired price still rejects with `PRICE_UNAVAILABLE`. This adds a price, not a bypass.
- **Age is measured from the market event's own `event_time`**, not our receive time.
- **tv-sync's observable behaviour must not change.** It passes `maxAge = 0` (never expire).
- Build/test with `GOFLAGS=-mod=mod` from the `kanz/` directory. Run the full suite with `-p 1` (packages share one Postgres; see the board).
- TDD: every test is watched failing before the implementation is written.
- Do NOT wire `bus.WithRetry` anywhere (`test/arch/bus_dlq_test.go` fails the build).

---

### Task 1: The shared staleness-aware mark source

**Files:**
- Create: `kanz/internal/marketdata/mark/mark.go`
- Test: `kanz/internal/marketdata/mark/mark_test.go`

**Interfaces:**
- Consumes: `internal/dec.FromProto`, `envelopepb.Envelope`, `marketpb.MarketDataEvent`.
- Produces: `mark.New(now func() time.Time, maxAge time.Duration) *Source`; `(*Source).Handle(ctx, *envelopepb.Envelope, []byte) error` (a `bus.EventHandler`); `(*Source).Mark(instrument string) *big.Rat`; `(*Source).Lookup(instrument string) (*big.Rat, time.Time, bool)`.

- [ ] **Step 1: Write the failing test**

Create `kanz/internal/marketdata/mark/mark_test.go`:

```go
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
		Payload:      &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: price}},
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
		Payload: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/marketdata/mark/ -v`
Expected: FAIL — the package does not exist (`no required module provides package .../internal/marketdata/mark`).

If instead the marshal helpers fail to compile, check the real field names on `marketpb.MarketDataEvent`, `marketpb.Trade` and `marketpb.Quote` in `kanz-schemas/proto/market/v1/market_data.proto` and fix the test before proceeding. Do not change the assertions.

- [ ] **Step 3: Write the implementation**

Create `kanz/internal/marketdata/mark/mark.go`:

```go
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
// payloads (Bar) are ignored. Malformed events are acked — a price monitor
// never wedges the partition.
func (s *Source) Handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	var ev marketpb.MarketDataEvent
	if proto.Unmarshal(payload, &ev) != nil || ev.GetInstrumentId() == "" {
		return nil
	}
	var price *big.Rat
	switch {
	case ev.GetTrade() != nil && ev.GetTrade().GetPrice() != nil:
		price = dec.FromProto(ev.GetTrade().GetPrice())
	case ev.GetQuote() != nil && ev.GetQuote().GetBidPrice() != nil && ev.GetQuote().GetAskPrice() != nil:
		mid := new(big.Rat).Add(dec.FromProto(ev.GetQuote().GetBidPrice()), dec.FromProto(ev.GetQuote().GetAskPrice()))
		price = mid.Quo(mid, big.NewRat(2, 1))
	default:
		return nil
	}
	if price == nil || price.Sign() <= 0 {
		return nil
	}
	s.mu.Lock()
	s.prices[ev.GetInstrumentId()] = entry{price: price, asOf: eventTime(env, &ev)}
	s.mu.Unlock()
	return nil
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/marketdata/mark/ -v`
Expected: PASS, 8 tests.

- [ ] **Step 5: Prove the expiry guard is non-vacuous**

Temporarily change `expired` to `return false`, re-run, and confirm
`TestAMarkOlderThanMaxAgeIsNil` and `TestLookupSeparatesNeverSeenFromExpired`
FAIL. Then revert. A staleness bound that has never refused anything is
indistinguishable from one that does not work.

- [ ] **Step 6: Commit**

```bash
cd kanz && gofmt -w internal/marketdata/mark/
git add kanz/internal/marketdata/mark/
git commit -m "feat(marketdata): staleness-aware last-mark source"
```

---

### Task 2: Migrate tv-sync to the shared source

**Files:**
- Delete: `kanz/services/tv-sync/internal/markfeed/markfeed.go`, `kanz/services/tv-sync/internal/markfeed/markfeed_test.go`
- Modify: `kanz/services/tv-sync/cmd/tv-sync/main.go:27` (import), `:82` (construction)

**Interfaces:**
- Consumes: `mark.New(now, maxAge) *Source` from Task 1.
- Produces: nothing new. `*mark.Source` satisfies `projection.MarkSource` (`Mark(string) *big.Rat`) structurally, so `projection` is untouched.

- [ ] **Step 1: Confirm the behaviour to preserve, before changing anything**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/tv-sync/... -v 2>&1 | tail -30`
Expected: PASS. Record the count. These same tests passing after the migration
IS the proof that the lift changed nothing — there is no separate assertion to write.

- [ ] **Step 2: Delete the old package**

```bash
cd kanz && git rm -r services/tv-sync/internal/markfeed/
```

The tests go with it: Task 1's suite covers the same fold semantics plus expiry,
and keeping a second copy would be the duplication this task exists to remove.

- [ ] **Step 3: Rewire the composition root**

In `kanz/services/tv-sync/cmd/tv-sync/main.go`, replace the import at line 27:

```go
	"github.com/kanz-eng/kanz/internal/marketdata/mark"
```

and the construction at line 82:

```go
	// maxAge 0 — tv-sync's marks deliberately DO NOT expire, which is the
	// behaviour it has always had and is preserved here rather than inherited by
	// accident. A stale mark makes a P&L number slightly old; the projection
	// already degrades to empty when a mark is missing entirely. Whether a P&L
	// display should refuse an hours-old mark is a real question and a separate
	// decision — it is not settled by an OMS task needing a bound of its own.
	mark := mark.New(time.Now, 0)
```

If the local variable name `mark` now shadows the package name in a way the
compiler rejects, rename the variable to `marks` and update its uses in this
file only.

- [ ] **Step 4: Run the tv-sync suite**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/tv-sync/... -v 2>&1 | tail -30`
Expected: PASS, same count as Step 1.

- [ ] **Step 5: Confirm nothing else referenced the old package**

Run: `cd kanz && grep -rn "internal/markfeed" --include=*.go . ; GOFLAGS=-mod=mod go build ./...`
Expected: no grep output, build clean.

- [ ] **Step 6: Commit**

```bash
cd kanz && gofmt -w services/tv-sync/
git add -A kanz/services/tv-sync/
git commit -m "refactor(tv-sync): use the shared mark source; behaviour unchanged"
```

---

### Task 3: The gate values MARKET/STOP from the mark source

**Files:**
- Modify: `kanz/services/oms/internal/compliance/comp01.go:21-53`
- Test: `kanz/services/oms/internal/compliance/comp01_price_test.go` (create)

**Interfaces:**
- Consumes: `(*mark.Source).Mark(string) *big.Rat` from Task 1, `dec.ToProto`.
- Produces: `compliance.MarkSource` interface; `compliance.WithMarkSource(MarkSource) COMP01Option`; `NewCOMP01Gate(gate, currency string, opts ...COMP01Option)`.

**Note on ordering:** `NewCOMP01Gate` gains a variadic parameter, so existing
call sites keep compiling unchanged. With no source wired the behaviour is
exactly today's — that is what makes this task independently safe to land.

- [ ] **Step 1: Write the failing test**

Create `kanz/services/oms/internal/compliance/comp01_price_test.go`.

**Read this before writing it.** The existing tests are in `package compliance`
(an internal test package), so names are unqualified. Two fixtures already exist
in `comp01_test.go` and MUST be reused rather than reinvented:
`concentrationMandate(maxPct)` and `unpricedOrder(orderType)` — the latter builds
a BUY of 10 AAPL for portfolio `p1` with no price, which is exactly a MARKET
order on the wire.

**The trap this fixture avoids:** `newTestGate(t)` uses an EMPTY
`comp.MapBookSource{}`. Once a market order is successfully valued it becomes
100% of a zero-position portfolio and BREACHES the 60% concentration cap — so a
test asserting "admitted" against `newTestGate` fails for a reason that has
nothing to do with pricing. The seeded book below mirrors
`TestCheck_PricedLimitOrderUnaffected`: 80,000 of existing positions, so a
10 × 100 = 1,000 AAPL buy lands at ~1.2% and stays well under the cap.

```go
package compliance

import (
	"context"
	"math/big"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	comp "github.com/kanz-eng/kanz/internal/compliance"
)

// stubMarks is a MarkSource returning a fixed price for one instrument.
type stubMarks struct {
	instrument string
	price      *big.Rat
}

func (s stubMarks) Mark(instrument string) *big.Rat {
	if instrument == s.instrument && s.price != nil {
		return new(big.Rat).Set(s.price)
	}
	return nil
}

// bookedTestGate is newTestGate with a NON-EMPTY book, so a successfully valued
// order has something to be a small fraction OF. Without the existing positions
// every priced order is 100% of the portfolio and breaches the concentration
// cap, which would look like a pricing failure and is not one.
func bookedTestGate(t *testing.T) *comp.PreTradeGate {
	t.Helper()
	reg := comp.NewMandateRegistry()
	reg.Put(concentrationMandate(60))
	books := comp.MapBookSource{"p1": &comp.Book{
		PortfolioID: "p1", BaseCurrency: "USD",
		Positions: []comp.Position{
			{
				InstrumentID: "MSFT",
				Quantity:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
				MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 40000, Exponent: 0}, CurrencyCode: "USD"},
			},
			{
				InstrumentID: "GOOG",
				Quantity:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
				MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 40000, Exponent: 0}, CurrencyCode: "USD"},
			},
		},
	}}
	return comp.NewPreTradeGate(nil, books, reg, nil, nil, nil)
}

func TestCheck_MarketOrderIsValuedFromTheMark(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(100, 1)}))

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("breach = %+v, want nil — a MARKET order with a fresh mark must be valued and admitted", breach)
	}
}

func TestCheck_MarketOrderWithNoMarkIsStillRefused(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "SOMETHING-ELSE", price: big.NewRat(100, 1)}))

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("breach = %+v, want PRICE_UNAVAILABLE — an order we cannot value must still be refused. "+
			"This adds a price, not a bypass", breach)
	}
}

func TestCheck_MarketOrderWithNoSourceWiredIsRefusedAsBefore(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD") // no WithMarkSource

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("breach = %+v, want PRICE_UNAVAILABLE — with no source wired the behaviour must be exactly COMP-M1's", breach)
	}
}

// NON-VACUITY: a LIMIT order must value from ITS OWN limit price and never
// consult the mark. Without this, an implementation that always used the mark
// would pass every test above while silently repricing every limit order.
//
// The numbers are chosen so the two paths give OPPOSITE verdicts: 10 at a limit
// of 1.00 is a notional of 10 (~0.01% of the book, admitted), while 10 at the
// stub's 1,000,000 mark is 10,000,000 (~99% of the book, a concentration
// breach). A nil breach here can only mean the limit price was used.
func TestCheck_LimitOrderIgnoresTheMark(t *testing.T) {
	cmd := unpricedOrder(orderpb.OrderType_ORDER_TYPE_LIMIT)
	cmd.LimitPrice = &commonpb.Decimal{Coefficient: 100, Exponent: -2} // 1.00

	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(1000000, 1)}))

	breach, err := g.Check(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("breach = %+v, want nil — a LIMIT order must be valued at its own limit price, "+
			"not repriced at the mark", breach)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/oms/internal/compliance/ -run 'Mark|Limit' -v`
Expected: FAIL to compile — `undefined: WithMarkSource`.

- [ ] **Step 3: Write the implementation**

In `kanz/services/oms/internal/compliance/comp01.go`, add to the imports:

```go
	"math/big"

	"github.com/kanz-eng/kanz/internal/dec"
```

Add above `COMP01Gate`:

```go
// MarkSource supplies a reference price for an order that carries none. It is
// satisfied by internal/marketdata/mark.Source. nil means "no usable price" —
// never a zero, never a guess.
type MarkSource interface {
	Mark(instrument string) *big.Rat
}

// COMP01Option configures the adapter.
type COMP01Option func(*COMP01Gate)

// WithMarkSource supplies the reference price used to value MARKET and STOP
// orders, which carry no limit price of their own (COMP-M2). Without it those
// orders are refused PRICE_UNAVAILABLE, which is COMP-M1's behaviour and
// remains the behaviour whenever the source has no fresh mark to give.
func WithMarkSource(m MarkSource) COMP01Option {
	return func(g *COMP01Gate) { g.marks = m }
}
```

Add the field to `COMP01Gate`:

```go
type COMP01Gate struct {
	gate     *comp.PreTradeGate
	currency string
	now      func() time.Time
	marks    MarkSource
}
```

Change the constructor:

```go
func NewCOMP01Gate(gate *comp.PreTradeGate, currency string, opts ...COMP01Option) *COMP01Gate {
	g := &COMP01Gate{gate: gate, currency: currency, now: time.Now}
	for _, opt := range opts {
		opt(g)
	}
	return g
}
```

Replace the `Price:` line (currently `comp01.go:48`) with `Price: g.price(cmd),`
and add the method:

```go
// price values the order's notional.
//
// A LIMIT or STOP_LIMIT order carries its own limit price and is valued at it —
// that is the price the fund has committed to, and repricing it at the market
// would evaluate a different order than the one submitted. A MARKET or STOP
// order carries none, so it is valued at the reference mark.
//
// The switch mirrors execution.SimVenue.executionPrice, deliberately: one
// question ("what price does this order type carry?") should not have two
// different answers in one codebase.
//
// nil is returned when there is no fresh mark, and nil is what the gate already
// refuses (Decision.Unpriced). There is no new rejection path here and no way
// to admit an order without a real price for it.
func (g *COMP01Gate) price(cmd *orderpb.SubmitOrder) *commonpb.Decimal {
	switch cmd.GetOrderType() {
	case orderpb.OrderType_ORDER_TYPE_LIMIT, orderpb.OrderType_ORDER_TYPE_STOP_LIMIT:
		return cmd.GetLimitPrice()
	}
	if g.marks == nil {
		return nil
	}
	m := g.marks.Mark(cmd.GetInstrumentId())
	if m == nil {
		return nil
	}
	return dec.ToProto(m)
}
```

Delete the stale COMP-M2 comment that sat above the old `Price:` line.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/oms/internal/compliance/ -v`
Expected: PASS — the four new tests plus every pre-existing test in the package.

- [ ] **Step 5: Commit**

```bash
cd kanz && gofmt -w services/oms/internal/compliance/
git add kanz/services/oms/internal/compliance/
git commit -m "feat(oms): value MARKET/STOP orders from a reference mark"
```

---

### Task 4: OMS config — price subject and max age

**Files:**
- Modify: `kanz/services/oms/internal/config/config.go`
- Test: `kanz/services/oms/internal/config/config_price_test.go` (create)

**Interfaces:**
- Produces: `Config.PriceSubject string`, `Config.PriceMaxAge time.Duration`.

- [ ] **Step 1: Write the failing test**

Create `kanz/services/oms/internal/config/config_price_test.go`:

```go
package config_test

import (
	"testing"
	"time"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
)

func TestPriceDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PriceSubject != "market.>" {
		t.Errorf("PriceSubject = %q, want \"market.>\"", cfg.PriceSubject)
	}
	if cfg.PriceMaxAge != 30*time.Second {
		t.Errorf("PriceMaxAge = %v, want 30s", cfg.PriceMaxAge)
	}
}

func TestPriceMaxAgeIsConfigurable(t *testing.T) {
	t.Setenv("OMS_PRICE_MAX_AGE", "5s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PriceMaxAge != 5*time.Second {
		t.Errorf("PriceMaxAge = %v, want 5s", cfg.PriceMaxAge)
	}
}

// An unparseable duration must not silently become zero — zero means "never
// expire" to the mark source, so a typo would disable the staleness bound
// entirely and admit orders against arbitrarily old prices.
func TestUnparseableMaxAgeIsAnError(t *testing.T) {
	t.Setenv("OMS_PRICE_MAX_AGE", "half a minute")
	if _, err := config.Load(); err == nil {
		t.Fatal("Load returned nil error for an unparseable OMS_PRICE_MAX_AGE — " +
			"a typo would silently disable the staleness bound")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/oms/internal/config/ -run Price -v`
Expected: FAIL to compile — `cfg.PriceSubject undefined`.

- [ ] **Step 3: Write the implementation**

In `kanz/services/oms/internal/config/config.go`, add to the `Config` struct:

```go
	// PriceSubject is the market-data spine the OMS folds into its reference-mark
	// source, so the pre-trade gate can value MARKET/STOP orders (COMP-M2).
	PriceSubject string

	// PriceMaxAge is how old a mark may be and still value an order. It is a
	// SAFETY BOUND, not a tuning knob: widening it to quiet PRICE_UNAVAILABLE
	// refusals does not fix the feed, it just admits orders priced off a feed
	// that is no longer reporting. Zero disables expiry entirely and is
	// deliberately NOT reachable from the environment (an unparseable value is
	// an error, not a fallback to zero).
	PriceMaxAge time.Duration
```

Add `"time"` to the imports if absent. In `Load()`, add to the struct literal:

```go
		PriceSubject: envOr("OMS_PRICE_SUBJECT", "market.>"),
```

and after the literal is built, before the existing return:

```go
	maxAge, err := time.ParseDuration(envOr("OMS_PRICE_MAX_AGE", "30s"))
	if err != nil {
		return Config{}, fmt.Errorf("OMS_PRICE_MAX_AGE: %w", err)
	}
	cfg.PriceMaxAge = maxAge
```

Add `"fmt"` to the imports if absent. If `Load()` currently returns
`Config{...}, nil` directly from a literal, restructure it to assign to `cfg`
first — match whatever shape the function already has.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/oms/internal/config/ -v`
Expected: PASS, including pre-existing config tests.

- [ ] **Step 5: Commit**

```bash
cd kanz && gofmt -w services/oms/internal/config/
git add kanz/services/oms/internal/config/
git commit -m "feat(oms): price subject + staleness bound config"
```

---

### Task 5: Wire the OMS composition root

**Files:**
- Modify: `kanz/services/oms/cmd/oms/main.go` (gate construction ~`:231`, subscription list ~`:257`)

**Interfaces:**
- Consumes: everything from Tasks 1, 3 and 4.
- Produces: nothing further.

- [ ] **Step 1: Construct the mark source and pass it to the gate**

In `kanz/services/oms/cmd/oms/main.go`, add the import:

```go
	"github.com/kanz-eng/kanz/internal/marketdata/mark"
```

Construct the source **above `main.go:162`** — before `preTrade` is built, because
Step 2's observer closure captures `marks`:

```go
	// COMP-M2: the reference-mark source the pre-trade gate values MARKET/STOP
	// orders from. It starts EMPTY and warms as the spine delivers, so a freshly
	// started pod refuses market orders until the first tick for that instrument
	// arrives. That is the safe direction and it is deliberate — the alternative
	// is admitting an order at a price we do not have.
	marks := mark.New(time.Now, cfg.PriceMaxAge)
```

At `main.go:167` the adapter is built as:

```go
	gate := compliance.NewCOMP01Gate(preTrade, cfg.BaseCurrency)
```

(`compliance` is the un-aliased `services/oms/internal/compliance`; `comp` is the
aliased `internal/compliance`.) Change it to:

```go
	gate := compliance.NewCOMP01Gate(preTrade, cfg.BaseCurrency,
		compliance.WithMarkSource(marks),
	)
```

- [ ] **Step 2: Make the refusal diagnosable**

`preTrade` is already built with options at `main.go:162-166`
(`comp.WithRequireMandate`, `comp.WithUngovernedObserver`). Append a third to
that same call — `NewPreTradeGate` is variadic (`opts ...PreTradeOption`).

```go
	comp.WithUnpricedObserver(func(portfolioID, instrumentID string) {
		// Two very different incidents arrive at the same refusal, and an
		// operator needs to tell them apart: a mark we have NEVER seen means a
		// cold pod, a thin instrument, or a subscription delivering nothing;
		// a mark we HAVE seen but which expired means the feed was working and
		// stalled. One is a warm-up, the other is an outage.
		if _, asOf, seen := marks.Lookup(instrumentID); seen {
			logger.Warn("order refused: the reference mark is STALE — the price feed has stopped reporting for this instrument",
				"portfolio", portfolioID, "instrument", instrumentID,
				"mark_as_of", asOf, "max_age", cfg.PriceMaxAge)
			return
		}
		logger.Warn("order refused: NO reference mark has ever been seen for this instrument — a cold pod warming up, an instrument nothing quotes, or a price subscription delivering nothing",
			"portfolio", portfolioID, "instrument", instrumentID, "subject", cfg.PriceSubject)
	}),
```

- [ ] **Step 3: Subscribe the price spine**

In the `subs` assembly (after the `FillSubjects` loop), add:

```go
	// The price spine is a plain work-queue subscription: unlike the mandate
	// registry below, a mark is not a control that must reach every replica —
	// it is refreshed continuously, so a pod that misses one tick gets the next.
	subs = append(subs, sub{cfg.PriceSubject, marks.Handle})
```

- [ ] **Step 4: Build and vet**

Run: `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./...`
Expected: both clean, no output.

- [ ] **Step 5: Run the full suite**

Run: `cd kanz && GOFLAGS=-mod=mod go test -p 1 ./... 2>&1 | grep -vE "^ok |no test files"`
Expected: no output (every package passes).

- [ ] **Step 6: Confirm the arch guards still hold**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -v 2>&1 | grep -E "^--- (FAIL|PASS)" | head -30`
Expected: all PASS. In particular `TestEveryBusConsumerWiresADLQ` and
`TestNoBusConsumerWiresRetryWhileHandlersResumeByAcking` — the new subscription
rides the existing consumer, so neither should change, and a failure here means
a second consumer was constructed.

- [ ] **Step 7: Commit**

```bash
cd kanz && gofmt -w services/oms/cmd/oms/
git add kanz/services/oms/cmd/oms/
git commit -m "feat(oms): fold the price spine and value market orders from it"
```

---

### Task 6: Update the board

**Files:**
- Modify: `KANZ_TASKS.md` (the COMP-M2 row in the PRODUCTION READINESS BOARD and the COMP-M2 entry under TODO → "Buildable now")

- [ ] **Step 1: Close the COMP-M2 TODO entry and move it to DONE**

Remove the COMP-M2 block from TODO → "Buildable now". Record in DONE that MARKET
and STOP orders can now be admitted, valued at a mark bounded by
`OMS_PRICE_MAX_AGE`, and that the refusal survives when no fresh mark exists.

- [ ] **Step 2: Correct the board's bridge.go claim**

The COMP-M2 TODO text scopes in `bridge.go` as "the same hole reached by a
different caller". Add a row recording that this is false: `bridge.Materialize`
has no caller outside its own tests (`services/optimization/internal/server/server.go:167`
calls `ToOrders` and renders JSON — it neither gates nor publishes), so its
`float64` prices and `int64(math.Round(p*100))` cent exponent are unreachable
today and should be fixed at wiring time, against a caller that can prove the fix.

- [ ] **Step 3: Record the cold-start behaviour**

Note on the COMP-M2 row that a freshly started OMS refuses MARKET/STOP orders
until its mark map warms, that this is deliberate, and that the two refusal
causes (never-seen vs expired) are distinguishable in the logs.

- [ ] **Step 4: Validate the board table**

Run: `cd /c/Users/root/Desktop/eighred-kanz && awk 'NR>=15 && NR<=60 {n=gsub(/\|/,"|"); if (n!=6 && n!=0) print NR": "n" pipes"}' KANZ_TASKS.md`
Expected: no output — every table row has exactly 5 columns.

- [ ] **Step 5: Commit**

```bash
git add KANZ_TASKS.md
git commit -m "docs(board): COMP-M2 closed; correct the bridge.go scope claim"
```

---

## Verification (whole feature)

- [ ] `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./...` — clean.
- [ ] `cd kanz && GOFLAGS=-mod=mod go test -p 1 ./...` — every package passes.
- [ ] A MARKET order with a fresh mark is admitted; with no mark, or an expired one, it is refused `PRICE_UNAVAILABLE`.
- [ ] A LIMIT order is still valued at its own limit price (`TestLimitOrderIgnoresTheMark`).
- [ ] tv-sync's suite passes unchanged — the lift altered no behaviour.
- [ ] `grep -rn "float64" kanz/services/oms/internal/compliance/ kanz/internal/marketdata/mark/` returns nothing.

## Known gaps, deliberately not closed here

- **`bridge.Materialize`** — unwired; `float64` money and a hard-coded cent exponent. Fix at wiring time (Task 6, Step 2).
- **SimVenue `WithPrice`** — still unwired, so simulated MARKET orders keep refusing with `ErrUnpriced`. Wiring a live mark into a simulation would make a paper deployment fill against real quotes.
- **`internal/marketdata/ingest.go:148` `midDecimal`** — the same bid/ask mid in a second representation. Now in the same package tree as the fold, for a later pass.
- **One global `PriceMaxAge`** across asset classes. Correct while both live venues are crypto; a session-based venue needs per-class bounds and an instrument→class lookup that does not exist.

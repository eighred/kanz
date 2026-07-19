# COMP-M2 — a reference price so the pre-trade gate can value MARKET/STOP orders

**Date:** 2026-07-19
**Status:** design approved, pending implementation plan
**Board row:** COMP-M2 (TODO → "Buildable now")

## Context

COMP-M1 made the pre-trade gate refuse any order it cannot value, replacing a gate
that valued unpriced orders at zero and silently admitted them. That was the right
direction and it created a real capability gap: a `SubmitOrder` carries a limit
price and nothing else, so a MARKET or STOP order reaches `PreTradeGate.Evaluate`
with no notional, is refused as `Unpriced`, and comes back `PRICE_UNAVAILABLE`.

Market and stop orders against a governed portfolio therefore cannot be admitted
at all today. This design supplies the missing reference price **without weakening
the refusal** — an order that still cannot be valued must still be rejected.

## Ground truth

Verified against the code on 2026-07-19, not taken from the board.

| Fact | Evidence |
|---|---|
| The gate values an order from `OrderDelta.Price` (`*commonpb.Decimal`) | `internal/compliance/gate.go:99` |
| A non-positive price yields `Decision{Unpriced: true}` | `gate.go:245-247` |
| The OMS passes only the limit price | `services/oms/internal/compliance/comp01.go:48` |
| `market-data` has **no query surface** — only `/healthz`, `/readyz`, `/metrics`, and no store | `services/market-data/internal/server/server.go:51-54` |
| Prices move over the bus on `market.>` as `market.v1.MarketDataEvent` | `services/market-data/internal/config/config.go:59`, `feed/bussink.go:35` |
| The OMS has no market-data seam at all — no import, no subscription | grep over `services/oms/` |
| A last-price fold already exists: trade-last, else quote-mid, `*big.Rat`, nil when unknown | `services/tv-sync/internal/markfeed/markfeed.go:22-66` |
| That fold stores **no timestamp**, so staleness is impossible today | `markfeed.go:24` — `prices map[string]*big.Rat` |
| It is unreachable from the OMS (Go internal rule) | `services/tv-sync/internal/...` |
| `MarketDataEvent.event_time` is the per-event venue timestamp, authoritative inside a batch | `kanz-schemas/proto/market/v1/market_data.proto:36-39` |
| `*big.Rat` ↔ `*commonpb.Decimal` conversion exists | `internal/dec` — `FromProto`, `ToProto` |
| The gate already has an unpriced-refusal observer hook | `gate.go:68-73` — `WithUnpricedObserver` |

### One board claim corrected

The board scopes `services/optimization/internal/bridge/bridge.go:113-132` into
COMP-M2 as "the same hole reached by a different caller". **It is not reached.**
`bridge.Materialize` — the only path that builds an `OrderDelta` from the
`map[string]float64` — has no caller outside its own tests. The optimization
server's `handleOrders` calls `bridge.ToOrders` and renders JSON
(`services/optimization/internal/server/server.go:167-181`); it neither gates nor
publishes. Fixing bridge would have no production effect, so it is **out of scope**
and recorded on the board instead (see *Out of scope*).

## Scope

**In:**
- A shared, staleness-aware last-mark source.
- OMS folds `market.>` into it and consults it when admitting MARKET/STOP orders.
- tv-sync migrates to the shared source with its current behaviour preserved.

**Out:**
- `SimVenue`'s `WithPrice` seam. MARKET orders on the sim venue keep refusing with
  `ErrUnpriced` (`2b6db53`). Wiring a live mark into a simulation would make a
  paper deployment fill against real quotes and would idle that guard.
- `bridge.Materialize` (unreachable — see above).
- Reconciling `internal/marketdata/ingest.go:148`'s `midDecimal` with the fold's
  `*big.Rat` mid. Same concept in two representations; predates this task. The lift
  puts them in one package tree for a later pass.
- Whether tv-sync's marks should themselves expire. Real question, separate decision.

## Design

### 1. `internal/marketdata/mark` — the shared fold

Lift `services/tv-sync/internal/markfeed` to `internal/marketdata/mark`. The lift is
forced, not stylistic: Go's internal rule makes the tv-sync package unreachable from
the OMS, so sharing one implementation requires moving it. `internal/marketdata`
already houses the market-data domain (`ingest`, `returns`, `store`), so this is a
sibling concern in an existing tree rather than a new parallel one.

```go
type Source struct { ... }

// New returns a mark source. maxAge == 0 means marks never expire.
func New(now func() time.Time, maxAge time.Duration) *Source

// Handle is the bus.EventHandler folding market.v1.MarketDataEvent.
func (s *Source) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error

// Mark returns the latest non-expired mark, or nil.
func (s *Source) Mark(instrument string) *big.Rat

// Lookup returns the stored entry REGARDLESS of expiry, so a caller can tell
// "never seen" (seen == false) from "seen but expired" (seen == true, asOf old).
// Mark is the safe accessor and applies maxAge; Lookup is the diagnostic one and
// does not. Only the unpriced observer should use Lookup — never the gate path.
func (s *Source) Lookup(instrument string) (price *big.Rat, asOf time.Time, seen bool)
```

Fold semantics are carried over unchanged — a `Trade` sets the mark to the trade
price, a `Quote` to the bid/ask mid, other payloads are ignored, malformed events
are acked rather than wedging the partition. What is added is the `asOf` recorded
alongside each price, taken from `MarketDataEvent.event_time` and falling back to
the envelope's `event_time` when the payload omits it.

**Age is measured from the event's own timestamp, not our receive time.** That is
what is true about the quote rather than about our plumbing, and replayed events
cannot poison it because the live-mode validator hard-rejects
`QUALITY_FLAG_REPLAYED` before dispatch (`pkg/bus/consumer.go:154`).

`Mark` keeps returning `*big.Rat`, so `projection.MarkSource` in tv-sync is still
satisfied structurally and needs no change. `*commonpb.Decimal` conversion happens
only at the compliance boundary, via `dec.ToProto`.

### 2. tv-sync migration

tv-sync constructs the shared source with `maxAge = 0` and a comment recording that
unbounded marks are its **existing, deliberately preserved** behaviour — not an
oversight inherited by the move, and not a judgement that stale marks are fine for
P&L. Its observable behaviour is unchanged.

### 3. OMS composition root

The OMS gains a subscription that folds the price spine:

- `OMS_PRICE_SUBJECT`, default `market.>`
- `OMS_PRICE_MAX_AGE`, default `30s`

It uses the DLQ-wired consumer from `936a2a9`, so a poison market event parks
rather than looping.

### 4. `comp01.go`

When the order carries no usable limit price — MARKET and STOP — consult the mark
source and convert with `dec.ToProto`. Everything else is unchanged.

The refusal path is **reused, not rebuilt**: an unknown or expired mark yields nil,
nil is not positive, and `gate.go:245` already returns `Unpriced` → `PRICE_UNAVAILABLE`.
No new rejection code, no new branch in the gate. This is the property that keeps
the task from becoming a bypass: the only way to admit an order is to have a real,
fresh price for it.

### 5. Observability — cold vs stalled

A fresh pod's map is empty, so MARKET orders refuse until the first tick for that
instrument arrives. That is accepted: it fails in the safe direction and matches
COMP-M1's stance. It must be **diagnosable**, which is what `Lookup` is for. The
OMS's `WithUnpricedObserver` distinguishes:

- **never seen** — a cold map, a thin instrument, or a subscription that is not
  delivering at all;
- **seen but expired** — the feed was working and stalled.

These are different incidents with different responses, and a single
`PRICE_UNAVAILABLE` count cannot tell them apart.

## What must not regress

- An unavailable or expired price still **rejects**. This adds a price, not a bypass.
- No float on the price path. `*big.Rat` internally, `*commonpb.Decimal` at the
  boundary; no `float64`, no hard-coded cent exponent.
- tv-sync's projection output is byte-identical for the same inputs.
- The gate's existing rules are untouched — this changes what `Price` is populated
  with, never how the gate decides.

## Testing

TDD throughout; every test watched failing first.

**Unit — `internal/marketdata/mark`:** a trade sets the mark; a quote sets the mid;
an unknown instrument is nil; a mark older than `maxAge` is nil while a fresher one
survives; `maxAge = 0` never expires; `Lookup` separates never-seen from expired;
a malformed payload is acked and changes nothing.

**Unit — `comp01.go`:** a MARKET order with a fresh mark is valued and admitted; the
same order with no mark is refused `PRICE_UNAVAILABLE`; with an expired mark, also
refused. **Non-vacuity:** a LIMIT order still values from its own limit price and
never consults the source — otherwise a change that always used the mark would pass.

**Regression:** the COMP-M1 refusal survives — an unpriced MARKET order against a
governed portfolio is still rejected, proving this did not become a bypass.

**tv-sync:** existing projection tests pass unchanged, which is the migration's
correctness proof.

## Risks

- **Cold-start refusals on every rolling restart.** Accepted and instrumented; see §5.
  If they prove disruptive in practice the readiness-gate option is the fallback,
  but it trades a refusal window for a pod that never becomes ready if its feed is
  silent.
- **A single global `maxAge` across asset classes.** Correct while both live venues
  are crypto and trade continuously. A venue with sessions and overnight gaps needs
  per-class bounds and an instrument→class lookup that does not exist yet.
- **`OMS_PRICE_MAX_AGE` is an operator-loosenable safety bound.** Widening it to
  silence `PRICE_UNAVAILABLE` alerts is the obvious wrong move; the default should
  be documented at the config site as a safety bound, not a tuning knob.

## Out of scope, recorded on the board

`bridge.Materialize` is a complete, tested, **unwired** seam carrying `float64`
prices and `int64(math.Round(p*100))` — a hard-coded cent exponent under which any
instrument priced below 0.005 rounds to zero. COMP-M1 refuses a zero price, so that
defect now fails loudly instead of silently bypassing every rule — which is why it
is not urgent. It should be fixed **at wiring time**, against a caller that can
prove the fix, rather than now against tests written by the same hand as the change.

# COMP-M1 — the pre-trade gate must REFUSE to evaluate an unpriced order

## Context

**The pre-trade compliance gate is bypassed by every MARKET and STOP order.** Verified end to
end by the controller:

1. `services/oms/internal/compliance/comp01.go:48` passes `cmd.GetLimitPrice()` as the
   valuation price. It is **nil** for MARKET/STOP orders (only limit orders are guaranteed a
   positive price — `services/oms/internal/order/aggregate.go:66`).
2. `internal/compliance/gate.go:236` projects `MarketValue: mulDecimal(newQty, d.Price)` and
   `gate.go:239` **REPLACES** the existing position with that projection.
3. `gate.go:266-269` — `mulDecimal(a, nil)` returns zero.
4. `rules.go:249-258` — `heldPositions()` **skips every zero-market-value position**.
5. `rules.go:73, 110, 163, 194, 203` — five call sites. That is **Concentration, Restriction,
   IssuerExclusion, Currency and Leverage — the entire rule set.**

So a market order projects at zero value, is filtered out before any rule sees it, and PASSES.
Worse: because `project()` replaces the traded position, a market order touching an
**already-breaching holding erases that holding's real market value** from the check. The
comment at `comp01.go:44-47` claims "the existing book is still fully checked" — **that claim is
false**, and this task deletes it.

**The root cause is a collapse of two states**, and `gate.go:167-173` — in the same file —
already names this exact bug class for mandates (EXEC-M14):

> "TWO DIFFERENT STATES, and collapsing them is what made this silent."

`heldPositions` collapses:
- *quantity is zero* → genuinely flat, cannot breach → correctly skipped.
- *price is unknown* → **not flat at all**, and silently skipped.

**Lead decision (2026-07-16): REJECT. Fail closed.** Verbatim: *"Reject market orders when no
reference price exists. Do not allow compliance evaluation with an unknown price."*

**There are TWO callers of `PreTradeGate.Evaluate`, and both are affected:**
- `services/oms/internal/compliance/comp01.go:40` — nil price on every MARKET/STOP order.
- `services/optimization/internal/bridge/bridge.go:91` → `orderDelta(...)` at `bridge.go:119-122`
  leaves `price` **nil whenever the instrument is absent from the `prices` map**. The rebalancer
  has the same hole.

This is why the refusal goes in `gate.Evaluate` — the choke point both callers pass through.
Putting it in `comp01.go` would fix one door and leave the other open, which is this repo's
signature defect (a fact in two places with nothing comparing them).

## Global Constraints

- **The refusal is a TERMINAL REJECTION, never an `error`.** `comp01.Check` maps a returned
  `error` to "transient — the handler retries" (`comp01.go:36-38`). An unpriced market order is
  not transient: retrying will never add a price, so returning an error would spin a **hot
  infinite retry loop** on a live trading command. It MUST be a `Decision`, exactly like
  `Ungoverned`.
- **Follow the `Ungoverned` precedent exactly.** It is the house pattern for "refused under its
  own code, not a rule violation" (`gate.go:174-180`, `comp01.go:57-63`) and it exists for the
  reason this task needs: *"A reviewer reading MANDATE_MISSING knows to go and write a mandate —
  not to go and look for the rule that fired."* A reviewer reading the new code must know to go
  wire a price source.
- **Do not change what a priced order does.** Every existing passing test for limit orders must
  still pass, unchanged. This task removes a bypass; it does not retune any rule.
- `gofmt -l` clean, `go vet` clean, `go test ./...` green from `kanz/` with `GOFLAGS=-mod=mod`.
- Do not fix `bridge.go`'s `prices map[string]float64` / `math.Round(p*100)` cent-rounding. It is
  a real separate defect (an instrument priced below 0.005 rounds to zero) — the controller has
  logged it. **Your change makes it fail loudly instead of silently, which is the point.** Do not
  scope-creep into fixing the float64.

## Task 1 — `gate.Evaluate` refuses an unpriced order

**Files:** `kanz/internal/compliance/gate.go` (+ its existing test file).

**Requirements:**

1. Add `Unpriced bool` to the `Decision` struct, documented in the `Ungoverned` style: it means
   *the order could not be valued, so no rule could be evaluated* — nothing breached.
2. In `Evaluate`, refuse when the order has no usable price:
   - Place the check **AFTER** the `len(mandate.GetRules()) == 0` early return at
     `gate.go:181-183` and **BEFORE** `g.books.Book(...)` / `project(...)` at `gate.go:184-188`.
   - **Why after:** a mandate that constrains nothing performs no evaluation, so there is nothing
     to protect and no price is needed. Blocking market orders on an explicitly-unconstrained
     portfolio would be a behaviour change with zero safety benefit. This is not a loophole: an
     empty mandate already allows everything, by explicit choice.
   - **Why before the book load:** do not do I/O for a decision already made.
3. "No usable price" means `d.Price == nil` **OR** its value is **not strictly positive**. Zero
   and negative are refused too — a zero price produces exactly the zero market value this task
   exists to stop, and `bridge.go`'s cent-rounding can produce a real zero. There is an existing
   positivity helper in the OMS (`aggregate.go:66` uses `dec.IsPositive`); use the equivalent
   available to this package rather than hand-rolling sign logic if one exists — check
   `pkg/dec` first and reuse it.
4. Return `Decision{Allowed: false, Unpriced: true}` — **no error**.
5. Mirror the `noteUngoverned` observability precedent (`gate.go:175`): if `noteUngoverned` emits
   a metric/log, add the equivalent for unpriced refusals so ops can see this firing. Read what
   `noteUngoverned` actually does and match its shape — do not invent a different mechanism.

**Tests (TDD — write them first, watch them fail, then implement):**

- **THE REGRESSION TEST, and it must fail before your change:** a portfolio holding an
  **already-breaching** position, plus a MARKET order (nil price) touching that same instrument.
  Before: PASSES (the breach is erased). After: refused with `Unpriced`. This single test is the
  bug.
- Nil price + governed portfolio with ≥1 rule → `Allowed: false, Unpriced: true`, and **no
  violation** in the result (nothing breached).
- Zero price and negative price → refused the same way.
- Nil price + mandate with **zero rules** → `Allowed: true` (unchanged; no evaluation occurred).
- Nil price + **no mandate at all** → still `Ungoverned` (the existing behaviour wins; do not let
  the new check shadow it — this is why placement matters).
- A positive-price limit order → evaluates exactly as before (rules still fire, breaches still
  breach).

## Task 2 — both callers report the refusal honestly

**Files:** `kanz/services/oms/internal/compliance/comp01.go`,
`kanz/services/optimization/internal/bridge/bridge.go` (+ their existing test files).

**Requirements:**

1. **`comp01.go`:** map `dec.Unpriced` to a terminal `*Breach` under its own code, in the exact
   shape of the `MANDATE_MISSING` block at `comp01.go:57-63`. Code: `PRICE_UNAVAILABLE`. The
   reason must name the instrument and say the order cannot be valued — a reader must know to go
   wire a reference-price source, not to hunt for the rule that fired.
2. **`comp01.go`:** **DELETE the false comment** at `comp01.go:44-47`. "the existing book is still
   fully checked" is untrue and is what made this survive review. Replace it with a short, true
   statement: the gate refuses an unpriced order, and wiring a reference-price source is what will
   admit market orders again (name the follow-up task ID: COMP-M2).
3. **`bridge.go`:** `gateReason` (`bridge.go:134-139`) currently falls through to
   `"pre-trade compliance breach"` for any decision without violations. That is now actively
   misleading — **nothing breached**. Handle `Unpriced` with its own reason.
4. **`bridge.go`:** while you are in `gateReason` — it has the **same pre-existing defect for
   `Ungoverned`** (an ungoverned portfolio is reported as a "pre-trade compliance breach"). Fix
   that too: it is one line, it is the identical principle, and leaving it is knowingly shipping
   the bug you were sent to remove. Do not go further than these two states.

**Tests:**
- `comp01`: an `Unpriced` decision → `Breach{Code: "PRICE_UNAVAILABLE"}`, not an error, and not a
  rule violation.
- `comp01`: an unpriced MARKET `SubmitOrder` end-to-end through `Check` → the `PRICE_UNAVAILABLE`
  breach.
- `bridge`: an instrument **absent from the `prices` map** → the order lands in `Rejected` with a
  reason that does **not** claim a breach.
- `bridge`: an `Ungoverned` decision → a reason naming the missing mandate, not a breach.

## Task 3 — guard the collapse that caused this

**File:** `kanz/internal/compliance/` (unit test; NOT `test/arch` — this is a package invariant,
not a repo-topology one).

**Why:** the bug was `heldPositions` treating "price unknown" as "position is flat". Task 1 stops
unpriced orders reaching it, but nothing states the invariant that made the bypass possible, and
a future caller could reintroduce a nil price by another door (the bridge already proves a second
door exists).

**Requirements:**
1. A test asserting the invariant directly: **no `Candidate` reaching the rule engine may contain
   a position whose `MarketValue` is zero while its `Quantity` is non-zero.** That combination is
   precisely "we do not know what this is worth" masquerading as "this is flat", and it must be
   unreachable via `Evaluate`.
2. Drive it through `Evaluate` (nil price, zero price, negative price, and a legitimate
   sell-to-flat which **must still be allowed** — a genuinely flat position IS zero-value and is
   correctly skipped; do not break that).
3. Non-vacuous: if the test observes zero candidates, it FAILS. A guard that inspects nothing
   passes for the wrong reason — this repo has shipped that twice.
4. Add a short comment on `heldPositions` recording WHY zero-value means flat and may not be
   allowed to mean unknown, pointing at the `gate.go:167-173` EXEC-M14 precedent. This is the one
   comment worth writing here: it is the load-bearing constraint that is invisible in the code.

**Verification (must be EXECUTED, output pasted):**
- `go test ./internal/compliance/... ./services/oms/... ./services/optimization/... -count=1`
- Full suite: `go test ./... -count=1` from `kanz/` (expect ~156 packages, 0 failures).
- `gofmt -l .` and `go vet ./...` clean.
- **Prove the regression test fails without the fix**: stash Task 1's refusal, show the test
  failing, restore. Use a targeted edit — **never `git checkout -- .`** (it has destroyed
  uncommitted work in this repo twice).

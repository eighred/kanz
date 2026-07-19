# The refusal contract for unrepresentable amounts

**Date:** 2026-07-19
**Status:** design approved, pending implementation plan
**Board rows:** `ratFromDecimal` hangs the compliance gate (OPEN, verified live) · `dec.ToProto` wraps silently across ~40 call sites (OPEN)

## Context

Two board rows were filed as separate defects. They are one root cause.

`common.v1.Decimal` is `coefficient × 10^exponent` — a `sint64` and a `sint32`. Its
own proto comment states *"The representation is not canonical"* and **defines no
valid domain**. Every consumer therefore assumes one, and nothing enforces it:

- **`internal/compliance/engine.go:256` `ratFromDecimal`** assumes `10^|exponent|`
  is computable. Verified by execution: `ratFromDecimal({Coefficient:0, Exponent:2000000000})`
  **does not return in 5 seconds.** Reachable today by submitting an order in an
  unheld instrument with that quantity. `Quantity.Exponent` is an unvalidated wire
  field and the gate runs *before* `Accept` validation, so nothing upstream stops it.
- **`internal/dec/dec.go` `ToProto`** assumes the value fits an `int64` coefficient
  at scale −8, and silently **wraps** when it does not. At scale −8 that ceiling is
  roughly **$92 billion** — implausible for most funds, not absurd for a platform
  aiming at Aladdin-class books.

The hang is pre-existing: `engine.go` is byte-identical before the `addDecimal`
work (`git show 0450d62`). The old `pow10` loop hung on the same input one layer
up, so that work removed one of two hang sites rather than creating this one.

## Scope

**In:**
1. A domain bound enforced once, at `PreTradeGate.Evaluate` — the trust boundary
   where wire-derived values enter the rules engine.
2. `dec.ToProtoScaled`, one safe conversion that preserves magnitude, plus
   migration of the ~10 capital-path callers and an arch guard against regression.

**Out, and recorded rather than assumed away:**
- **Bus-wide envelope validation.** Enforcing the bound on every `Decimal` in every
  payload on the spine needs a reflection pass over arbitrary protos, with its own
  performance question on the estate's highest-volume subjects. The gate is where
  the verified hang is. Recorded as its own question.
- **The ~30 non-capital `ToProto` callers** (reporting, analytics). Migrating them
  has no correctness payoff and would triple the diff.
- **Bounding the exponent in the proto itself.** That is the most correct long-term
  answer — the type would finally define its own domain — but `common.v1` is
  high-blast-radius and CODEOWNERS requires architecture review. It is a lead
  decision, and it would not fix the live hang today.

## Design

### 1. The bound: `|exponent| ≤ 64`, a safety limit and explicitly not a policy

Genuine financial values keep `|exponent|` well under 30: the smallest real crypto
prices sit near 1e-12, the largest plausible notionals near 1e13. The hang needs
roughly 1e9. 64 sits far above everything real and far below anything slow.

**It must survive the system's own arithmetic**, which is the constraint that sets
the number. `mulDecimal` sums exponents and rescales upward, so values bounded at
±64 on entry reach at most ≈ ±168 internally before `ratFromDecimal` sees them —
and `10^168` is instant. A bound that only considered entry values would be wrong.

This is deliberately **not** a statement about what money means on this platform.
A tighter, meaningful domain (±30, say) is a *policy* bound, and setting policy
wrong refuses live orders — a trading outage, which is its own kind of incident.
That question is recorded for the lead with the value ranges attached. This change
buys safety without quietly deciding business rules as a side effect of a bug fix.

### 2. Where it is enforced

At the **top of `PreTradeGate.Evaluate` (`gate.go:249`)**, before the mandate
lookup. Out-of-domain refuses through the **existing** `Decision.Unvaluable` path
— no new decision flag, no new breach code.

The fields it must cover, enumerated so none is missed:

| source | fields |
|---|---|
| `OrderDelta` (`gate.go:100-101`) | `SignedQuantity`, `Price` |
| `Book` | `NAV.Amount` |
| `Book.Positions[]` | `Quantity`, `MarketValue.Amount` |

A `nil` `Decimal` is in-domain (it means absent, and the existing unpriced check
already owns that case). Zero is in-domain at any exponent — but note the exponent
is still checked, because `{0, 2e9}` is precisely the verified hang.

Inside that boundary every `Decimal` is known-sane, so `ratFromDecimal`, all seven
of its callers, and the rules engine need **no signature change**. That is the
whole reason for choosing the boundary over the conversion function: rules return
violations, not errors, and there is no error channel to thread through them.

Two consequences worth stating rather than discovering later:

- **The check runs before the ungoverned branch.** An out-of-domain order against a
  portfolio with no mandate is refused rather than admitted-ungoverned. That is
  correct — this is malformed input, not a compliance question — but it does change
  behaviour on that path, so it is deliberate rather than incidental.
- **The book is validated too, not just the order.** The book comes from the
  position read model, which is built from venue fills, so it is externally
  influenced. Validating only the order would leave the same hang reachable through
  a corrupted position.

### 3. One safe conversion, replacing a wrapping one

```go
// ToProtoScaled converts an exact rational to a Decimal, preserving MAGNITUDE.
func ToProtoScaled(r *big.Rat) (*commonpb.Decimal, bool)
```

Semantics mirror `mulDecimal`, which is already proven in this codebase: emit at
scale −8 when it fits; otherwise raise the exponent (half-up, away from zero) until
the coefficient fits an `int64`; refuse only when the exponent itself cannot move.

**Why rescale rather than refuse.** You do not need eight decimal places on $100
billion. Refusing a large-but-real value turns it into a failed operation, which on
a capital path means a refused order — quieter than a hang and just as much an
incident. Rescaling keeps the number correct at the precision that matters.

**This revises a decision made earlier today, and retires what it added.** COMP-M2
gave `comp01.go` the existing `ToProtoExact`, which refuses; under that contract a
$100bn mark refuses the order instead of pricing it. `comp01` moves to
`ToProtoScaled` — and `comp01.go:155` is `ToProtoExact`'s **only caller in the
repository** (verified), so `ToProtoExact` becomes dead and is **deleted**.

Adding a third conversion and leaving a dead second one would be the API sprawl
this design exists to avoid. The end state is two functions with one clear rule:
`ToProto` (legacy, wrapping, documented unsafe for capital paths) and
`ToProtoScaled` (safe). `ToProtoExact`'s genuinely useful test — that its output
agrees with `ToProto` wherever both are representable, which is what protects
`ToProto`'s ~30 remaining callers from a shared-helper regression — is retargeted
to `ToProtoScaled` rather than deleted with it.

`ToProto` keeps its signature and its wrapping behaviour — ~30 non-capital callers
depend on it and this change does not audit them — but is documented as unsafe for
capital paths, and an arch test prevents new capital-path code from reaching for it.

### 4. What must not regress

- An order with a normal exponent is still **admitted**. Every guard here fails
  closed, so a bound that refused everything would satisfy all the negative tests.
- The rules engine, `ratFromDecimal`, and its seven callers are **unchanged**.
- `ToProto`'s existing behaviour is **bit-identical** for its remaining callers.
- No floats anywhere.

## Testing

- **The hang repro terminates:** an order carrying `{0, 2000000000}` is refused
  promptly, not hung. Guard it structurally, not with a timeout — a timeout bounds
  the test, not the process, and Go cannot cancel the goroutine.
- **Boundary:** `|exponent| = 64` admits, `65` refuses, in both signs.
- **The book path:** a corrupted book position with an out-of-domain exponent is
  refused, proving validation covers more than the order.
- **Ungoverned interaction:** an out-of-domain order against an ungoverned portfolio
  is refused, pinning the deliberate behaviour change.
- **Rescaling:** a value near $100bn produces the correct magnitude at a coarser
  exponent — not a wrap, not a refusal.
- **Non-vacuity, the one that matters:** a normal order is still admitted, and a
  normal value still converts at scale −8 unchanged.

## Risks

- **The bound is a new refusal on the capital path.** It cannot reject anything
  financially real at ±64, but it is a new way for an order to be refused, and the
  ungoverned-path change is a genuine behaviour change.
- **`ToProtoScaled` silently loses precision** where `ToProto` silently wrapped.
  That is a strict improvement in correctness and still a silent change; the
  rescale threshold is far above any value the platform has seen.
- **~30 `ToProto` callers keep the wrap.** Documented and guarded against growth,
  not fixed.

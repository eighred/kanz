# The Refusal Contract — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a crafted order from hanging the compliance gate, and give capital-path producers one conversion that preserves magnitude instead of silently wrapping.

**Architecture:** `common.v1.Decimal` defines no valid domain, so consumers assume one and nothing enforces it. Bound the domain **once** at the trust boundary (`PreTradeGate.Evaluate`), so every `Decimal` inside the rules engine is known-sane and `ratFromDecimal`, its seven callers, and the rules need no signature change. Separately, replace the wrapping `dec.ToProto` on capital paths with a rescaling conversion mirroring the already-proven `mulDecimal`.

**Tech Stack:** Go 1.26, `math/big`, protobuf `common.v1.Decimal` (sint64 coefficient, sint32 exponent).

## Global Constraints

- **No floats.** `*big.Int`/`*big.Rat` internally, `*commonpb.Decimal` at boundaries.
- **Fail closed, but do not refuse anything real.** The bound is a SAFETY limit, not a policy statement about valid money.
- **Refuse through the EXISTING path.** Out-of-domain input yields `Decision{Allowed: false, Unvaluable: true}` — no new decision flag, no new breach code, no new error type.
- **The rules engine is untouched.** `ratFromDecimal`, its seven callers, and every rule keep their current signatures. That is the entire reason the bound sits at the boundary.
- **`ToProto` keeps its signature and its wrapping behaviour** — ~30 non-capital callers depend on it and this work does not audit them.
- Build/test from `kanz/` with `GOFLAGS=-mod=mod`. Full suite with `-p 1`. Currently **159 test packages ok, 0 failures**.
- TDD: every test watched failing before the implementation.
- Do NOT wire `bus.WithRetry` anywhere (`test/arch/bus_dlq_test.go` fails the build).

---

### Task 1: Bound the domain at the trust boundary

**Files:**
- Modify: `kanz/internal/compliance/gate.go` (add the helper; call it in `Evaluate`, which begins at `:249`)
- Test: `kanz/internal/compliance/domain_test.go` (create)

**Interfaces:**
- Consumes: existing `Decision{Allowed, Unvaluable}`, `(*PreTradeGate).noteUnvaluable(portfolioID, instrumentID string)`.
- Produces: `maxDecimalExponent` constant; `decimalInDomain(d *commonpb.Decimal) bool`; `deltaInDomain(d OrderDelta) bool`; `bookInDomain(b *Book) bool`.

**The live defect.** `internal/compliance/engine.go:256` `ratFromDecimal` computes `10^abs(exponent)` with no bound. Verified: `ratFromDecimal({Coefficient:0, Exponent:2000000000})` **does not return in 5 seconds**. Reachable today by submitting an order in an unheld instrument with that quantity — `Quantity.Exponent` is an unvalidated wire field and the gate runs *before* `Accept` validation.

**Why 64.** Real financial values keep `|exponent|` under 30 (smallest crypto prices ≈ 1e-12, largest plausible notionals ≈ 1e13). The hang needs ≈ 1e9. The number is set by the system's own arithmetic, not by entry values: `mulDecimal` sums exponents and rescales upward, so entry at ±64 reaches ≈ ±168 internally before `ratFromDecimal` sees it, and `10^168` is instant.

- [ ] **Step 1: Write the failing tests**

Create `kanz/internal/compliance/domain_test.go`:

```go
package compliance

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

// domainGate builds a governed gate over a book holding $100,000 of AAPL, so an
// order reaches the rules rather than being short-circuited as ungoverned.
func domainGate(t *testing.T) *PreTradeGate {
	t.Helper()
	reg := NewMandateRegistry()
	reg.Put(mandate(concentrationRule(60)))
	books := MapBookSource{"p1": &Book{
		PortfolioID:  "p1",
		BaseCurrency: "USD",
		NAV:          &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
		Positions: []Position{{
			InstrumentID: "AAPL",
			Quantity:     dec(1000, 0),
			MarketValue:  &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
		}},
	}}
	return NewPreTradeGate(nil, books, reg, nil, nil, nil)
}

func domainDelta(qty *commonpb.Decimal) OrderDelta {
	return OrderDelta{
		PortfolioID:    "p1",
		InstrumentID:   "AAPL",
		SignedQuantity: qty,
		Price:          dec(100, 0),
		Currency:       "USD",
		OrderID:        "o1",
		AsOf:           t0,
	}
}

// THE LIVE HANG. An order in an unheld instrument carrying {0, 2e9} made
// ratFromDecimal materialise 10^2000000000. This must refuse, promptly.
//
// Guarded STRUCTURALLY rather than with a timeout: a timeout bounds the test,
// not the process, and Go cannot cancel the goroutine that would still be
// grinding a multi-billion-digit bignum. If the bound regresses, the assertion
// below never returns and CI fails on its own panic timeout — which is loud,
// but the real protection is that decimalInDomain is O(1) by construction.
func TestEvaluate_OutOfDomainExponentIsRefused(t *testing.T) {
	g := domainGate(t)
	d, err := g.Evaluate(context.Background(), domainDelta(&commonpb.Decimal{
		Coefficient: 0, Exponent: 2000000000,
	}))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if d.Allowed {
		t.Fatal("an order carrying exponent 2e9 was ADMITTED")
	}
	if !d.Unvaluable {
		t.Fatalf("want Unvaluable, got %+v — out-of-domain input must refuse through "+
			"the existing path, not invent a new one", d)
	}
}

func TestEvaluate_DomainBoundaryInBothSigns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exp     int32
		refused bool
	}{
		{"at the positive bound", 64, false},
		{"past the positive bound", 65, true},
		{"at the negative bound", -64, false},
		{"past the negative bound", -65, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := domainGate(t)
			d, err := g.Evaluate(context.Background(), domainDelta(&commonpb.Decimal{
				Coefficient: 1, Exponent: tc.exp,
			}))
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if d.Unvaluable != tc.refused {
				t.Fatalf("exponent %d: Unvaluable = %v, want %v", tc.exp, d.Unvaluable, tc.refused)
			}
		})
	}
}

// The book is validated too. It is built from venue fills, so it is externally
// influenced; validating only the order would leave the same hang reachable
// through a corrupted position.
func TestEvaluate_OutOfDomainBookPositionIsRefused(t *testing.T) {
	reg := NewMandateRegistry()
	reg.Put(mandate(concentrationRule(60)))
	books := MapBookSource{"p1": &Book{
		PortfolioID:  "p1",
		BaseCurrency: "USD",
		NAV:          &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
		Positions: []Position{{
			InstrumentID: "MSFT",
			Quantity:     &commonpb.Decimal{Coefficient: 1, Exponent: 2000000000},
			MarketValue:  &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
		}},
	}}
	g := NewPreTradeGate(nil, books, reg, nil, nil, nil)

	d, err := g.Evaluate(context.Background(), domainDelta(dec(10, 0)))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !d.Unvaluable {
		t.Fatalf("a book position carrying exponent 2e9 was not refused: %+v", d)
	}
}

// The check runs BEFORE the ungoverned branch, so malformed input is refused
// even where no mandate governs the portfolio. That is a deliberate behaviour
// change on that path: this is input validation, not a compliance question.
func TestEvaluate_OutOfDomainRefusedEvenWhenUngoverned(t *testing.T) {
	g := NewPreTradeGate(nil, MapBookSource{}, NewMandateRegistry(), nil, nil, nil)
	d, err := g.Evaluate(context.Background(), domainDelta(&commonpb.Decimal{
		Coefficient: 1, Exponent: 2000000000,
	}))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if d.Allowed {
		t.Fatal("an out-of-domain order was admitted as ungoverned — the domain check " +
			"must run before the mandate lookup")
	}
}

// NON-VACUITY, and the most important test here. Every guard in this change
// fails closed, so a bound that refused EVERYTHING would satisfy all four tests
// above. A normal order must still reach the rules and be admitted.
func TestEvaluate_NormalOrderStillAdmitted(t *testing.T) {
	g := domainGate(t)
	d, err := g.Evaluate(context.Background(), domainDelta(dec(10, 0)))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if d.Unvaluable {
		t.Fatal("a normal 10-share order was refused as unvaluable — the bound is " +
			"rejecting real orders")
	}
	if !d.Allowed {
		t.Fatalf("a normal 10-share order was not admitted: %+v", d)
	}
}

// A nil Decimal is in-domain: absent is not out-of-range, and the existing
// unpriced check already owns that case.
func TestDecimalInDomain_NilIsInDomain(t *testing.T) {
	if !decimalInDomain(nil) {
		t.Fatal("nil must be in-domain — absence is the unpriced check's business")
	}
}

// Zero is in-domain at a sane exponent, but its EXPONENT is still checked.
// {0, 2e9} is precisely the verified hang, so "it's zero, skip it" is the
// shortcut that would reintroduce it.
func TestDecimalInDomain_ZeroIsCheckedByExponent(t *testing.T) {
	if !decimalInDomain(&commonpb.Decimal{Coefficient: 0, Exponent: 0}) {
		t.Fatal("plain zero must be in-domain")
	}
	if decimalInDomain(&commonpb.Decimal{Coefficient: 0, Exponent: 2000000000}) {
		t.Fatal("zero with an absurd exponent must be OUT of domain — a zero " +
			"coefficient does not make 10^2e9 cheap to compute")
	}
}

var _ = time.Second // keep the time import if unused after edits
```

Read `engine_test.go` first for the existing `mandate(...)`, `dec(coeff, exp)`, `t0` and `MapBookSource` helpers and reuse them. If the concentration rule helper is named differently than `concentrationRule(60)`, use the real one — `gate_test.go` and `arithmetic_sweep_test.go` both build one.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/compliance/ -run 'TestEvaluate_OutOfDomain|TestEvaluate_Domain|TestDecimalInDomain|TestEvaluate_NormalOrder' -v -count=1`

Expected: compile failure (`undefined: decimalInDomain`). Add the helper signature only, re-run, then expect `TestEvaluate_OutOfDomainExponentIsRefused` and `TestEvaluate_OutOfDomainBookPositionIsRefused` to **hang** — that is the defect, and it is what you are fixing. Kill the run (`Ctrl-C` or a short `-timeout 30s`) and record that it hung. Do NOT leave a hanging run in CI.

- [ ] **Step 3: Write the implementation**

Add to `kanz/internal/compliance/gate.go`:

```go
// maxDecimalExponent bounds the exponent of any Decimal entering the rules
// engine.
//
// It is a SAFETY limit, not a statement about what money means on this platform.
// Real financial values keep |exponent| well under 30 — the smallest crypto
// prices sit near 1e-12, the largest plausible notionals near 1e13 — so nothing
// legitimate is within thirty orders of magnitude of this bound, and it cannot
// refuse a real order. A tighter, meaningful domain would be a POLICY bound, and
// setting policy wrong refuses live orders, which is its own kind of incident.
//
// The number is set by the system's own arithmetic rather than by entry values:
// mulDecimal sums exponents and rescales upward, so values bounded at ±64 on
// entry reach at most ≈ ±168 internally before ratFromDecimal sees them, and
// 10^168 is instant. A bound chosen only against entry values would be wrong.
//
// Why this exists at all: engine.go's ratFromDecimal materialises
// 10^abs(exponent) with no bound, and Decimal.exponent is an unvalidated wire
// field on an order that reaches this gate BEFORE Accept validates it. An order
// carrying {0, 2000000000} did not return in 5 seconds.
const maxDecimalExponent = 64

// decimalInDomain reports whether a Decimal can be safely computed with.
// nil is in-domain — absent is not out-of-range, and the unpriced check owns it.
func decimalInDomain(d *commonpb.Decimal) bool {
	if d == nil {
		return true
	}
	exp := d.GetExponent()
	return exp >= -maxDecimalExponent && exp <= maxDecimalExponent
}

// deltaInDomain reports whether every Decimal on an incoming order is in-domain.
func deltaInDomain(d OrderDelta) bool {
	return decimalInDomain(d.SignedQuantity) && decimalInDomain(d.Price)
}

// bookInDomain reports whether every Decimal on a loaded book is in-domain.
// The book is built from venue fills, so it is externally influenced too;
// validating only the order would leave the same hang reachable through a
// corrupted position.
func bookInDomain(b *Book) bool {
	if b == nil {
		return true
	}
	if b.NAV != nil && !decimalInDomain(b.NAV.GetAmount()) {
		return false
	}
	for i := range b.Positions {
		if !decimalInDomain(b.Positions[i].Quantity) {
			return false
		}
		if b.Positions[i].MarketValue != nil && !decimalInDomain(b.Positions[i].MarketValue.GetAmount()) {
			return false
		}
	}
	return true
}
```

Then wire both checks into `Evaluate`. The order check goes **first**, above the mandate lookup at `gate.go:250`:

```go
func (g *PreTradeGate) Evaluate(ctx context.Context, d OrderDelta) (Decision, error) {
	// INPUT VALIDATION, BEFORE ANYTHING COMPUTES WITH THESE NUMBERS — including
	// before the mandate lookup, so an out-of-domain order against an UNGOVERNED
	// portfolio is refused rather than admitted. That is deliberate: a malformed
	// exponent is not a compliance question.
	if !deltaInDomain(d) {
		g.noteUnvaluable(d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unvaluable: true}, nil
	}
	mandate, ok, err := g.mandates.Mandate(ctx, d.PortfolioID, d.AsOf)
	...
```

and the book check immediately after the book is loaded (find `book, err := g.books.Book(ctx, d.PortfolioID)`):

```go
	book, err := g.books.Book(ctx, d.PortfolioID)
	if err != nil {
		return Decision{}, err
	}
	if !bookInDomain(book) {
		g.noteUnvaluable(d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unvaluable: true}, nil
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/compliance/ -v -count=1`
Expected: PASS — the eight new tests plus every pre-existing test in the package. Nothing should hang.

- [ ] **Step 5: Prove the bound is load-bearing and not over-broad**

Temporarily raise `maxDecimalExponent` to `2000000001` and re-run: the two hang tests must hang again (kill with `-timeout 30s`). Restore. Then temporarily lower it to `1` and re-run: `TestEvaluate_NormalOrderStillAdmitted` must FAIL. Restore, confirm green. The first mutation proves the bound does something; the second proves it does not do too much.

- [ ] **Step 6: Verify the shared package did not shift**

Run: `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./... && GOFLAGS=-mod=mod go test -p 1 ./...`
Expected: build/vet clean; 159 test packages ok, 0 failures.

- [ ] **Step 7: Commit**

```bash
cd kanz && gofmt -w internal/compliance/
git add kanz/internal/compliance/
git commit -m "fix(compliance): bound the Decimal domain so a crafted order cannot hang the gate"
```

---

### Task 2: One safe conversion, replacing the exact-or-refuse one

**Files:**
- Modify: `kanz/internal/dec/dec.go` (add `ToProtoScaled`, delete `ToProtoExact`), `kanz/internal/dec/dec_test.go` (retarget its tests), `kanz/services/oms/internal/compliance/comp01.go:148-159`
- Test: `kanz/internal/dec/dec_test.go`

**Interfaces:**
- Consumes: existing unexported `scaledCoefficient(r *big.Rat) *big.Int` and `const scale = 8`.
- Produces: `dec.ToProtoScaled(r *big.Rat) (*commonpb.Decimal, bool)`. `dec.ToProtoExact` is **removed**.

**Why.** `ToProto` emits at scale −8 and wraps when the coefficient exceeds an int64 — a ceiling of roughly **$92 billion**. `ToProtoExact` refuses instead, which turns a large-but-real value into a failed operation; on a capital path that is a refused order. You do not need eight decimal places on $100bn. Rescaling keeps the magnitude and drops precision that does not matter at that scale, mirroring `mulDecimal`, which is already proven in this codebase.

`comp01.go:155` is `ToProtoExact`'s **only caller in the repository** (verified), so once it moves, `ToProtoExact` is dead and goes with it. The end state is two functions with one clear rule: `ToProto` (legacy, wrapping, unsafe on capital paths) and `ToProtoScaled` (safe).

- [ ] **Step 1: Write the failing tests**

In `kanz/internal/dec/dec_test.go`, add:

```go
func TestToProtoScaled_NormalValueIsUnchangedAtScale8(t *testing.T) {
	got, ok := ToProtoScaled(big.NewRat(12345, 100)) // 123.45
	if !ok {
		t.Fatal("ok = false for a value well inside int64")
	}
	if got.GetExponent() != -8 || got.GetCoefficient() != 12345000000 {
		t.Fatalf("got %d e%d, want 12345000000 e-8", got.GetCoefficient(), got.GetExponent())
	}
}

// The $92bn ceiling: at scale -8 this coefficient does not fit an int64.
// ToProto WRAPS here; ToProtoScaled must keep the magnitude instead.
func TestToProtoScaled_LargeValueRescalesInsteadOfWrapping(t *testing.T) {
	v := new(big.Rat).SetInt64(100_000_000_000) // $100bn
	got, ok := ToProtoScaled(v)
	if !ok {
		t.Fatal("ok = false for $100bn — a real institutional position must not refuse")
	}
	if got.GetCoefficient() < 0 {
		t.Fatalf("negative coefficient %d — it wrapped", got.GetCoefficient())
	}
	// Reconstruct and compare: must be within one unit of the last kept digit.
	back := new(big.Rat).SetInt64(got.GetCoefficient())
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-got.GetExponent())), nil)
	back.Quo(back, new(big.Rat).SetInt(pow))
	diff := new(big.Rat).Sub(back, v)
	if diff.Abs(diff).Cmp(big.NewRat(1, 1)) > 0 {
		t.Fatalf("reconstructed %s differs from %s by more than 1", back.FloatString(2), v.FloatString(2))
	}
}

func TestToProtoScaled_NegativeLargeValueKeepsItsSign(t *testing.T) {
	got, ok := ToProtoScaled(new(big.Rat).SetInt64(-100_000_000_000))
	if !ok {
		t.Fatal("ok = false for -$100bn")
	}
	if got.GetCoefficient() >= 0 {
		t.Fatalf("coefficient %d is not negative — the sign was lost", got.GetCoefficient())
	}
}

// NON-VACUITY: ToProtoScaled must agree with ToProto wherever BOTH are
// representable. This is the test retargeted from the deleted ToProtoExact —
// it is what protects ToProto's ~30 remaining callers from a regression in the
// shared scaledCoefficient helper.
func TestToProtoScaled_AgreesWithToProtoWhereBothFit(t *testing.T) {
	for _, r := range []*big.Rat{
		big.NewRat(0, 1), big.NewRat(1, 1), big.NewRat(-1, 1),
		big.NewRat(12345, 100), big.NewRat(-12345, 100),
		big.NewRat(1, 3), big.NewRat(-1, 3),
		big.NewRat(999999999, 1),
	} {
		want := ToProto(r)
		got, ok := ToProtoScaled(r)
		if !ok {
			t.Fatalf("%s: ToProtoScaled refused a representable value", r.FloatString(4))
		}
		if got.GetCoefficient() != want.GetCoefficient() || got.GetExponent() != want.GetExponent() {
			t.Fatalf("%s: ToProtoScaled = %d e%d, ToProto = %d e%d — they must agree where both fit",
				r.FloatString(4), got.GetCoefficient(), got.GetExponent(),
				want.GetCoefficient(), want.GetExponent())
		}
	}
}

func TestToProtoScaled_NilIsZero(t *testing.T) {
	got, ok := ToProtoScaled(nil)
	if !ok || got.GetCoefficient() != 0 {
		t.Fatalf("got %v (ok=%v), want zero", got, ok)
	}
}
```

Delete the `TestToProtoExact_*` tests in the same file — their subject is being removed. The agreement test above replaces `TestToProtoExact_AgreesWithToProto`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/dec/ -v -count=1`
Expected: compile failure — `undefined: ToProtoScaled`.

- [ ] **Step 3: Write the implementation**

In `kanz/internal/dec/dec.go`, add:

```go
// ToProtoScaled converts an exact rational to a Decimal, preserving MAGNITUDE.
//
// It emits at the fixed scale when the coefficient fits an int64; otherwise it
// raises the exponent (half-up, away from zero) until it does, and refuses only
// when the exponent itself cannot move. This is the same shape as
// internal/compliance's mulDecimal, deliberately: it is the same question asked
// of a different operator.
//
// Use this on any capital path. ToProto wraps at roughly $92bn at scale -8, and
// a wrapped coefficient is a fabricated number the system will then act on. The
// alternative — refusing a large-but-real value — turns it into a failed
// operation, which on an order path is a refused trade. You do not need eight
// decimal places on $100bn; you do need the magnitude to be right.
func ToProtoScaled(r *big.Rat) (*commonpb.Decimal, bool) {
	if r == nil {
		return &commonpb.Decimal{}, true
	}
	q := scaledCoefficient(r)
	exp := int64(-scale)
	ten, five := big.NewInt(10), big.NewInt(5)
	rem := new(big.Int)
	for !q.IsInt64() {
		if exp >= math.MaxInt32 {
			return nil, false // cannot raise the exponent any further
		}
		q.QuoRem(q, ten, rem)
		if rem.CmpAbs(five) >= 0 { // half-up, away from zero
			if rem.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
		exp++
	}
	return &commonpb.Decimal{Coefficient: q.Int64(), Exponent: int32(exp)}, true
}
```

Add `"math"` to the imports if absent. **Delete `ToProtoExact` entirely.** Update `ToProto`'s doc comment so it no longer points at `ToProtoExact` — it should now say that callers needing safety use `ToProtoScaled`, and that `ToProto` retains its wrapping behaviour for its remaining non-capital callers.

Then change `kanz/services/oms/internal/compliance/comp01.go:148-159`:

```go
	if m == nil {
		return nil
	}
	// A mark too large for the fixed scale is RESCALED, not refused: the
	// magnitude is what the gate values the order on, and eight decimal places
	// on a very large price buy nothing. Refusing here would turn a real price
	// into a refused order. Only a value that cannot be represented at ANY
	// exponent yields nil, which the gate already treats as Unpriced.
	d, ok := dec.ToProtoScaled(m)
	if !ok {
		return nil
	}
	return d
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/dec/ ./services/oms/internal/compliance/ -v -count=1`
Expected: PASS. The OMS compliance package currently has 16 tests; all must still pass — `comp01`'s behaviour is unchanged for every value that was representable before.

- [ ] **Step 5: Confirm `ToProtoExact` is gone and nothing references it**

Run: `cd kanz && grep -rn "ToProtoExact" --include=*.go . ; GOFLAGS=-mod=mod go build ./...`
Expected: no grep output, build clean.

- [ ] **Step 6: Commit**

```bash
cd kanz && gofmt -w internal/dec/ services/oms/internal/compliance/
git add kanz/internal/dec/ kanz/services/oms/internal/compliance/
git commit -m "feat(dec): one rescaling conversion for capital paths; retire ToProtoExact"
```

---

### Task 3: Migrate the capital-path callers and guard against regression

**Files:**
- Modify: `kanz/services/oms/internal/order/aggregate.go:157,158,159,208`
- Test: `kanz/test/arch/decimal_conversion_test.go` (create)

**Interfaces:**
- Consumes: `dec.ToProtoScaled(r *big.Rat) (*commonpb.Decimal, bool)` from Task 2.
- Produces: nothing.

**Scope, and why it is drawn here.** The capital-path call sites split cleanly in two, and only one half is mechanical:

| site | enclosing function | migratable? |
|---|---|---|
| `order/aggregate.go:157,158,159` | `ApplyFill(...) (*orderpb.OrderState, error)` | **yes** — error path exists |
| `order/aggregate.go:208` | `Amend(...) (*orderpb.OrderState, error)` | **yes** — error path exists |
| `position/book.go:123,124,185,215,216` | `stateOf`, `money` — **no error return** | no |
| `position/postgres.go:314,315,343,344,353` | same shape | no |

Migrating `book.go` means threading an error through `money` → `stateOf` → `Snapshot`, three signatures, in code that currently cannot fail. That is a refactor with its own design question, and doing it inside this task would be designing in the wrong document — the same reason `dec.ToProto` was split out of the previous plan.

So: **migrate the four sites that have an error path, and declare the ten that do not** in the guard's exception map with a written reason. The exceptions are the worklist for a follow-up task, and the guard stops the set growing in the meantime. This mirrors `unbackedByDesign` in `archiver_topology_test.go` and `UNVERIFIED_RLS_TABLES` in the onboarding script — the repo's established way to record a known gap without silencing the check.

- [ ] **Step 1: Write the failing arch guard**

Create `kanz/test/arch/decimal_conversion_test.go`:

```go
package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// dec.ToProto WRAPS when a value does not fit an int64 coefficient at the fixed
// scale — roughly $92bn in money terms. A wrapped coefficient is a fabricated
// number, and on a capital path the system then acts on it: a position valued
// at a wrapped figure, an average fill price that is not the price anything
// filled at.
//
// dec.ToProtoScaled preserves magnitude instead. This guard keeps the capital
// paths on it. ToProto is not banned outright — ~30 reporting and analytics
// callers use it and are unaffected by the ceiling — so the rule is scoped to
// the packages where a wrong number moves money.
var capitalPathPackages = []string{
	"services/oms/internal/position",
	"services/oms/internal/order",
}

// pendingErrorThreading are capital-path call sites that still use the wrapping
// conversion because their enclosing function has NO error return, so migrating
// them means threading an error through several signatures in code that
// currently cannot fail. That is a refactor with its own design question, not a
// find-and-replace.
//
// This map is the WORKLIST, not an excuse. A site here is a known gap with a
// written reason; a site NOT here and not migrated fails the build. Removing an
// entry is how the follow-up task reports progress.
var pendingErrorThreading = map[string]string{
	"services/oms/internal/position/book.go":     "Book.money and Book.stateOf return no error; threading one reaches Snapshot through three signatures",
	"services/oms/internal/position/postgres.go": "same shape as book.go — the Postgres-backed twin of the same read model",
}

func TestCapitalPathsDoNotUseWrappingToProto(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var offenders []string
	scanned, seenPending := 0, map[string]bool{}

	for _, pkg := range capitalPathPackages {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			rel := filepath.ToSlash(mustRel(root, path))
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ToProto" {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != "dec" {
					return true
				}
				if _, pending := pendingErrorThreading[rel]; pending {
					seenPending[rel] = true
					return true
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d", rel, fset.Position(call.Pos()).Line))
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// NON-VACUITY: if the walk found no files, this guard would pass no matter
	// how many capital paths used the wrapping conversion.
	if scanned == 0 {
		t.Fatal("scanned zero Go files across the capital-path packages — the walk is broken")
	}

	// A declared exception that no longer has any dec.ToProto call is DEAD.
	// Removing it is how the follow-up task reports progress; leaving it lets a
	// future regression hide behind a stale entry.
	for file := range pendingErrorThreading {
		if !seenPending[file] {
			offenders = append(offenders, file+": declared in pendingErrorThreading but calls dec.ToProto nowhere — stale exemption, remove it")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("capital-path code calls the WRAPPING dec.ToProto:\n  %s\n\n"+
			"ToProto silently wraps above an int64 coefficient at the fixed scale (~$92bn in "+
			"money terms), and the system then acts on the fabricated number — a position "+
			"valued at a figure nothing is worth, an average price nothing filled at. Use "+
			"dec.ToProtoScaled, which preserves magnitude and reports when it cannot. If the "+
			"enclosing function has no error return, add the FILE to pendingErrorThreading "+
			"with the reason, rather than dropping the ok return.",
			strings.Join(offenders, "\n  "))
	}
}

func mustRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestCapitalPathsDoNotUseWrappingToProto -v -count=1`
Expected: FAIL, listing the call sites in `position/book.go`, `order/aggregate.go` and `position/postgres.go`. Record that list — it is the work.

- [ ] **Step 3: Migrate the four sites that have an error path**

In `kanz/services/oms/internal/order/aggregate.go`, `ApplyFill` already returns `(*orderpb.OrderState, error)`. Replace lines 157-159:

```go
	next := cloneState(st)
	filled, ok := dec.ToProtoScaled(newFilled)
	if !ok {
		return nil, fmt.Errorf("order %s: filled quantity is not representable", st.GetOrderId())
	}
	leaves, ok := dec.ToProtoScaled(newLeaves)
	if !ok {
		return nil, fmt.Errorf("order %s: leaves quantity is not representable", st.GetOrderId())
	}
	avgPx, ok := dec.ToProtoScaled(avg)
	if !ok {
		return nil, fmt.Errorf("order %s: average fill price is not representable", st.GetOrderId())
	}
	next.FilledQuantity = filled
	next.LeavesQuantity = leaves
	next.AverageFillPrice = avgPx
```

And at line 208, inside `Amend` (which returns `(*orderpb.OrderState, error)`):

```go
		remaining, ok := dec.ToProtoScaled(new(big.Rat).Sub(dec.FromProto(q), dec.FromProto(st.GetFilledQuantity())))
		if !ok {
			return nil, fmt.Errorf("order %s: amended leaves quantity is not representable", st.GetOrderId())
		}
		next.LeavesQuantity = remaining
```

Add `"fmt"` to the imports if absent.

**Never substitute zero, and never discard `ok`.** A zero quantity or market value is COMP-M1's exact defect: `heldPositions` treats a zero-valued position as flat and drops it from the compliance check, so the rules silently stop seeing it. Refusing is the only safe direction.

**Leave `book.go` and `postgres.go` alone** — they are declared in `pendingErrorThreading` and are the follow-up task. If you find that one of them *does* have a reachable error path after all, remove its entry and migrate it; the guard's stale-exemption check will tell you if you remove an entry without migrating.

- [ ] **Step 4: Run the guard and the suites**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ ./services/oms/... -v -count=1 2>&1 | tail -20`
Expected: the guard passes; the OMS suites pass.

- [ ] **Step 5: Mutation-test the guard**

Revert one migrated call site to `dec.ToProto`, confirm the guard FAILS naming that exact file and line, then restore. A guard that has never fired is indistinguishable from one that does not work.

- [ ] **Step 6: Full suite**

Run: `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./... && GOFLAGS=-mod=mod go test -p 1 ./...`
Expected: build/vet clean; 159 test packages ok, 0 failures.

- [ ] **Step 7: Commit**

```bash
cd kanz && gofmt -w services/ test/arch/
git add kanz/services/ kanz/test/arch/
git commit -m "fix(oms): capital paths use the rescaling conversion, guarded"
```

---

### Task 4: Update the board

**Files:**
- Modify: `KANZ_TASKS.md` — the `ratFromDecimal` row and the `dec.ToProto` row

- [ ] **Step 1: Close both rows**

The `ratFromDecimal` row becomes FIXED: the domain is bounded at the trust boundary (`|exponent| ≤ 64`), refusing through the existing `Unvaluable` path, with the rules engine and all seven `ratFromDecimal` callers unchanged. State that the bound is a safety limit and explicitly not a policy claim, and that the tighter domain question remains open for the lead.

The `dec.ToProto` row becomes PARTIALLY FIXED: capital paths use `ToProtoScaled` and are guarded; the ~30 non-capital callers keep the wrap deliberately, documented and guarded against growth. Note that `ToProtoExact` — added earlier the same day — was retired, because rescaling is the better contract and it had exactly one caller.

- [ ] **Step 2: Record what stayed open**

Add or update rows for: **bus-wide envelope validation** (needs a reflection pass over arbitrary protos, with a performance question on the highest-volume subjects) and **bounding the exponent in the proto itself** (the correct long-term answer; `common.v1` is high-blast-radius and CODEOWNERS requires architecture review, so it is a lead decision).

Also record the **deliberate behaviour change**: an out-of-domain order against an ungoverned portfolio is now refused rather than admitted-ungoverned.

- [ ] **Step 3: Validate the table**

Run: `cd /c/Users/root/Desktop/eighred-kanz && sh .superpowers/sdd/validate-board.sh KANZ_TASKS.md`
Expected: `board OK: N rows, 5 columns each`, exit 0. This script **exits non-zero** on a malformed row — it exists because an earlier `awk` check printed MALFORMED and exited 0, so a `&& git add` chain committed a broken row anyway. Do not replace it with an inline `awk`.

**Watch for literal `|` characters in your prose** — they split a markdown table cell. Write `10^abs(exponent)`, not `10^|exponent|`. That exact mistake broke this table earlier today.

- [ ] **Step 4: Commit**

```bash
git add KANZ_TASKS.md
git commit -m "docs(board): the Decimal domain is bounded; capital paths rescale"
```

---

## Verification (whole feature)

- [ ] `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./...` — clean.
- [ ] `cd kanz && GOFLAGS=-mod=mod go test -p 1 ./...` — 159 test packages ok, 0 failures.
- [ ] An order carrying `{0, 2000000000}` is refused promptly, not hung.
- [ ] `|exponent| = 64` admits; `65` refuses; both signs.
- [ ] A normal 10-share order is still **admitted** — the bound refuses nothing real.
- [ ] `$100bn` converts to the correct magnitude at a coarser exponent, neither wrapped nor refused.
- [ ] `grep -rn "ToProtoExact" --include=*.go kanz/` returns nothing.
- [ ] The arch guard fails when a capital-path call site is reverted to `dec.ToProto`.

## Out of scope, recorded on the board

- **Bus-wide envelope validation** — needs a reflection pass over arbitrary protos.
- **The ~30 non-capital `ToProto` callers** — keep the wrap, guarded against growth.
- **Bounding the exponent in `common.v1.Decimal`** — CODEOWNERS architecture review; would not fix the live hang today.
- **A tighter, policy-bearing domain (±30)** — refusing live orders is its own incident; lead decision, evidence attached.

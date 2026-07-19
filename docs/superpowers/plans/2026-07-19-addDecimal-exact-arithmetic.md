# Exact `addDecimal` on the Admission Path — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the projected order quantity impossible to compute wrongly — `addDecimal` currently wraps twice in unchecked `int64`, and the result feeds every compliance rule through the position's market value.

**Architecture:** Mirror the fix already applied to `mulDecimal` twelve lines below it in the same file: compute in `math/big`, rescale to a coarser exponent to preserve magnitude when the result does not fit an `int64` coefficient, and refuse only when the exponent itself cannot move. `project` already has the refusal path (`return nil, false` → `Decision.Unvaluable` → breach code `NOTIONAL_UNREPRESENTABLE`); this task routes a second failure into it.

**Tech Stack:** Go 1.26, `math/big`, protobuf `common.v1.Decimal` (int64 coefficient + int32 exponent).

## Global Constraints

- **No floats.** `*big.Int`/`*big.Rat` internally, `*commonpb.Decimal` at boundaries. Never `float64`, never a hard-coded exponent.
- **Never silently wrap.** Every arithmetic path either produces the correct magnitude or refuses. There is no third option.
- **Fail closed.** A quantity that cannot be represented refuses the order; it never admits one.
- **Bit-identical in the normal range.** Any input that does not overflow today must produce exactly the same `Decimal` after this change. This is a shared package (`internal/compliance`) used across the repo.
- Build/test from `kanz/` with `GOFLAGS=-mod=mod`. Full suite with `-p 1` (packages share one Postgres).
- TDD: every test watched failing before the implementation is written.
- Do NOT wire `bus.WithRetry` anywhere (`test/arch/bus_dlq_test.go` fails the build).

---

### Task 1: `addDecimal` computes exactly or refuses

**Files:**
- Modify: `kanz/internal/compliance/gate.go:358-372` (`addDecimal`), `:338` (its only caller, inside `project`)
- Test: `kanz/internal/compliance/gate_test.go` (add to the existing `TestMulDecimal_*` neighbourhood)

**Interfaces:**
- Consumes: nothing new.
- Produces: `addDecimal(a, b *commonpb.Decimal) (*commonpb.Decimal, bool)` — `ok == false` means the sum cannot be represented and the caller must refuse.

**The defect, exactly.** Current implementation:

```go
func addDecimal(a, b *commonpb.Decimal) *commonpb.Decimal {
	if a == nil { a = &commonpb.Decimal{} }
	if b == nil { b = &commonpb.Decimal{} }
	exp := a.GetExponent()
	if b.GetExponent() < exp { exp = b.GetExponent() }
	ac := a.GetCoefficient() * pow10(a.GetExponent()-exp)   // (1) wraps
	bc := b.GetCoefficient() * pow10(b.GetExponent()-exp)   // (1) wraps
	return &commonpb.Decimal{Coefficient: ac + bc, Exponent: exp}  // (2) wraps
}
```

**There are TWO wrap sites, not one.** `pow10` returns `int64` and overflows for a gap ≥ 19, and the alignment multiply wraps; independently, `ac + bc` can overflow even when both operands are fine. The review that found this reported only the first.

Demonstrated: `addDecimal(100e0, 1e-19)` returns `0.3875820196…` instead of ~100, and `addDecimal(100e0, 5e-25)` returns a **negative** quantity.

**Why it matters.** `gate.go:338` is the only caller: `newQty := addDecimal(existingQty, d.SignedQuantity)`. That `newQty` becomes the projected position's `Quantity` AND the left operand of `mulDecimal(newQty, d.Price)`. No rule reads `Quantity` directly — all five read `MarketValue` — so a wrapped quantity reaches every rule *through the notional*. A wrapped-small quantity yields a small notional, which under-reports exposure to the concentration and gross-leverage caps.

- [ ] **Step 1: Write the failing tests**

Add to `kanz/internal/compliance/gate_test.go`. Match the file's existing style; `TestMulDecimal_*` tests are already there and show the conventions.

```go
func TestAddDecimal_ExactInTheNormalRange(t *testing.T) {
	// Values that do not overflow must be unchanged by this fix. Aligning to the
	// smaller exponent: 100 (100e0) + 0.5 (5e-1) = 100.5 (1005e-1).
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: 100, Exponent: 0},
		&commonpb.Decimal{Coefficient: 5, Exponent: -1},
	)
	if !ok {
		t.Fatal("ok = false for a value well inside int64 — the guard is refusing normal input")
	}
	if got.GetCoefficient() != 1005 || got.GetExponent() != -1 {
		t.Fatalf("got %d e%d, want 1005 e-1", got.GetCoefficient(), got.GetExponent())
	}
}

func TestAddDecimal_NilOperandsBehaveAsZero(t *testing.T) {
	got, ok := addDecimal(nil, &commonpb.Decimal{Coefficient: 7, Exponent: 0})
	if !ok || got.GetCoefficient() != 7 || got.GetExponent() != 0 {
		t.Fatalf("got %v (ok=%v), want 7 e0 — a nil operand is zero, as before", got, ok)
	}
	if _, ok := addDecimal(nil, nil); !ok {
		t.Fatal("addDecimal(nil, nil) must succeed as zero")
	}
}

// THE ALIGNMENT WRAP. An exponent gap of 19 overflows pow10's int64.
// Before the fix this returns 0.3875820196..., a number with no relationship
// to the inputs.
func TestAddDecimal_LargeExponentGapDoesNotWrap(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: 100, Exponent: 0},
		&commonpb.Decimal{Coefficient: 1, Exponent: -19},
	)
	if !ok {
		// Refusing is acceptable here (the exact sum needs more than int64 of
		// precision); returning a WRONG number is not.
		return
	}
	// If it did represent it, the value must be ~100, not 0.38.
	f, _ := new(big.Rat).SetFrac(
		big.NewInt(got.GetCoefficient()),
		new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-got.GetExponent())), nil),
	).Float64()
	if f < 99.9 || f > 100.1 {
		t.Fatalf("got %v (%d e%d), want ~100 — the alignment multiply wrapped",
			f, got.GetCoefficient(), got.GetExponent())
	}
}

// THE SIGN FLIP. A gap of 25 wraps the alignment negative, so adding a tiny
// POSITIVE quantity to 100 yields a NEGATIVE position.
func TestAddDecimal_LargeGapNeverFlipsTheSign(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: 100, Exponent: 0},
		&commonpb.Decimal{Coefficient: 5, Exponent: -25},
	)
	if ok && got.GetCoefficient() < 0 {
		t.Fatalf("got a NEGATIVE coefficient (%d e%d) from adding two POSITIVE quantities",
			got.GetCoefficient(), got.GetExponent())
	}
}

// THE SECOND WRAP SITE, which the review did not report: both operands are
// individually fine and the SUM overflows.
func TestAddDecimal_OverflowingSumDoesNotWrap(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
	)
	if !ok {
		return // refusing is acceptable
	}
	if got.GetCoefficient() < 0 {
		t.Fatalf("MaxInt64 + MaxInt64 produced a NEGATIVE coefficient (%d e%d)",
			got.GetCoefficient(), got.GetExponent())
	}
	// Rescaled, it must still be ~1.8e19 in magnitude.
	mag := new(big.Rat).SetFrac(
		big.NewInt(got.GetCoefficient()),
		big.NewInt(1),
	)
	if got.GetExponent() > 0 {
		mag.Mul(mag, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(got.GetExponent())), nil)))
	}
	want := new(big.Rat).SetInt(new(big.Int).Mul(big.NewInt(math.MaxInt64), big.NewInt(2)))
	ratio := new(big.Rat).Quo(mag, want)
	f, _ := ratio.Float64()
	if f < 0.999 || f > 1.001 {
		t.Fatalf("magnitude %v is not ~2*MaxInt64 — the sum wrapped or was mis-rescaled", mag)
	}
}

// NON-VACUITY: sign is preserved through rescaling for genuine negatives
// (a SELL is a negative signed quantity).
func TestAddDecimal_NegativeSumKeepsItsSign(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: -math.MaxInt64, Exponent: 0},
		&commonpb.Decimal{Coefficient: -math.MaxInt64, Exponent: 0},
	)
	if ok && got.GetCoefficient() >= 0 {
		t.Fatalf("two negative quantities summed to a non-negative coefficient (%d e%d)",
			got.GetCoefficient(), got.GetExponent())
	}
}
```

Add `"math"` and `"math/big"` to the test file's imports if absent.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/compliance/ -run TestAddDecimal -v -count=1`

Expected: compile failure first (`addDecimal` returns one value, not two). Fix ONLY the call sites in the test to use the two-value form — do not change assertions. Then expect `TestAddDecimal_LargeExponentGapDoesNotWrap`, `TestAddDecimal_LargeGapNeverFlipsTheSign` and `TestAddDecimal_OverflowingSumDoesNotWrap` to FAIL with wrong/negative values. Record that output — it is the proof the tests bite.

- [ ] **Step 3: Write the implementation**

Replace `addDecimal` in `kanz/internal/compliance/gate.go`:

```go
// addDecimal sums two Decimals exactly, or refuses.
//
// It aligns to the SMALLER exponent, which means scaling one operand up by
// 10^gap — and that is where it used to wrap: pow10 returns an int64 and
// overflows at a gap of 19, so `addDecimal(100e0, 1e-19)` returned 0.3875…,
// and at a gap of 25 the alignment went NEGATIVE, turning the sum of two
// positive quantities into a negative position. The sum itself could overflow
// independently even when both aligned operands fit.
//
// This is the same failure `mulDecimal` had and is fixed the same way, because
// it is the same question: compute in math/big, rescale to a coarser exponent
// to keep the MAGNITUDE when the coefficient will not fit an int64, and refuse
// only when the exponent cannot move.
//
// The caller is project(), whose result values every compliance rule. A wrong
// quantity here is not a rounding error — it is a position the rules never see
// the true size of.
func addDecimal(a, b *commonpb.Decimal) (*commonpb.Decimal, bool) {
	if a == nil {
		a = &commonpb.Decimal{}
	}
	if b == nil {
		b = &commonpb.Decimal{}
	}
	exp := int64(a.GetExponent())
	if int64(b.GetExponent()) < exp {
		exp = int64(b.GetExponent())
	}
	ten := big.NewInt(10)
	scale := func(d *commonpb.Decimal) *big.Int {
		gap := int64(d.GetExponent()) - exp // ≥ 0 by construction
		c := big.NewInt(d.GetCoefficient())
		if gap == 0 {
			return c
		}
		return c.Mul(c, new(big.Int).Exp(ten, big.NewInt(gap), nil))
	}
	coeff := new(big.Int).Add(scale(a), scale(b))

	five := big.NewInt(5)
	rem := new(big.Int)
	for !coeff.IsInt64() || exp > math.MaxInt32 {
		if exp >= math.MaxInt32 {
			return nil, false // cannot raise the exponent any further
		}
		coeff.QuoRem(coeff, ten, rem)
		if rem.CmpAbs(five) >= 0 { // half-up, away from zero
			if rem.Sign() < 0 {
				coeff.Sub(coeff, big.NewInt(1))
			} else {
				coeff.Add(coeff, big.NewInt(1))
			}
		}
		exp++
	}
	return &commonpb.Decimal{Coefficient: coeff.Int64(), Exponent: int32(exp)}, true
}
```

Then update its only caller, `project` at `gate.go:338`:

```go
	newQty, ok := addDecimal(existingQty, d.SignedQuantity)
	if !ok {
		return nil, false
	}
	notional, ok := mulDecimal(newQty, d.Price)
	if !ok {
		return nil, false
	}
```

Both refusals land on the existing `Decision.Unvaluable` path — no new decision flag, no new breach code.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/compliance/ -v -count=1`
Expected: PASS — the six new tests plus every pre-existing test in the package.

- [ ] **Step 5: Check whether `pow10` is now dead**

Run: `cd kanz && grep -rn "pow10(" --include=*.go internal/compliance/`

`pow10` (`gate.go:428-435`) existed for `addDecimal`'s alignment. If this is now its only definition and there are no remaining callers in the package, DELETE it — an unused helper that performs the exact unchecked multiplication this task removed is a trap for the next reader. If something else still calls it, leave it and note what in your report.

- [ ] **Step 6: Verify the shared package did not shift**

Run: `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./... && GOFLAGS=-mod=mod go test -p 1 ./...`
Expected: build/vet clean; 159 test packages ok, 0 failures.

- [ ] **Step 7: Commit**

```bash
cd kanz && gofmt -w internal/compliance/
git add kanz/internal/compliance/
git commit -m "fix(compliance): sum the projected quantity exactly instead of wrapping twice"
```

---

### Task 2: prove no wrap-induced admission across ALL five rules

**Files:**
- Test: `kanz/internal/compliance/arithmetic_sweep_test.go` (create)

**Interfaces:**
- Consumes: `addDecimal(a, b) (*commonpb.Decimal, bool)` from Task 1; the existing `PreTradeGate`, `MapBookSource`, `MandateRegistry`.
- Produces: nothing.

**Why this task exists separately.** The finding was recorded on the board as "not demonstrated exploitable **via concentration**" — a 738-case sweep against the concentration rule found zero admissions, because that rule's numerator and denominator move together. That is one negative result against one of **five** rule types. The masking is incidental to how concentration normalizes, and nothing says `GROSS_LEVERAGE` (which sums absolute market values) behaves the same way.

Task 1 makes the wrap impossible. This task proves it, permanently, across the whole rule set — so the claim on the board stops being an argument and becomes a test.

- [ ] **Step 1: Write the sweep**

Create `kanz/internal/compliance/arithmetic_sweep_test.go`.

These helpers already exist in the package's tests and MUST be reused, not reinvented: `mandate(rules ...*compliancepb.Rule) *compliancepb.Mandate` and `dec(coeff int64, exp int32) *commonpb.Decimal` (both `engine_test.go`), and `t0` (the fixed test time).

```go
package compliance

import (
	"context"
	"math"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
)

// sweepBook is a portfolio holding $100,000 of AAPL, so a correctly-valued
// order in the same instrument is large relative to it and any value-based cap
// has something to bite on.
func sweepBook() *Book {
	return &Book{
		PortfolioID:  "p1",
		BaseCurrency: "USD",
		Positions: []Position{{
			InstrumentID: "AAPL",
			Quantity:     dec(1000, 0),
			MarketValue:  &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
		}},
	}
}

// EVERY rule type the engine implements. Restriction, issuer-exclusion and
// currency key off IDENTITY rather than magnitude, so a wrong quantity should
// not be able to move them — but "should not" is the assumption this test
// exists to stop relying on, which is why all five are swept and not just the
// two value-based ones.
func sweepRules() map[string]*compliancepb.Rule {
	return map[string]*compliancepb.Rule{
		"concentration": {
			RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: dec(10, -2), // 10%
			}},
		},
		"gross_leverage": {
			RuleId: "l1", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
			Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
				MaxGrossLeverage: dec(11, -1), // 1.1x
			}},
		},
		"restriction": {
			RuleId: "r1", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
			Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
				Mode: compliancepb.RestrictionMode_RESTRICTION_MODE_DENY, InstrumentIds: []string{"AAPL"},
			}},
		},
		"issuer_exclusion": {
			RuleId: "e1", Type: compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION,
			Params: &compliancepb.Rule_IssuerExclusion{IssuerExclusion: &compliancepb.IssuerExclusion{
				IssuerIds: []string{"AAPL"},
			}},
		},
		"currency": {
			RuleId: "cc1", Type: compliancepb.RuleType_RULE_TYPE_CURRENCY,
			Params: &compliancepb.Rule_Currency{Currency: &compliancepb.CurrencyPolicy{
				AllowedCurrencyCodes: []string{"JPY"}, // USD is NOT allowed
			}},
		},
	}
}

// TestArithmeticSweep_NoWrapAdmitsAnOrder drives every rule type against a
// wide range of quantity exponents and coefficients, and asserts that NOTHING
// is admitted.
//
// The board recorded this hole as "not demonstrated exploitable via
// CONCENTRATION" on the strength of a 738-case sweep against that one rule --
// whose numerator and denominator move together, which is exactly why it
// masked the defect. That was one negative result about one of five rules.
// This is the whole rule set, and it is a permanent test rather than a
// one-off script, so the claim stops being an argument.
func TestArithmeticSweep_NoWrapAdmitsAnOrder(t *testing.T) {
	ctx := context.Background()
	// Exponents straddle the gap of 19 where pow10's int64 used to overflow,
	// and -25 where the alignment used to go negative.
	exponents := []int32{0, -1, -2, -8, -17, -18, -19, -20, -25, -30, -38, -40}
	coefficients := []int64{
		1, 5, 1000,
		1 << 62, -(1 << 62),
		math.MaxInt64, -math.MaxInt64,
		1844674407, 9223372036,
	}

	for name, rule := range sweepRules() {
		t.Run(name, func(t *testing.T) {
			reg := NewMandateRegistry()
			reg.Put(mandate(rule))
			g := NewPreTradeGate(nil, MapBookSource{"p1": sweepBook()}, reg, nil, nil, nil)

			admitted := 0
			for _, exp := range exponents {
				for _, coeff := range coefficients {
					d := OrderDelta{
						PortfolioID:    "p1",
						InstrumentID:   "AAPL",
						SignedQuantity: &commonpb.Decimal{Coefficient: coeff, Exponent: exp},
						Price:          dec(100, 0),
						Currency:       "USD",
						OrderID:        "o1",
						AsOf:           t0,
					}
					dec, err := g.Evaluate(ctx, d)
					if err != nil {
						t.Fatalf("Evaluate(coeff=%d exp=%d): %v", coeff, exp, err)
					}
					if dec.Allowed {
						admitted++
						t.Errorf("ADMITTED with coeff=%d exp=%d — an order of this size "+
							"against a $100,000 book must be refused by %s or refused as "+
							"unvaluable, never allowed", coeff, exp, name)
					}
				}
			}
			if admitted > 0 {
				t.Fatalf("%s admitted %d of %d cases", name, admitted, len(exponents)*len(coefficients))
			}
		})
	}
}
```

Note the local variable `dec` shadowing the package-level `dec` helper inside the loop — rename the `Decision` variable to `verdict` if the compiler objects, and do not rename the helper.

If a rule's params struct or enum name differs from the above, correct the CONSTRUCTION to match the real proto (read `engine_test.go`, which builds all five) — but do not change the assertion or the swept ranges.

- [ ] **Step 2: Run it and confirm it passes on the FIXED code**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/compliance/ -run TestArithmeticSweep -v -count=1`
Expected: PASS.

- [ ] **Step 3: Prove the sweep can actually fail — revert Task 1 temporarily**

Restore the OLD `addDecimal` (the `pow10` version, returning a single value) and its old call site, keeping the sweep. Run it again.

Expected: the sweep FAILS for at least one rule type, naming the exponent/coefficient that was admitted. **Record that output — it is the entire value of this task.** If the sweep passes even against the wrapping implementation, the sweep is not exercising the defect: widen it (more exponents, more coefficients, a larger standing position) until it fails, then restore Task 1's code.

If, after genuinely widening it, no rule type can be made to admit, say so explicitly in your report — that is a real and useful result, and it means the board row should be softened rather than the test deleted. Do NOT quietly keep a sweep that proves nothing.

- [ ] **Step 4: Restore the fix and confirm green**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./internal/compliance/ -v -count=1`
Expected: PASS, including the sweep.

- [ ] **Step 5: Commit**

```bash
cd kanz && gofmt -w internal/compliance/
git add kanz/internal/compliance/arithmetic_sweep_test.go
git commit -m "test(compliance): sweep every rule type for wrap-induced admission"
```

---

### Task 3: update the board

**Files:**
- Modify: `KANZ_TASKS.md` — the `addDecimal` row

- [ ] **Step 1: Close the `addDecimal` row**

Record: fixed by exact `math/big` arithmetic mirroring `mulDecimal`; **two** wrap sites not one (the alignment multiply AND the sum — the review reported only the first); refusal routes through the existing `Unvaluable` path with no new decision flag; and the sweep now covers all five rule types rather than concentration alone.

State plainly whichever of these Task 2 established:
- if the sweep failed against the old code, name the rule type and inputs that admitted — the hole WAS exploitable and the board row understated it;
- if it could not be made to fail, say that the hole was real arithmetic but not reachable as an admission through any rule, and that the row's caution was correct but its severity was not.

Do not write both. Write the one that happened.

- [ ] **Step 2: Note what remains**

The `dec.ToProto` row stays open — 40 call sites, 10+ on capital paths, deliberately not touched by this task because converting them is a design question (exact-or-refuse versus rescale-preserving-magnitude, and whether a third conversion function is acceptable) that needs its own pass.

- [ ] **Step 3: Validate the table**

Run: `cd /c/Users/root/Desktop/eighred-kanz && awk '/^\| \*\*/ {n=gsub(/\|/,"|"); if (n!=6) print NR": "n" pipes"}' KANZ_TASKS.md`
Expected: no output — every row has exactly 5 columns.

- [ ] **Step 4: Commit**

```bash
git add KANZ_TASKS.md
git commit -m "docs(board): addDecimal closed; sweep now covers every rule type"
```

---

## Verification (whole feature)

- [ ] `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./...` — clean.
- [ ] `cd kanz && GOFLAGS=-mod=mod go test -p 1 ./...` — 159 test packages ok, 0 failures.
- [ ] `addDecimal(100e0, 1e-19)` no longer returns `0.3875…`; `addDecimal(100e0, 5e-25)` no longer returns a negative quantity.
- [ ] `MaxInt64 + MaxInt64` does not produce a negative coefficient.
- [ ] The sweep was demonstrated FAILING against the old implementation (or its inability to fail was reported honestly).
- [ ] `grep -rn "float64" kanz/internal/compliance/gate.go` returns nothing.

## Out of scope, deliberately

- **`dec.ToProto`'s 40 call sites.** Its wrapping contract is preserved for existing callers; converting the capital-path ones needs a design decision about conversion semantics and belongs in its own task.
- **The unbounded `market.>` subscription** and **crash-mid-work resumption** — both tracked separately on the board.

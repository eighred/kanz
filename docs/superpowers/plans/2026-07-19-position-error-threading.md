# Position Read-Model Error Threading — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the last ten capital-path call sites off the wrapping `dec.ToProto`, so a position value too large to represent refuses instead of being fabricated.

**Architecture:** `Book.money`/`Book.stateOf` and their `Postgres` twins return no error, which is why the earlier migration declared them as a worklist rather than forcing one. Both of their callers — `Apply` and `Snapshot` — already return `error`, so threading adds an error return to two unexported methods per file and changes nothing outside the package.

**Tech Stack:** Go 1.26, `math/big`, protobuf `common.v1.Decimal` / `domain.v1.PositionState`.

## Global Constraints

- **Never substitute zero, and never discard the `ok` return.** A zero quantity or market value is COMP-M1's exact defect: `heldPositions` treats a zero-valued position as flat and drops it from the compliance check, so the rules silently stop seeing it. Refusing is the only safe direction.
- No floats. `*big.Rat` internally, `*commonpb.Decimal` at boundaries.
- `dec.ToProto`'s signature and behaviour stay unchanged — ~30 non-capital callers (reporting, analytics) rely on it and this work does not audit them.
- **No signature change may escape the package.** `money` and `stateOf` are unexported; `Apply` and `Snapshot` already return `error`. If you find yourself changing an exported signature or an interface, stop and report — that means the ripple is wider than this plan assumed.
- Build/test from `kanz/` with `GOFLAGS=-mod=mod`. Full suite with `-p 1`. Currently **159 test packages ok, 0 failures**.
- Do NOT wire `bus.WithRetry` anywhere (`test/arch/bus_dlq_test.go` fails the build).

---

### Task 1: Thread the error through both position read models

**Files:**
- Modify: `kanz/services/oms/internal/position/book.go` (`stateOf` at `:117`, `money` at `:184`, call sites at `:89-90` and `:217-226`)
- Modify: `kanz/services/oms/internal/position/postgres.go` (`stateOf` at `:337`, `money` at `:352`, call sites at `:162-163` and `:316-326`)
- Modify: `kanz/test/arch/decimal_conversion_test.go` (empty `pendingErrorThreading`)

**Interfaces:**
- Consumes: `dec.ToProtoScaled(r *big.Rat) (*commonpb.Decimal, bool)`.
- Produces: `(*Book).money(r *big.Rat) (*commonpb.Money, error)`, `(*Book).stateOf(...) (*domainpb.PositionState, error)`, and the identical pair on `*Postgres`. No exported signature changes.

**The two files are the same code twice**, differing only in receiver (`b` / `p`) and two locals (`l.qty` vs `x.l.qty`, `k.instrument` vs `instrument`). Do both in this task — a reviewer would accept or reject them together.

**Completion is build-enforced.** `TestCapitalPathsDoNotUseWrappingToProto` has a stale-exemption arm: a file listed in `pendingErrorThreading` that no longer calls `dec.ToProto` **fails the build**. So emptying the map is not bookkeeping — it is how the guard confirms the migration is actually finished.

- [ ] **Step 1: Write the failing test**

Create `kanz/services/oms/internal/position/representability_test.go`. Read the package's existing tests first for how a `*Book` is constructed and reuse those helpers.

```go
package position

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

// hugeRat is large enough that dec.ToProtoScaled cannot represent it at any
// exponent it is willing to reach — far past the ~$92bn the fixed scale allows
// and past what rescaling recovers.
func hugeRat() *big.Rat {
	// 10^400: comfortably beyond an int64 coefficient at any reachable exponent.
	return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(400), nil))
}

// A quantity that cannot be represented must REFUSE, not be fabricated. The
// alternative is COMP-M1's defect: a zero-valued position is treated as flat by
// heldPositions and silently drops out of the compliance check.
func TestBookStateOf_UnrepresentableValueRefuses(t *testing.T) {
	b := NewBook("USD")
	l := &lot{qty: hugeRat(), avg: big.NewRat(1, 1), realized: new(big.Rat)}

	_, err := b.stateOf("p1", "XSIM", "AAPL", l, big.NewRat(1, 1), time.Now())
	if err == nil {
		t.Fatal("stateOf returned nil error for a quantity that cannot be represented — " +
			"a fabricated or zeroed position is what COMP-M1 was opened for")
	}
	if !strings.Contains(err.Error(), "AAPL") {
		t.Errorf("error %q does not name the instrument — an operator cannot act on it", err)
	}
}

func TestBookMoney_UnrepresentableAmountRefuses(t *testing.T) {
	b := NewBook("USD")
	if _, err := b.money(hugeRat()); err == nil {
		t.Fatal("money returned nil error for an amount that cannot be represented")
	}
}

// NON-VACUITY, and the test that matters most: every guard here fails closed, so
// an implementation that refused EVERYTHING would satisfy both tests above.
// Ordinary values must still produce a state, unchanged.
func TestBookStateOf_OrdinaryValuesStillSucceed(t *testing.T) {
	b := NewBook("USD")
	l := &lot{qty: big.NewRat(100, 1), avg: big.NewRat(25, 1), realized: new(big.Rat)}

	st, err := b.stateOf("p1", "XSIM", "AAPL", l, big.NewRat(30, 1), time.Now())
	if err != nil {
		t.Fatalf("stateOf refused an ordinary 100-share position: %v", err)
	}
	if st.GetQuantity().GetCoefficient() == 0 {
		t.Fatal("quantity is zero for a 100-share position — a zero-valued position is " +
			"dropped by heldPositions and vanishes from the compliance check")
	}
	if st.GetMarketValue().GetAmount().GetCoefficient() == 0 {
		t.Fatal("market value is zero for a 100 × 30 position")
	}
}

// Snapshot is the other caller with an error channel and must propagate rather
// than swallow. The book is seeded by writing b.lots directly — this test is
// in-package, and going through Apply would couple it to the fill helper for no
// benefit, since what is under test is Snapshot's propagation.
func TestBookSnapshot_PropagatesRepresentabilityFailure(t *testing.T) {
	b := NewBook("USD")
	for k := range b.lots {
		delete(b.lots, k)
	}
	// key is the package's unexported map key; construct it the way book.go does.
	b.lots[key{portfolio: "p1", venue: "XSIM", instrument: "AAPL"}] = &lot{
		qty: hugeRat(), avg: big.NewRat(1, 1), realized: new(big.Rat),
	}
	if _, err := b.Snapshot(context.Background(), "p1", time.Now()); err == nil {
		t.Fatal("Snapshot returned nil error while holding an unrepresentable position")
	}
}
```

**Verified against the real API:** `NewBook(baseCcy string) *Book`, `lot{qty, avg, realized}` (all `*big.Rat`), and `Book.lots map[key]*lot` all exist as written. The one thing to check is `key`'s field names — read its declaration in `book.go` and use the real ones. The package's existing test helpers are `d(coeff, exp)` and `fill(side, qty, price)`; these tests deliberately avoid both. **Do not change production code to make a test compile** — adjust the test to the real API and say so in your report.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/oms/internal/position/ -run 'Representab|UnrepresentableValue|UnrepresentableAmount|OrdinaryValues|PropagatesRepresentability' -v -count=1`

Expected: compile failure — `stateOf` and `money` return one value, not two. That is the RED. Record it.

- [ ] **Step 3: Thread the error through `book.go`**

Replace `stateOf` at `book.go:117`:

```go
func (b *Book) stateOf(portfolioID, venue, instrument string, l *lot, price *big.Rat, asOf time.Time) (*domainpb.PositionState, error) {
	unreal := new(big.Rat).Mul(new(big.Rat).Sub(price, l.avg), l.qty)
	qty, ok := dec.ToProtoScaled(l.qty)
	if !ok {
		return nil, fmt.Errorf("position %s/%s: quantity is not representable", portfolioID, instrument)
	}
	avg, ok := dec.ToProtoScaled(l.avg)
	if !ok {
		return nil, fmt.Errorf("position %s/%s: average price is not representable", portfolioID, instrument)
	}
	marketValue, err := b.money(new(big.Rat).Mul(price, l.qty))
	if err != nil {
		return nil, fmt.Errorf("position %s/%s market value: %w", portfolioID, instrument, err)
	}
	realized, err := b.money(l.realized)
	if err != nil {
		return nil, fmt.Errorf("position %s/%s realized pnl: %w", portfolioID, instrument, err)
	}
	unrealized, err := b.money(unreal)
	if err != nil {
		return nil, fmt.Errorf("position %s/%s unrealized pnl: %w", portfolioID, instrument, err)
	}
	return &domainpb.PositionState{
		PortfolioId:   portfolioID,
		Venue:         venue,
		InstrumentId:  instrument,
		Quantity:      qty,
		AveragePrice:  avg,
		MarketValue:   marketValue,
		RealizedPnl:   realized,
		UnrealizedPnl: unrealized,
		AsOf:          timestamppb.New(asOf.UTC()),
	}, nil
}
```

Replace `money` at `book.go:184`:

```go
// money wraps an exact amount as Money, refusing rather than fabricating one.
// dec.ToProtoScaled preserves magnitude by rescaling; ok is false only when the
// value cannot be represented at any exponent it will reach, which no real
// position value approaches. Returning a zero Money here would be worse than
// returning nothing: heldPositions treats a zero-valued position as flat and
// drops it, so the compliance rules would stop seeing the holding entirely.
func (b *Book) money(r *big.Rat) (*commonpb.Money, error) {
	amt, ok := dec.ToProtoScaled(r)
	if !ok {
		return nil, fmt.Errorf("amount is not representable as a Decimal")
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: b.baseCcy}, nil
}
```

Update `Apply`'s call sites at `book.go:89-90`:

```go
	venueState, err := b.stateOf(portfolioID, fill.GetVenue(), fill.GetInstrumentId(), l, price, asOf)
	if err != nil {
		return nil, err
	}
	aggState, err := b.stateOf(portfolioID, "", fill.GetInstrumentId(), b.aggregate(portfolioID, fill.GetInstrumentId()), price, asOf)
	if err != nil {
		return nil, err
	}
	return &Applied{Venue: venueState, Aggregate: aggState}, nil
```

Update `Snapshot`'s call sites at `book.go:217-226`:

```go
		marketValue, err := b.money(mv)
		if err != nil {
			return nil, fmt.Errorf("portfolio %s position %s: %w", portfolioID, k.instrument, err)
		}
		realized, err := b.money(l.realized)
		if err != nil {
			return nil, fmt.Errorf("portfolio %s position %s realized pnl: %w", portfolioID, k.instrument, err)
		}
		qty, ok := dec.ToProtoScaled(l.qty)
		if !ok {
			return nil, fmt.Errorf("portfolio %s position %s: quantity is not representable", portfolioID, k.instrument)
		}
		avg, ok := dec.ToProtoScaled(l.avg)
		if !ok {
			return nil, fmt.Errorf("portfolio %s position %s: average price is not representable", portfolioID, k.instrument)
		}
		positions = append(positions, &domainpb.PositionState{
			PortfolioId:  portfolioID,
			InstrumentId: k.instrument,
			Quantity:     qty,
			AveragePrice: avg,
			MarketValue:  marketValue,
			RealizedPnl:  realized,
			AsOf:         ts,
		})
```

and the NAV at `book.go:226`:

```go
	navMoney, err := b.money(nav)
	if err != nil {
		return nil, fmt.Errorf("portfolio %s NAV: %w", portfolioID, err)
	}
```

then use `navMoney` for `TotalMarketValue`. Add `"fmt"` to `book.go`'s imports if absent.

- [ ] **Step 4: Thread the error through `postgres.go`**

Replace `stateOf` at `postgres.go:337`:

```go
func (p *Postgres) stateOf(portfolioID, venue, instrument string, l *lot, price *big.Rat, asOf time.Time) (*domainpb.PositionState, error) {
	unreal := new(big.Rat).Mul(new(big.Rat).Sub(price, l.avg), l.qty)
	qty, ok := dec.ToProtoScaled(l.qty)
	if !ok {
		return nil, fmt.Errorf("position %s/%s: quantity is not representable", portfolioID, instrument)
	}
	avg, ok := dec.ToProtoScaled(l.avg)
	if !ok {
		return nil, fmt.Errorf("position %s/%s: average price is not representable", portfolioID, instrument)
	}
	marketValue, err := p.money(new(big.Rat).Mul(price, l.qty))
	if err != nil {
		return nil, fmt.Errorf("position %s/%s market value: %w", portfolioID, instrument, err)
	}
	realized, err := p.money(l.realized)
	if err != nil {
		return nil, fmt.Errorf("position %s/%s realized pnl: %w", portfolioID, instrument, err)
	}
	unrealized, err := p.money(unreal)
	if err != nil {
		return nil, fmt.Errorf("position %s/%s unrealized pnl: %w", portfolioID, instrument, err)
	}
	return &domainpb.PositionState{
		PortfolioId:   portfolioID,
		Venue:         venue,
		InstrumentId:  instrument,
		Quantity:      qty,
		AveragePrice:  avg,
		MarketValue:   marketValue,
		RealizedPnl:   realized,
		UnrealizedPnl: unrealized,
		AsOf:          timestamppb.New(asOf.UTC()),
	}, nil
}
```

Replace `money` at `postgres.go:352`:

```go
// money wraps an exact amount as Money, refusing rather than fabricating one.
// Returning a zero Money would be worse than returning nothing: heldPositions
// treats a zero-valued position as flat and drops it, so the compliance rules
// would stop seeing the holding entirely.
func (p *Postgres) money(r *big.Rat) (*commonpb.Money, error) {
	amt, ok := dec.ToProtoScaled(r)
	if !ok {
		return nil, fmt.Errorf("amount is not representable as a Decimal")
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: p.baseCcy}, nil
}
```

Update `Apply`'s call sites at `postgres.go:162-163`:

```go
	venueState, err := p.stateOf(portfolioID, venue, instrument, l, price, asOf)
	if err != nil {
		return nil, err
	}
	aggState, err := p.stateOf(portfolioID, "", instrument, agg, price, asOf)
	if err != nil {
		return nil, err
	}
	return &Applied{Venue: venueState, Aggregate: aggState}, nil
```

Update `Snapshot`'s call sites at `postgres.go:316-326`. **Note this file uses `x.l.*` where `book.go` uses `l.*`, and a local `instrument` where `book.go` uses `k.instrument`:**

```go
		marketValue, err := p.money(mv)
		if err != nil {
			return nil, fmt.Errorf("portfolio %s position %s: %w", portfolioID, instrument, err)
		}
		realized, err := p.money(x.l.realized)
		if err != nil {
			return nil, fmt.Errorf("portfolio %s position %s realized pnl: %w", portfolioID, instrument, err)
		}
		qty, ok := dec.ToProtoScaled(x.l.qty)
		if !ok {
			return nil, fmt.Errorf("portfolio %s position %s: quantity is not representable", portfolioID, instrument)
		}
		avg, ok := dec.ToProtoScaled(x.l.avg)
		if !ok {
			return nil, fmt.Errorf("portfolio %s position %s: average price is not representable", portfolioID, instrument)
		}
		positions = append(positions, &domainpb.PositionState{
			PortfolioId:  portfolioID,
			InstrumentId: instrument,
			Quantity:     qty,
			AveragePrice: avg,
			MarketValue:  marketValue,
			RealizedPnl:  realized,
			AsOf:         ts,
		})
```

and the NAV at `postgres.go:326`:

```go
	navMoney, err := p.money(nav)
	if err != nil {
		return nil, fmt.Errorf("portfolio %s NAV: %w", portfolioID, err)
	}
```

then use `navMoney` for `TotalMarketValue`. `postgres.go` already imports `"fmt"` (it uses `fmt.Errorf` at `:159`).

**If any local variable name in this file differs from what is written above, use the real one** — the surrounding loop's variables were read at `:310-330`, but check the enclosing scope rather than assuming.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/oms/internal/position/ -v -count=1`
Expected: PASS — the four new tests plus every pre-existing test in the package.

- [ ] **Step 6: Empty the exception map**

In `kanz/test/arch/decimal_conversion_test.go`, remove both entries from `pendingErrorThreading`, leaving an empty map with its explanatory comment intact:

```go
// pendingErrorThreading are capital-path call sites that still use the wrapping
// conversion because their enclosing function has NO error return.
//
// EMPTY, and that is the point: it was the worklist, and the work is done. The
// guard's stale-exemption arm means an entry whose file no longer calls
// dec.ToProto FAILS the build, so this map cannot quietly accumulate excuses.
// A new entry needs a written reason and should be temporary.
var pendingErrorThreading = map[string]string{}
```

- [ ] **Step 7: The guard must now pass on its own terms**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestCapitalPathsDoNotUseWrappingToProto -v -count=1`
Expected: PASS. If it fails naming a file, that file still calls `dec.ToProto` and the migration is incomplete — finish it rather than re-adding the exemption.

- [ ] **Step 8: Mutation-test both guard arms**

Revert one migrated call site in `book.go` to `dec.ToProto` and confirm the guard fails naming that file and line. Restore with `git checkout -- <path>`.

Then add a bogus entry `"services/oms/internal/order/aggregate.go": "bogus"` to `pendingErrorThreading` — that file is fully migrated, so the **stale-exemption** arm must fire. Restore. (Removing an entry for a file that still calls `ToProto` trips the *main* arm, not the stale one, so it does not exercise this path.)

- [ ] **Step 9: Full suite**

Run: `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./... && GOFLAGS=-mod=mod go test -p 1 ./...`
Expected: build/vet clean; 159 test packages ok, 0 failures.

- [ ] **Step 10: Commit**

```bash
cd kanz && gofmt -w services/oms/internal/position/ test/arch/
git add kanz/services/oms/internal/position/ kanz/test/arch/decimal_conversion_test.go
git commit -m "fix(position): refuse an unrepresentable position value instead of fabricating one"
```

---

### Task 2: Update the board

**Files:**
- Modify: `KANZ_TASKS.md` — the `dec.ToProto` row

- [ ] **Step 1: Close the worklist**

The row currently reads PARTIALLY FIXED with ten declared sites. Update it: the capital paths are now fully migrated, `pendingErrorThreading` is empty, and the guard covers the whole set. State plainly what remains — the ~30 non-capital callers (reporting, analytics) keep the wrapping `ToProto` deliberately, because migrating them has no correctness payoff, and the guard prevents new capital-path uses.

- [ ] **Step 2: Validate the table**

Run: `cd /c/Users/root/Desktop/eighred-kanz && sh .superpowers/sdd/validate-board.sh KANZ_TASKS.md`
Expected: `board OK: N rows, 5 columns each`, exit 0. This script **exits non-zero** on a malformed row — do not replace it with an inline `awk`, which exits 0 on a finding and lets a `&& git add` chain commit through a failure.

**Watch for literal `|` characters in prose** — they split a markdown table cell. Write `10^abs(exponent)`, not the pipe form.

- [ ] **Step 3: Commit**

```bash
git add KANZ_TASKS.md
git commit -m "docs(board): capital-path Decimal conversion fully migrated"
```

---

## Verification (whole feature)

- [ ] `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./...` — clean.
- [ ] `cd kanz && GOFLAGS=-mod=mod go test -p 1 ./...` — 159 test packages ok, 0 failures.
- [ ] `grep -rn "dec.ToProto(" kanz/services/oms/internal/position/` returns nothing.
- [ ] `pendingErrorThreading` is empty and the guard passes.
- [ ] Both guard arms were shown failing under mutation.
- [ ] An ordinary position still produces a non-zero quantity and market value — the bound refuses nothing real.

## Out of scope

- The ~30 non-capital `dec.ToProto` callers (reporting, analytics) — they keep the wrap deliberately; the guard scopes to capital-path packages.
- `dec.ToProto` itself — signature and behaviour unchanged.

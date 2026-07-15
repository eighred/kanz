# RISK-M1 Tail & Concentration Measures Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add four risk measures — `ES99` (Expected Shortfall / CVaR), `MaxDrawdown` (fraction), `MaxDrawdownAmount` (money), and `HHI` (concentration) — to the risk engine's measure Registry.

**Architecture:** Each measure plugs into the existing RISK-07 Registry (`internal/risk/compute`). `HHI` is a pure position-based `MeasureFunc` (no market data), registered unconditionally in `DefaultRegistry`. `ES99` and the two drawdown measures are returns-provider-backed `ReturnsMeasure`s in the `varmodel` package (`internal/risk/compute/var`); they reuse the historical-sim P&L distribution via a behaviour-preserving `portfolioPnL` extraction so VaR and ES cannot drift, and register at the same market-data-gated site as historical VaR.

**Tech Stack:** Go 1.26, `github.com/kanz-eng/kanz-schemas-go/common/v1` (`Decimal`), standard-library `math`/`sort`. No new dependencies.

## Global Constraints

- **`MeasureName` is a plain string type**, not a proto enum — new measures need NO `.proto` or api/v1 change. Names are a **one-way door** (dashboards/alerts bind to them): `ES99`, `MaxDrawdown`, `MaxDrawdownAmount`, `HHI` — exact, chosen once.
- **`MeasureFunc` never errors and never returns nil**: insufficient data / empty portfolio ⇒ a zero-value `Measure` (the response layer marks it degraded).
- **Money measures** emit at `varExponent = -2` (base-currency cents), matching `VaR99`. **Fraction/dimensionless measures** (`MaxDrawdown`, `HHI`) emit at exponent `-4`.
- **Same-currency convention**: only positions whose `MarketValue.CurrencyCode == p.BaseCurrency()` are included; others are skipped (no FX layer).
- **Deployed VaR is historical-sim** (`varmodel.Register` → `Historical`). ES/drawdown pair with that path. Do NOT add Monte-Carlo variants (YAGNI — MC VaR is not deployed).
- Every task ends green: `go build ./... && go vet ./...` and the named tests pass. `GOFLAGS=-mod=mod` is set for this module.
- The `test/arch` suite must stay unchanged — no new service, manifest, or contract.

## File Structure

- `internal/risk/compute/concentration.go` (new) — `MeasureHHI` const + `HHI` `MeasureFunc`; registered in `DefaultRegistry`.
- `internal/risk/compute/concentration_test.go` (new) — HHI unit tests.
- `internal/risk/compute/measures.go` (modify) — add `MeasureHHI` registration to `DefaultRegistry`; add the four new name consts here (registry-visible, alongside `MeasureVaR99`).
- `internal/risk/compute/var/historical.go` (modify) — extract `portfolioPnL`; add `zeroNamed`; `Historical` reads the helper (behaviour-preserving).
- `internal/risk/compute/var/expectedshortfall.go` (new) — `ExpectedShortfall` `ReturnsMeasure`.
- `internal/risk/compute/var/drawdown.go` (new) — `maxDrawdown` helper + `MaxDrawdownFraction` / `MaxDrawdownAmount` `ReturnsMeasure`s.
- `internal/risk/compute/var/tailmeasures_test.go` (new, `package varmodel_test`) — ES + drawdown-measure tests (reuses the `fixedProvider`/`portfolio`/`dval` helpers already in `historical_test.go`, same package).
- `internal/risk/compute/var/drawdown_internal_test.go` (new, `package varmodel`) — direct unit test of the unexported `maxDrawdown` helper (the divergence case).
- `internal/risk/compute/var/register.go` OR extend `historical.go`'s `Register` (modify) — register ES + both drawdowns alongside VaR99.
- `internal/risk/compute/var/register_test.go` (new or extend `historical_test.go`) — end-to-end Register wiring.
- `services/risk-engine/cmd/risk-engine/main.go` (modify, ~line 162) — update the log line to name the newly registered measures.

---

### Task 1: HHI concentration measure

Pure position-based measure, no provider. Simplest and independent — do first.

**Files:**
- Create: `internal/risk/compute/concentration.go`
- Create: `internal/risk/compute/concentration_test.go`
- Modify: `internal/risk/compute/measures.go` (add `MeasureHHI` name const near the existing `MeasureVaR99` block; register `HHI` in `DefaultRegistry`)
- Modify: `internal/risk/compute/measures_test.go` (`TestComputeMeasures_DefaultRegistryProducesAllFour` asserts an exact 4-measure set — HHI makes it 5; update the expected list)

**Interfaces:**
- Consumes: `domain.Portfolio` (`.Positions()`, `.BaseCurrency()`), `v1.Measure`, `v1.MeasureName`, the existing package-private `decimalToFloat` / `floatToDecimal` (`internal/risk/compute/decimal.go`).
- Produces: `const MeasureHHI v1.MeasureName = "HHI"`; `func HHI(p *domain.Portfolio) v1.Measure`.

- [ ] **Step 1: Write the failing test**

Create `internal/risk/compute/concentration_test.go`. Use `package compute_test` (matching `measures_test.go`) and reuse the existing `makePortfolio(id v1.PortfolioID, ccy domain.CurrencyCode, ...domain.Position)` and `mkMoney(coef int64, exp int32, ccy string)` helpers from `exposure_test.go`:

```go
package compute_test

import (
	"math"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

func hhiVal(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

func TestHHI(t *testing.T) {
	tests := []struct {
		name string
		pos  []domain.Position
		want float64
	}{
		{"single position is fully concentrated",
			[]domain.Position{{InstrumentID: "AAPL", MarketValue: mkMoney(1000, 0, "USD")}}, 1.0},
		{"two equal positions ⇒ 1/n",
			[]domain.Position{
				{InstrumentID: "AAPL", MarketValue: mkMoney(500, 0, "USD")},
				{InstrumentID: "MSFT", MarketValue: mkMoney(500, 0, "USD")},
			}, 0.5},
		{"four equal positions ⇒ 0.25",
			[]domain.Position{
				{InstrumentID: "A", MarketValue: mkMoney(250, 0, "USD")},
				{InstrumentID: "B", MarketValue: mkMoney(250, 0, "USD")},
				{InstrumentID: "C", MarketValue: mkMoney(250, 0, "USD")},
				{InstrumentID: "D", MarketValue: mkMoney(250, 0, "USD")},
			}, 0.25},
		{"mixed 750/250 ⇒ 0.625",
			[]domain.Position{
				{InstrumentID: "AAPL", MarketValue: mkMoney(750, 0, "USD")},
				{InstrumentID: "MSFT", MarketValue: mkMoney(250, 0, "USD")},
			}, 0.625},
		{"non-base-currency positions are skipped",
			[]domain.Position{
				{InstrumentID: "AAPL", MarketValue: mkMoney(1000, 0, "USD")},
				{InstrumentID: "VOD.L", MarketValue: mkMoney(9999, 0, "EUR")},
			}, 1.0},
		{"empty portfolio ⇒ 0", nil, 0.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := compute.HHI(makePortfolio("P1", "USD", tc.pos...))
			if m.Name != compute.MeasureHHI {
				t.Fatalf("name = %q, want HHI", m.Name)
			}
			if got := hhiVal(m.Value); math.Abs(got-tc.want) > 1e-4 {
				t.Fatalf("HHI = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHHIRegisteredInDefault(t *testing.T) {
	r := compute.DefaultRegistry()
	found := false
	for _, n := range r.Names() {
		if n == compute.MeasureHHI {
			found = true
		}
	}
	if !found {
		t.Fatal("HHI must be in DefaultRegistry (positions-only, always available)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/ -run TestHHI -v`
Expected: FAIL — `undefined: HHI` / `undefined: MeasureHHI`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/risk/compute/concentration.go`:

```go
package compute

import (
	"math"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// hhiExponent is the Decimal scale of the emitted HHI: a dimensionless ratio in
// [1/n, 1], so a fractional scale (not the -2 money scale VaR/ES use).
const hhiExponent int32 = -4

// HHI is the Herfindahl-Hirschman Index of position concentration: Σ wᵢ² where
// wᵢ is each position's share of GROSS exposure (|MarketValue| / Σ|MarketValue|),
// over positions in the portfolio base currency (the RISK-07 same-currency
// convention; other-currency positions are skipped). Bounds: 1/n ≤ HHI ≤ 1 — 1
// is a single-position book (maximally concentrated), 1/n is n equal positions
// (maximally diversified). A zero-gross / empty portfolio yields the zero-value
// measure (the MeasureFunc never-error contract). Needs no market data, so it is
// registered unconditionally in DefaultRegistry and is served even in the
// no-price-store fallback.
func HHI(p *domain.Portfolio) v1.Measure {
	base := string(p.BaseCurrency())
	var sumSq, gross float64
	for _, pos := range p.Positions() {
		if pos.MarketValue == nil || pos.MarketValue.CurrencyCode != base {
			continue
		}
		v := math.Abs(decimalToFloat(pos.MarketValue.Amount))
		sumSq += v * v
		gross += v
	}
	if gross == 0 {
		return v1.Measure{Name: MeasureHHI, Value: &commonpb.Decimal{Coefficient: 0, Exponent: 0}}
	}
	hhi := sumSq / (gross * gross)
	return v1.Measure{Name: MeasureHHI, Value: floatToDecimal(hhi, hhiExponent)}
}
```

In `internal/risk/compute/measures.go`, add the name const to the existing const block (after `MeasureDelta`):

```go
	MeasureHHI v1.MeasureName = "HHI"
```

And register it in `DefaultRegistry` (after the `MeasureDelta` line):

```go
	r.Register(MeasureHHI, HHI)
```

Update `TestComputeMeasures_DefaultRegistryProducesAllFour` in `internal/risk/compute/measures_test.go` — `Registry.Names()` returns lexicographic order, so HHI slots between `GrossExposure` and `NetExposure`. Rename the test and extend `want`:

```go
func TestComputeMeasures_DefaultRegistryProducesAll(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoney(1000, 0, "USD"), AsOf: baseTime},
	)
	set := compute.ComputeMeasures(p, nil, nil)
	names := set.Names()
	want := []v1.MeasureName{
		compute.MeasureDelta,
		compute.MeasureGrossExposure,
		compute.MeasureHHI,
		compute.MeasureNetExposure,
		compute.MeasureVaR99,
	}
	if len(names) != len(want) {
		t.Fatalf("names=%v want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("names[%d]=%q want %q", i, names[i], n)
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/ -run 'TestHHI|TestHHIRegisteredInDefault' -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Run the full compute package to catch regressions**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/`
Expected: `ok` — the new `DefaultRegistry` entry does not break existing tests.

- [ ] **Step 6: Commit**

```bash
git add internal/risk/compute/concentration.go internal/risk/compute/concentration_test.go internal/risk/compute/measures.go internal/risk/compute/measures_test.go
git commit -m "feat(risk): HHI concentration measure (RISK-M1)"
```

---

### Task 2: Extract `portfolioPnL` (behaviour-preserving refactor)

Pull the per-scenario P&L construction out of `Historical` into a shared helper so ES and drawdown build on the identical distribution. VaR behaviour must not change — the existing tests are the spec.

**Files:**
- Modify: `internal/risk/compute/var/historical.go`

**Interfaces:**
- Produces (package-private, used by Tasks 3–4):
  - `func portfolioPnL(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider, window int) (pnl []float64, v0 float64, ok bool)` — time-ordered per-scenario P&L, `v0` = signed sum of base-currency position values, `ok=false` on insufficient data (`<1` leg or `<2` scenarios).
  - `func zeroNamed(name v1.MeasureName) v1.Measure` — the zero-value measure for an arbitrary name.

- [ ] **Step 1: Confirm the existing VaR tests are green (the refactor's guardrail)**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/var/ -run TestHistorical -v`
Expected: PASS (`TestHistorical_EmpiricalQuantile` gives VaR99 = 98.00, etc.).

- [ ] **Step 2: Refactor `historical.go`**

Replace the body of `Historical`'s returned closure and add the two helpers. The `Historical` function becomes:

```go
func Historical(cfg Config) compute.ReturnsMeasure {
	conf := cfg.confidence()
	window := cfg.Window
	return func(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider) v1.Measure {
		pnl, _, ok := portfolioPnL(ctx, p, rp, window)
		if !ok {
			return zeroNamed(compute.MeasureVaR99)
		}
		sorted := append([]float64(nil), pnl...)
		sort.Float64s(sorted)
		loss := -quantile(sorted, 1-conf)
		if loss < 0 {
			loss = 0
		}
		return v1.Measure{
			Name:  compute.MeasureVaR99,
			Value: floatToDecimal(loss, varExponent),
		}
	}
}

// portfolioPnL builds the TIME-ORDERED per-scenario P&L series for the
// portfolio's base-currency positions over the window, tail-aligned to the
// shortest available series (providers return most-recent-N, so the recent tail
// lines up). pnl[t] = Σ_i value_i × return_i[t]. v0 is the signed sum of the
// included positions' base-currency values — the starting portfolio value the
// drawdown path folds P&L onto. ok=false on insufficient data (no legs, or the
// common window < 2), matching Historical's original guard. The series is NOT
// sorted: VaR/ES sort a copy, drawdown walks it in time order.
func portfolioPnL(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider, window int) (pnl []float64, v0 float64, ok bool) {
	base := string(p.BaseCurrency())
	type leg struct {
		value   float64
		returns []float64
	}
	var legs []leg
	minLen := -1
	for _, pos := range p.Positions() {
		if pos.MarketValue == nil || pos.MarketValue.CurrencyCode != base {
			continue
		}
		r, err := rp.Returns(ctx, string(pos.InstrumentID), p.AsOf(), window)
		if err != nil || len(r) == 0 {
			continue
		}
		val := decimalToFloat(pos.MarketValue.Amount)
		legs = append(legs, leg{value: val, returns: r})
		v0 += val
		if minLen < 0 || len(r) < minLen {
			minLen = len(r)
		}
	}
	if len(legs) == 0 || minLen < 2 {
		return nil, 0, false
	}
	pnl = make([]float64, minLen)
	for _, lg := range legs {
		off := len(lg.returns) - minLen // tail-align to the common window
		for t := 0; t < minLen; t++ {
			pnl[t] += lg.value * lg.returns[off+t]
		}
	}
	return pnl, v0, true
}

// zeroNamed is the zero-value measure for name — the MeasureFunc never-error
// return when there is insufficient data.
func zeroNamed(name v1.MeasureName) v1.Measure {
	return v1.Measure{Name: name, Value: &commonpb.Decimal{Coefficient: 0, Exponent: 0}}
}
```

`montecarlo.go` still calls `zeroMeasure()`, so do NOT delete it — **redefine** it to delegate to `zeroNamed`, leaving `montecarlo.go` untouched. Replace the existing `zeroMeasure` body in `historical.go` with:

```go
func zeroMeasure() v1.Measure { return zeroNamed(compute.MeasureVaR99) }
```

Net: `historical.go` gains `portfolioPnL` and `zeroNamed`, `zeroMeasure` becomes a one-line delegator, and `Historical` reads `portfolioPnL`. No other file changes in this task.

- [ ] **Step 3: Run the VaR tests to verify behaviour is preserved**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/var/ -run 'TestHistorical|TestMonteCarlo|TestRegister' -v`
Expected: PASS — identical numbers to Step 1 (VaR99 = 98.00, etc.). If any VaR value changed, the extraction is not behaviour-preserving — revert and redo.

- [ ] **Step 4: Commit**

```bash
git add internal/risk/compute/var/historical.go
git commit -m "refactor(risk): extract portfolioPnL so VaR/ES/drawdown share one distribution"
```

---

### Task 3: `ES99` — Expected Shortfall / CVaR

**Files:**
- Create: `internal/risk/compute/var/expectedshortfall.go`
- Create: `internal/risk/compute/var/tailmeasures_test.go` (holds ES + drawdown-measure tests; `package varmodel_test`)
- Modify: `internal/risk/compute/measures.go` (add `MeasureES99` name const)

**Interfaces:**
- Consumes: `portfolioPnL`, `zeroNamed`, `quantile`, `floatToDecimal`, `varExponent` (Task 2 + existing `historical.go`); `compute.MeasureES99`.
- Produces: `const MeasureES99 v1.MeasureName = "ES99"` (in `compute`); `func ExpectedShortfall(cfg Config) compute.ReturnsMeasure`.

- [ ] **Step 1: Write the failing test**

Create `internal/risk/compute/var/tailmeasures_test.go` (reuses `fixedProvider`, `portfolio`, `money`, `dval`, `asOf` from `historical_test.go` — same `varmodel_test` package):

```go
package varmodel_test

import (
	"context"
	"math"
	"testing"

	"github.com/kanz-eng/kanz/internal/risk/compute"
	varmodel "github.com/kanz-eng/kanz/internal/risk/compute/var"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// $1000 under returns {-10,-5,0,+5,+10}% ⇒ P&L {-100,-50,0,50,100}. n=5, α=0.99,
// k=⌈5·0.01⌉=1, so ES99 = −mean(worst 1) = 100.00. VaR99 (interpolated) = 98.00,
// so ES99 ≥ VaR99 holds.
func TestExpectedShortfall_Empirical(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{-0.10, -0.05, 0, 0.05, 0.10}}

	es := varmodel.ExpectedShortfall(varmodel.Config{})(context.Background(), p, prov)
	if es.Name != compute.MeasureES99 {
		t.Fatalf("name = %q, want ES99", es.Name)
	}
	if got := dval(es.Value); math.Abs(got-100.0) > 1e-9 {
		t.Fatalf("ES99 = %v, want 100.00", got)
	}
}

// ES99 ≥ VaR99 on the same sample — the core consistency invariant (both read
// the same portfolioPnL distribution).
func TestExpectedShortfall_GEVaR(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{-0.10, -0.07, -0.03, 0, 0.02, 0.05, 0.08, 0.10}}

	varv := dval(varmodel.Historical(varmodel.Config{})(context.Background(), p, prov).Value)
	es := dval(varmodel.ExpectedShortfall(varmodel.Config{})(context.Background(), p, prov).Value)
	if es < varv-1e-9 {
		t.Fatalf("ES99 (%v) must be ≥ VaR99 (%v)", es, varv)
	}
}

func TestExpectedShortfall_AllGains(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{0.01, 0.02, 0.03, 0.05}} // no losing scenario
	if got := dval(varmodel.ExpectedShortfall(varmodel.Config{})(context.Background(), p, prov).Value); got != 0 {
		t.Fatalf("all-gains window ⇒ ES99 floored at 0, got %v", got)
	}
}

func TestExpectedShortfall_InsufficientData(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{0.01}} // <2 scenarios
	if got := dval(varmodel.ExpectedShortfall(varmodel.Config{})(context.Background(), p, prov).Value); got != 0 {
		t.Fatalf("insufficient data ⇒ zero ES99, got %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/var/ -run TestExpectedShortfall -v`
Expected: FAIL — `undefined: varmodel.ExpectedShortfall` / `undefined: compute.MeasureES99`.

- [ ] **Step 3: Write minimal implementation**

Add the name const to `internal/risk/compute/measures.go` const block:

```go
	MeasureES99 v1.MeasureName = "ES99"
```

Create `internal/risk/compute/var/expectedshortfall.go`:

```go
package varmodel

import (
	"context"
	"math"
	"sort"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// ExpectedShortfall builds the ES99 (CVaR) measure: the mean loss in the tail
// AT OR BEYOND the 99% VaR quantile, on the same empirical P&L distribution as
// Historical VaR (via portfolioPnL) — so ES99 ≥ VaR99 by construction and the
// two cannot drift. Estimator: ES = −mean(worst k P&Ls), k = ⌈n·(1−α)⌉ (the
// empirical ES estimator — the mean of the ⌈n·(1−α)⌉ worst scenarios; e.g.
// n=250, α=0.99 ⇒ k=3), k floored at 1 so a small window still yields the single
// worst loss. Emitted as a money loss in base-currency cents, like VaR99.
// Insufficient data ⇒ zero measure; a non-loss tail floors at zero.
func ExpectedShortfall(cfg Config) compute.ReturnsMeasure {
	conf := cfg.confidence()
	window := cfg.Window
	return func(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider) v1.Measure {
		pnl, _, ok := portfolioPnL(ctx, p, rp, window)
		if !ok {
			return zeroNamed(compute.MeasureES99)
		}
		sorted := append([]float64(nil), pnl...)
		sort.Float64s(sorted) // ascending: worst (most negative) first
		n := len(sorted)
		k := int(math.Ceil(float64(n) * (1 - conf)))
		if k < 1 {
			k = 1
		}
		if k > n {
			k = n
		}
		var sum float64
		for i := 0; i < k; i++ {
			sum += sorted[i]
		}
		es := -(sum / float64(k))
		if es < 0 {
			es = 0
		}
		return v1.Measure{
			Name:  compute.MeasureES99,
			Value: floatToDecimal(es, varExponent),
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/var/ -run TestExpectedShortfall -v`
Expected: PASS (all four subtests; ES99 = 100.00, ES99 ≥ VaR99, all-gains ⇒ 0, insufficient ⇒ 0).

- [ ] **Step 5: Commit**

```bash
git add internal/risk/compute/var/expectedshortfall.go internal/risk/compute/var/tailmeasures_test.go internal/risk/compute/measures.go
git commit -m "feat(risk): ES99 Expected Shortfall / CVaR (RISK-M1)"
```

---

### Task 4: `MaxDrawdown` (fraction) + `MaxDrawdownAmount` (money)

Two measures from one `maxDrawdown` walk. Independent maxima — the worst %-decline and the worst $-decline can fall at different peaks; there is deliberately no fixed `Amount = Fraction × peak` relationship.

**Files:**
- Create: `internal/risk/compute/var/drawdown.go`
- Create: `internal/risk/compute/var/drawdown_internal_test.go` (`package varmodel` — tests the unexported helper)
- Modify: `internal/risk/compute/var/tailmeasures_test.go` (add measure-level tests)
- Modify: `internal/risk/compute/measures.go` (add the two name consts)

**Interfaces:**
- Consumes: `portfolioPnL`, `zeroNamed`, `floatToDecimal`, `varExponent` (money); `compute.MeasureMaxDrawdown`, `compute.MeasureMaxDrawdownAmount`.
- Produces:
  - `const MeasureMaxDrawdown v1.MeasureName = "MaxDrawdown"`, `const MeasureMaxDrawdownAmount v1.MeasureName = "MaxDrawdownAmount"` (in `compute`).
  - `func maxDrawdown(pnl []float64, v0 float64) (fraction, amount float64)` (package-private).
  - `func MaxDrawdownFraction(cfg Config) compute.ReturnsMeasure`, `func MaxDrawdownAmount(cfg Config) compute.ReturnsMeasure`.

- [ ] **Step 1: Write the failing tests**

Create `internal/risk/compute/var/drawdown_internal_test.go`:

```go
package varmodel

import (
	"math"
	"testing"
)

// Single-peak path: v0=1000, P&L {-100,-50,+200} ⇒ path 1000→900→850→1050.
// Trough 850 vs peak 1000: amount=150, fraction=0.15.
func TestMaxDrawdown_SinglePeak(t *testing.T) {
	frac, amount := maxDrawdown([]float64{-100, -50, 200}, 1000)
	if math.Abs(amount-150) > 1e-9 {
		t.Fatalf("amount = %v, want 150", amount)
	}
	if math.Abs(frac-0.15) > 1e-9 {
		t.Fatalf("fraction = %v, want 0.15", frac)
	}
}

// Monotonic rise ⇒ no drawdown.
func TestMaxDrawdown_NoDrawdown(t *testing.T) {
	frac, amount := maxDrawdown([]float64{10, 20, 5}, 1000)
	if amount != 0 || frac != 0 {
		t.Fatalf("rising path ⇒ 0/0, got fraction=%v amount=%v", frac, amount)
	}
}

// Two peaks: v0=100, P&L {-20,+120,-30} ⇒ path 100→80→200→170. The worst FRACTION
// (0.20, from peak 100 to 80) and the worst AMOUNT (30, from peak 200 to 170)
// fall at DIFFERENT peaks — pins the independent-maxima design.
func TestMaxDrawdown_IndependentMaxima(t *testing.T) {
	frac, amount := maxDrawdown([]float64{-20, 120, -30}, 100)
	if math.Abs(frac-0.20) > 1e-9 {
		t.Fatalf("fraction = %v, want 0.20 (from the low peak)", frac)
	}
	if math.Abs(amount-30) > 1e-9 {
		t.Fatalf("amount = %v, want 30 (from the high peak)", amount)
	}
}
```

Append to `internal/risk/compute/var/tailmeasures_test.go` (measure-level, `varmodel_test`):

```go
// $1000, returns {-10,-5,+20}% ⇒ P&L {-100,-50,+200}, v0=1000. Drawdown: 15% and
// $150 (both at the 850 trough vs the 1000 start-peak).
func TestMaxDrawdownMeasures(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{-0.10, -0.05, 0.20}}

	frac := varmodel.MaxDrawdownFraction(varmodel.Config{})(context.Background(), p, prov)
	if frac.Name != compute.MeasureMaxDrawdown {
		t.Fatalf("name = %q, want MaxDrawdown", frac.Name)
	}
	if got := dval(frac.Value); math.Abs(got-0.15) > 1e-4 {
		t.Fatalf("MaxDrawdown = %v, want 0.15", got)
	}

	amt := varmodel.MaxDrawdownAmount(varmodel.Config{})(context.Background(), p, prov)
	if amt.Name != compute.MeasureMaxDrawdownAmount {
		t.Fatalf("name = %q, want MaxDrawdownAmount", amt.Name)
	}
	if got := dval(amt.Value); math.Abs(got-150.0) > 1e-9 {
		t.Fatalf("MaxDrawdownAmount = %v, want 150.00", got)
	}
}

func TestMaxDrawdown_InsufficientData(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{0.01}} // <2 scenarios
	if got := dval(varmodel.MaxDrawdownFraction(varmodel.Config{})(context.Background(), p, prov).Value); got != 0 {
		t.Fatalf("insufficient data ⇒ zero MaxDrawdown, got %v", got)
	}
	if got := dval(varmodel.MaxDrawdownAmount(varmodel.Config{})(context.Background(), p, prov).Value); got != 0 {
		t.Fatalf("insufficient data ⇒ zero MaxDrawdownAmount, got %v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/var/ -run 'TestMaxDrawdown' -v`
Expected: FAIL — `undefined: maxDrawdown` / `undefined: varmodel.MaxDrawdownFraction` / `undefined: compute.MeasureMaxDrawdown`.

- [ ] **Step 3: Write minimal implementation**

Add both name consts to `internal/risk/compute/measures.go` const block:

```go
	MeasureMaxDrawdown       v1.MeasureName = "MaxDrawdown"
	MeasureMaxDrawdownAmount v1.MeasureName = "MaxDrawdownAmount"
```

Create `internal/risk/compute/var/drawdown.go`:

```go
package varmodel

import (
	"context"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// drawdownExponent is the Decimal scale of the FRACTION drawdown: a ratio in
// [0,1], so a fractional scale. The AMOUNT drawdown uses varExponent (cents).
const drawdownExponent int32 = -4

// maxDrawdown walks the cumulative value path V_t = v0 + Σ_{s≤t} pnl_s in TIME
// order, tracking the running peak, and returns the maximum peak-to-trough
// decline as a FRACTION of peak and as an absolute AMOUNT. The two are maximized
// INDEPENDENTLY and may fall at different peaks when the path has several peaks
// of different heights — the worst percentage decline (from a lower peak) and
// the worst dollar decline (from a higher peak) are distinct questions with
// distinct answers, so there is no fixed amount = fraction × peak relationship.
// The fraction is only updated where peak > 0 (a non-positive peak — a net-flat
// or net-short book — has no meaningful percentage drawdown); the amount is
// always well-defined.
func maxDrawdown(pnl []float64, v0 float64) (fraction, amount float64) {
	peak := v0
	v := v0
	for _, x := range pnl {
		v += x
		if v > peak {
			peak = v
		}
		dd := peak - v
		if dd > amount {
			amount = dd
		}
		if peak > 0 {
			if f := dd / peak; f > fraction {
				fraction = f
			}
		}
	}
	return fraction, amount
}

// MaxDrawdownFraction builds the MaxDrawdown measure — worst peak-to-trough
// decline as a fraction of peak (0–1), the relative-severity / benchmark layer.
// Reuses portfolioPnL, so it is consistent with VaR/ES over the same window.
func MaxDrawdownFraction(cfg Config) compute.ReturnsMeasure {
	window := cfg.Window
	return func(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider) v1.Measure {
		pnl, v0, ok := portfolioPnL(ctx, p, rp, window)
		if !ok {
			return zeroNamed(compute.MeasureMaxDrawdown)
		}
		frac, _ := maxDrawdown(pnl, v0)
		return v1.Measure{Name: compute.MeasureMaxDrawdown, Value: floatToDecimal(frac, drawdownExponent)}
	}
}

// MaxDrawdownAmount builds the MaxDrawdownAmount measure — worst peak-to-trough
// decline as an absolute base-currency loss (cents), the capital / margin layer.
func MaxDrawdownAmount(cfg Config) compute.ReturnsMeasure {
	window := cfg.Window
	return func(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider) v1.Measure {
		pnl, v0, ok := portfolioPnL(ctx, p, rp, window)
		if !ok {
			return zeroNamed(compute.MeasureMaxDrawdownAmount)
		}
		_, amount := maxDrawdown(pnl, v0)
		return v1.Measure{Name: compute.MeasureMaxDrawdownAmount, Value: floatToDecimal(amount, varExponent)}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/var/ -run 'TestMaxDrawdown' -v`
Expected: PASS (single-peak, no-drawdown, independent-maxima, measure-level, insufficient-data).

- [ ] **Step 5: Commit**

```bash
git add internal/risk/compute/var/drawdown.go internal/risk/compute/var/drawdown_internal_test.go internal/risk/compute/var/tailmeasures_test.go internal/risk/compute/measures.go
git commit -m "feat(risk): MaxDrawdown fraction + amount, independent maxima (RISK-M1)"
```

---

### Task 5: Register the tail measures + composition-root log line

Wire ES99 and both drawdowns into `varmodel.Register` so the single market-data-gated call at `risk-engine/main.go:161` registers them alongside VaR99 — no new composition-root branch.

**Files:**
- Modify: `internal/risk/compute/var/historical.go` (the `Register` function)
- Modify: `internal/risk/compute/var/historical_test.go` (extend the end-to-end wiring test)
- Modify: `services/risk-engine/cmd/risk-engine/main.go` (~line 162 log line)

**Interfaces:**
- Consumes: `ExpectedShortfall`, `MaxDrawdownFraction`, `MaxDrawdownAmount` (Tasks 3–4); `compute.BindReturns`; the four `compute.Measure*` name consts.

- [ ] **Step 1: Write the failing test**

Append to `internal/risk/compute/var/historical_test.go`:

```go
// TestRegister_RegistersTailMeasures asserts the one Register call wires ES99 and
// both drawdown measures alongside VaR99, all served off the same provider.
func TestRegister_RegistersTailMeasures(t *testing.T) {
	s := store.NewMemory()
	closes := []float64{100, 110, 105, 95, 90}
	var obs []store.Observation
	for i, px := range closes {
		obs = append(obs, store.Observation{
			InstrumentID:    "AAPL",
			ObservationTime: asOf.AddDate(0, 0, i-len(closes)),
			Price:           &commonpb.Decimal{Coefficient: int64(px * 100), Exponent: -2},
			Kind:            store.PriceKindClose,
			KnowledgeTime:   asOf.AddDate(0, 0, i-len(closes)),
		})
	}
	if err := s.Put(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	provider := returns.NewStoreReturnsProvider(s, returns.ReturnsConfig{Method: returns.ReturnSimple})

	r := compute.DefaultRegistry()
	varmodel.Register(context.Background(), r, provider, varmodel.Config{})

	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	set := compute.ComputeMeasures(p, r, nil)

	for _, name := range []v1.MeasureName{
		compute.MeasureES99, compute.MeasureMaxDrawdown, compute.MeasureMaxDrawdownAmount,
	} {
		if _, ok := set.Lookup(name); !ok {
			t.Errorf("%s missing from the registered set", name)
		}
	}
	// ES99 ≥ VaR99 on the served set.
	varM, _ := set.Lookup(compute.MeasureVaR99)
	esM, _ := set.Lookup(compute.MeasureES99)
	if dval(esM.Value) < dval(varM.Value)-1e-9 {
		t.Fatalf("served ES99 (%v) must be ≥ VaR99 (%v)", dval(esM.Value), dval(varM.Value))
	}
}

// TestDefaultRegistry_NoMarketDataHasHHINotTail: without Register (no price
// store), HHI is served (positions-only) but the returns-backed tail measures
// are absent — the honest no-market-data posture.
func TestDefaultRegistry_NoMarketDataHasHHINotTail(t *testing.T) {
	r := compute.DefaultRegistry()
	names := map[v1.MeasureName]bool{}
	for _, n := range r.Names() {
		names[n] = true
	}
	if !names[compute.MeasureHHI] {
		t.Error("HHI must be served even without market data")
	}
	for _, n := range []v1.MeasureName{
		compute.MeasureES99, compute.MeasureMaxDrawdown, compute.MeasureMaxDrawdownAmount,
	} {
		if names[n] {
			t.Errorf("%s must NOT be in the default (no-market-data) registry", n)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/var/ -run 'TestRegister_RegistersTailMeasures|TestDefaultRegistry_NoMarketDataHasHHINotTail' -v`
Expected: FAIL — `TestRegister_RegistersTailMeasures` reports ES99/MaxDrawdown/MaxDrawdownAmount missing (Register only wires VaR99).

- [ ] **Step 3: Extend `Register` in `historical.go`**

Replace the `Register` function body:

```go
// Register overrides MeasureVaR99 with historical-simulation VaR and registers
// the tail measures that read the same distribution — ES99 and both drawdown
// measures — all closed over provider. The engine calls this at startup once it
// has a price-store provider; absent that, the registry keeps the placeholder
// VaR (compute.VaR99) and serves none of the tail measures (HHI, being
// positions-only, is already in DefaultRegistry).
func Register(ctx context.Context, r *compute.Registry, provider compute.ReturnsProvider, cfg Config) {
	r.Register(compute.MeasureVaR99, compute.BindReturns(ctx, provider, Historical(cfg)))
	r.Register(compute.MeasureES99, compute.BindReturns(ctx, provider, ExpectedShortfall(cfg)))
	r.Register(compute.MeasureMaxDrawdown, compute.BindReturns(ctx, provider, MaxDrawdownFraction(cfg)))
	r.Register(compute.MeasureMaxDrawdownAmount, compute.BindReturns(ctx, provider, MaxDrawdownAmount(cfg)))
}
```

- [ ] **Step 4: Update the composition-root log line**

In `services/risk-engine/cmd/risk-engine/main.go`, change the success log (~line 162) so operators see what was registered:

```go
		logger.Info("RISK-12/RISK-M1: historical-simulation VaR99 + ES99 + MaxDrawdown(+Amount) registered off market-data price store")
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `GOFLAGS=-mod=mod go test ./internal/risk/compute/var/ -run 'TestRegister|TestDefaultRegistry' -v`
Expected: PASS.

- [ ] **Step 6: Full verification**

Run:
```bash
GOFLAGS=-mod=mod go build ./... && \
GOFLAGS=-mod=mod go vet ./internal/risk/... ./services/risk-engine/... && \
GOFLAGS=-mod=mod go test ./internal/risk/... ./services/risk-engine/... && \
GOFLAGS=-mod=mod go test ./test/arch/
```
Expected: build clean, vet clean, all risk + risk-engine tests PASS, `test/arch` unchanged (`ok`).

- [ ] **Step 7: Commit**

```bash
git add internal/risk/compute/var/historical.go internal/risk/compute/var/historical_test.go services/risk-engine/cmd/risk-engine/main.go
git commit -m "feat(risk): register ES99 + drawdown tail measures at the VaR site (RISK-M1)"
```

---

## Known cost (documented, not fixed here)

Each returns-backed measure (VaR99, ES99, MaxDrawdown, MaxDrawdownAmount) pulls its own return series from the provider per recompute — four reads where the data is identical. The `StoreReturnsProvider` reads Postgres; at scale a **request-scoped memoizing provider wrapper** (cache keyed by instrument+asOf+window for the span of one `ComputeMeasures` call) would collapse this to one read per instrument. This is a follow-up perf task, not part of RISK-M1: correctness first, and the existing single-measure VaR already pays one read. Left as a note so a future perf pass has the context.

## Post-implementation

After all five tasks: update `KANZ_TASKS.md` (move RISK-M1 from Buildable-now to DONE; note RISK-M2 = Black-Litterman/HRP as the next RISK task) and, if a durable decision emerged (the ES/VaR shared-distribution invariant is a candidate), add one line to `KANZ_BRAIN.md`. These are board hygiene, done in the wrap-up, not a plan task.

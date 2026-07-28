package liquidity

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/domain"
)

// --- deterministic test provider ------------------------------------------

type staticLiquidity map[string]LiquiditySpec

func (m staticLiquidity) Liquidity(_ context.Context, id string, _ time.Time) (LiquiditySpec, bool) {
	s, ok := m[id]
	return s, ok
}

func dec(c int64, e int32) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: c, Exponent: e} }

func money(amount int64, exp int32) *commonpb.Money {
	return &commonpb.Money{Amount: dec(amount, exp), CurrencyCode: "USD"}
}

func TestDaysToLiquidate_MonotonicInSize(t *testing.T) {
	m := DefaultModel() // participation 0.20
	spec := LiquiditySpec{ADV: 1_000_000, Spread: 0.0005}
	// rate = 0.20 × 1e6 = 200k/day.
	small, ok := m.DaysToLiquidate(100_000, spec)
	if !ok || math.Abs(small-0.5) > 1e-9 {
		t.Fatalf("100k @ 200k/day: got %.6f ok=%v want 0.5", small, ok)
	}
	large, ok := m.DaysToLiquidate(1_000_000, spec)
	if !ok || math.Abs(large-5.0) > 1e-9 {
		t.Fatalf("1M @ 200k/day: got %.6f ok=%v want 5", large, ok)
	}
	if large <= small {
		t.Fatalf("horizon must increase with size: %.4f !> %.4f", large, small)
	}
}

func TestDaysToLiquidate_ADVZero(t *testing.T) {
	m := DefaultModel()
	days, ok := m.DaysToLiquidate(1000, LiquiditySpec{ADV: 0, Spread: 0.001})
	if ok {
		t.Fatal("zero-ADV instrument must report not-liquidatable (ok=false)")
	}
	if !math.IsInf(days, 1) {
		t.Fatalf("zero-ADV horizon must be +Inf, got %v", days)
	}
}

func TestCostFraction(t *testing.T) {
	m := DefaultModel() // impactCoeff 1.0
	const spread = 0.0010
	half := 0.5 * spread

	if got := m.CostFraction(1, spread); math.Abs(got-half) > 1e-12 {
		t.Fatalf("1-day cost should be the half-spread: got %.8f want %.8f", got, half)
	}
	if got := m.CostFraction(4, spread); math.Abs(got-half*2) > 1e-12 { // √4 = 2
		t.Fatalf("4-day cost should be half-spread×√4: got %.8f want %.8f", got, half*2)
	}
	if m.CostFraction(9, spread) <= m.CostFraction(4, spread) {
		t.Fatal("cost must increase with horizon")
	}
	if got := m.CostFraction(0, 0); got != 0 {
		t.Fatalf("zero spread ⇒ zero cost, got %.8f", got)
	}
	// An +Inf horizon (zero-ADV) caps at the full notional fraction.
	if got := m.CostFraction(math.Inf(1), 0.5); got != 1 {
		t.Fatalf("illiquid cost fraction must cap at 1, got %.8f", got)
	}
}

func testBook(t *testing.T) (*domain.Portfolio, Provider) {
	t.Helper()
	asOf := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)
	p := domain.NewPortfolio("p1", "USD")
	// LIQUID_FAST: 1M ADV, 100k qty ⇒ 0.5 days. notional 1,000,000.
	p.SetPosition(domain.Position{InstrumentID: "LIQUID_FAST", Quantity: dec(100_000, 0), MarketValue: money(1_000_000, 0), AsOf: asOf})
	// LIQUID_SLOW: 100k ADV, 100k qty ⇒ 5 days. notional 500,000.
	p.SetPosition(domain.Position{InstrumentID: "LIQUID_SLOW", Quantity: dec(100_000, 0), MarketValue: money(500_000, 0), AsOf: asOf})
	// ILLIQUID: zero ADV ⇒ not liquidatable. notional 200,000.
	p.SetPosition(domain.Position{InstrumentID: "ILLIQUID", Quantity: dec(1_000, 0), MarketValue: money(200_000, 0), AsOf: asOf})
	// NO_DATA: not in the provider ⇒ skipped entirely.
	p.SetPosition(domain.Position{InstrumentID: "NO_DATA", Quantity: dec(10, 0), MarketValue: money(5_000, 0), AsOf: asOf})
	prov := staticLiquidity{
		"LIQUID_FAST": {ADV: 1_000_000, Spread: 0.0005},
		"LIQUID_SLOW": {ADV: 100_000, Spread: 0.0010},
		"ILLIQUID":    {ADV: 0, Spread: 0.005},
	}
	return p, prov
}

func TestLiquidationProfile(t *testing.T) {
	m := DefaultModel()
	p, prov := testBook(t)
	prof := m.LiquidationProfile(context.Background(), p, prov)

	if len(prof.Positions) != 3 { // NO_DATA skipped
		t.Fatalf("profile should cover the 3 instruments with liquidity data, got %d", len(prof.Positions))
	}
	if math.Abs(prof.MaxDays-5.0) > 1e-9 {
		t.Fatalf("MaxDays (slowest liquid name) should be 5, got %.6f", prof.MaxDays)
	}
	if prof.IlliquidNotional != 200_000 {
		t.Fatalf("IlliquidNotional should be 200000, got %.0f", prof.IlliquidNotional)
	}
	// Weighted over liquid names: (1,000,000×0.5 + 500,000×5)/(1,500,000) = 2.0.
	if math.Abs(prof.WeightedDays-2.0) > 1e-9 {
		t.Fatalf("WeightedDays should be 2.0, got %.6f", prof.WeightedDays)
	}
}

func TestStressWidensHorizonAndCost(t *testing.T) {
	m := DefaultModel()
	p, prov := testBook(t)
	base := m.LiquidationProfile(context.Background(), p, prov)
	baseCost := m.LiquidationCost(context.Background(), p, prov)

	stressed := Stress{SpreadMult: 3, ADVMult: 1.0 / 3.0}.Wrap(prov)
	sProf := m.LiquidationProfile(context.Background(), p, stressed)
	sCost := m.LiquidationCost(context.Background(), p, stressed)

	if sProf.WeightedDays <= base.WeightedDays {
		t.Fatalf("stress must widen the horizon: %.4f !> %.4f", sProf.WeightedDays, base.WeightedDays)
	}
	if sCost <= baseCost {
		t.Fatalf("stress must raise the liquidation cost: %.2f !> %.2f", sCost, baseCost)
	}
}

func TestLiquidityAdjustedVaR_NeverBelowVaR(t *testing.T) {
	m := DefaultModel()
	p, prov := testBook(t)
	const varValue = 50_000.0
	lvar := m.LiquidityAdjustedVaR(context.Background(), varValue, p, prov)
	if lvar < varValue {
		t.Fatalf("LVaR must be ≥ VaR: %.2f < %.2f", lvar, varValue)
	}
	if cost := m.LiquidationCost(context.Background(), p, prov); cost < 0 {
		t.Fatalf("liquidation cost must be non-negative, got %.2f", cost)
	}
}

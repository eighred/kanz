package compute

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/liquidity"
)

type staticLiquidity map[string]liquidity.LiquiditySpec

func (m staticLiquidity) Liquidity(_ context.Context, id string, _ time.Time) (liquidity.LiquiditySpec, bool) {
	s, ok := m[id]
	return s, ok
}

func liqTestBook(t *testing.T) (*domain.Portfolio, liquidity.Provider) {
	t.Helper()
	asOf := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "FAST", Quantity: dec(100_000, 0), MarketValue: &commonpb.Money{Amount: dec(1_000_000, 0), CurrencyCode: "USD"}, AsOf: asOf})
	p.SetPosition(domain.Position{InstrumentID: "SLOW", Quantity: dec(100_000, 0), MarketValue: &commonpb.Money{Amount: dec(500_000, 0), CurrencyCode: "USD"}, AsOf: asOf})
	prov := staticLiquidity{
		"FAST": {ADV: 1_000_000, Spread: 0.0005}, // 0.5 days
		"SLOW": {ADV: 100_000, Spread: 0.0010},   // 5 days
	}
	return p, prov
}

func TestRegisterLiquidityRisk_LVaRNeverBelowVaR(t *testing.T) {
	p, prov := liqTestBook(t)
	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil) // nil ⇒ VaR99 placeholder

	ms := ComputeMeasures(p, r, nil)
	v, ok := ms.Lookup(MeasureVaR99)
	if !ok {
		t.Fatal("VaR99 missing")
	}
	lv, ok := ms.Lookup(MeasureLVaR99)
	if !ok {
		t.Fatal("LVaR99 missing")
	}
	if decimalToFloat(lv.Value) < decimalToFloat(v.Value) {
		t.Fatalf("LVaR99 must be ≥ VaR99: %.2f < %.2f", decimalToFloat(lv.Value), decimalToFloat(v.Value))
	}
	// With a real liquidation cost the two diverge (LVaR strictly above VaR).
	if decimalToFloat(lv.Value) <= decimalToFloat(v.Value) {
		t.Fatalf("LVaR99 should exceed VaR99 given a positive liquidation cost")
	}
}

func TestRegisterLiquidityRisk_Horizon(t *testing.T) {
	p, prov := liqTestBook(t)
	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil)

	ms := ComputeMeasures(p, r, nil)
	h, ok := ms.Lookup(MeasureLiquidationHorizon)
	if !ok {
		t.Fatal("LiquidationHorizon missing")
	}
	// (1,000,000×0.5 + 500,000×5)/1,500,000 = 2.0 days.
	if d := math.Abs(decimalToFloat(h.Value) - 2.0); d > 1e-3 {
		t.Fatalf("weighted horizon: got %.4f want 2.0", decimalToFloat(h.Value))
	}
}

func TestRegisterLiquidityRisk_StressWidens(t *testing.T) {
	p, prov := liqTestBook(t)
	base := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), base, prov, liquidity.DefaultModel(), nil)
	baseMS := ComputeMeasures(p, base, nil)

	stressed := DefaultRegistry()
	stress := liquidity.Stress{SpreadMult: 3, ADVMult: 1.0 / 3.0}
	RegisterLiquidityRisk(context.Background(), stressed, stress.Wrap(prov), liquidity.DefaultModel(), nil)
	stressedMS := ComputeMeasures(p, stressed, nil)

	bh, _ := baseMS.Lookup(MeasureLiquidationHorizon)
	sh, _ := stressedMS.Lookup(MeasureLiquidationHorizon)
	if decimalToFloat(sh.Value) <= decimalToFloat(bh.Value) {
		t.Fatalf("stress must widen horizon: %.4f !> %.4f", decimalToFloat(sh.Value), decimalToFloat(bh.Value))
	}
	bl, _ := baseMS.Lookup(MeasureLVaR99)
	sl, _ := stressedMS.Lookup(MeasureLVaR99)
	if decimalToFloat(sl.Value) <= decimalToFloat(bl.Value) {
		t.Fatalf("stress must raise LVaR: %.2f !> %.2f", decimalToFloat(sl.Value), decimalToFloat(bl.Value))
	}
}

package scenario_test

import (
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/compute/factor"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/scenario"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func money(amount int64, exp int32, ccy string) *commonpb.Money {
	return &commonpb.Money{
		Amount:       &commonpb.Decimal{Coefficient: amount, Exponent: exp},
		CurrencyCode: ccy,
	}
}

func pct(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

func makePortfolio(positions ...domain.Position) *domain.Portfolio {
	p := domain.NewPortfolio("PORT-1", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: baseTime, BaseCurrency: "USD"})
	for _, pos := range positions {
		p.SetPosition(pos)
	}
	return p
}

func TestEvaluate_PriceShockMovesTargetMarketValue(t *testing.T) {
	// -10% on AAPL: 1000 → 900. mulDecimal of 1000×10^0 by the
	// factor 90×10^-2 yields 90000×10^-2 — same numeric value 900,
	// different Decimal representation. The Decimal contract
	// (docs/decimal.proto) is explicit: 1.0 and 1.00 are equal in
	// value but differ in fields; consumers compare numerically.
	p := makePortfolio(
		domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	got := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.PriceShock{InstrumentID: "AAPL", Pct: pct(-10, -2)}, // -0.10
	}, nil)

	m, _ := got.Lookup(compute.MeasureNetExposure)
	if m.Value.Coefficient != 90000 || m.Value.Exponent != -2 {
		t.Errorf("NetExposure=%d × 10^%d want 90000 × 10^-2 (= 900)", m.Value.Coefficient, m.Value.Exponent)
	}
}

func TestEvaluate_OriginalPortfolioIsUnchanged(t *testing.T) {
	// The contract: scenario state is a transient clone; the live
	// portfolio held under RISK-05's lock must survive untouched
	// even when scenarios mutate aggressively.
	p := makePortfolio(
		domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	_ = scenario.Evaluate(p, []v1.ScenarioShock{
		v1.PriceShock{InstrumentID: "AAPL", Pct: pct(-99, -2)}, // -99%
	}, nil)

	// Original position unchanged.
	pos, _ := p.Position("AAPL")
	if pos.MarketValue.Amount.Coefficient != 1000 {
		t.Errorf("original AAPL MarketValue mutated: %d want 1000", pos.MarketValue.Amount.Coefficient)
	}
}

func TestEvaluate_PriceShockOnUnheldInstrumentIsNoOp(t *testing.T) {
	// Watchlist-wide batches commonly fire against multiple
	// portfolios; a shock for an instrument the portfolio does not
	// hold MUST NOT error.
	p := makePortfolio(
		domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	got := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.PriceShock{InstrumentID: "MSFT", Pct: pct(-50, -2)}, // MSFT not held
	}, nil)
	m, _ := got.Lookup(compute.MeasureNetExposure)
	if m.Value.Coefficient != 1000 {
		t.Errorf("NetExposure=%d want 1000 (unheld-instrument shock should not affect held positions)", m.Value.Coefficient)
	}
}

func TestEvaluate_ParallelShiftAppliesToAllPositions(t *testing.T) {
	// -5% on every position: 100 + 200 + 300 = 600 → 95 + 190 + 285 = 570.
	p := makePortfolio(
		domain.Position{InstrumentID: "A", MarketValue: money(100, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "B", MarketValue: money(200, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "C", MarketValue: money(300, 0, "USD"), AsOf: baseTime},
	)
	got := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.ParallelShift{Pct: pct(-5, -2)},
	}, nil)
	m, _ := got.Lookup(compute.MeasureNetExposure)
	// 600 × 0.95 = 570; but each position is independently shocked:
	// 95 + 190 + 285 = 570, with each Amount × 10^-2 representation.
	// 100 × (1 + -0.05) = 100 - 5.00 = 95.00 = 9500 × 10^-2
	// Total at exp -2: 9500 + 19000 + 28500 = 57000 × 10^-2 = 570
	if m.Value.Coefficient != 57000 || m.Value.Exponent != -2 {
		t.Errorf("NetExposure=%d × 10^%d want 57000 × 10^-2 (= 570)", m.Value.Coefficient, m.Value.Exponent)
	}
}

func TestEvaluate_ShocksApplyInOrder(t *testing.T) {
	// Compose +10% then -10%: 100 × 1.10 × 0.90 = 99 (not 100,
	// because percentages don't commute through ×).
	p := makePortfolio(
		domain.Position{InstrumentID: "A", MarketValue: money(100, 0, "USD"), AsOf: baseTime},
	)
	got := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.PriceShock{InstrumentID: "A", Pct: pct(10, -2)},  // +0.10
		v1.PriceShock{InstrumentID: "A", Pct: pct(-10, -2)}, // -0.10
	}, nil)
	m, _ := got.Lookup(compute.MeasureNetExposure)
	// 100 × 1.10 × 0.90 = 99 — but representation may carry the
	// product's exponent. 100 × (110×10^-2) × (90×10^-2):
	//   step 1: 100 × 110×10^-2 = 11000×10^-2 = 110
	//   step 2: 11000×10^-2 × 90×10^-2 = 990000×10^-4 = 99.
	if m.Value.Coefficient != 990000 || m.Value.Exponent != -4 {
		t.Errorf("NetExposure=%d × 10^%d want 990000 × 10^-4 (= 99)", m.Value.Coefficient, m.Value.Exponent)
	}
}

func TestEvaluate_UnknownShockTypeIsSilentlySkipped(t *testing.T) {
	// A custom shock the engine doesn't dispatch on must not abort
	// the run — large batches of mixed shock types should be
	// best-effort.
	p := makePortfolio(
		domain.Position{InstrumentID: "A", MarketValue: money(100, 0, "USD"), AsOf: baseTime},
	)
	got := scenario.Evaluate(p, []v1.ScenarioShock{
		unknownShock{},
	}, nil)
	m, _ := got.Lookup(compute.MeasureNetExposure)
	if m.Value.Coefficient != 100 {
		t.Errorf("NetExposure=%d want 100 (unknown shock must be no-op)", m.Value.Coefficient)
	}
}

func TestEvaluate_EmptyShockListReturnsCurrentMeasures(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "A", MarketValue: money(500, 0, "USD"), AsOf: baseTime},
	)
	got := scenario.Evaluate(p, nil, nil)
	m, _ := got.Lookup(compute.MeasureGrossExposure)
	if m.Value.Coefficient != 500 {
		t.Errorf("GrossExposure=%d want 500", m.Value.Coefficient)
	}
}

func TestEvaluate_AsOfPropagatedThroughScenario(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "A", MarketValue: money(100, 0, "USD"), AsOf: baseTime},
	)
	got := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.ParallelShift{Pct: pct(-1, -2)},
	}, nil)
	if !got.AsOf().Equal(baseTime) {
		t.Errorf("AsOf=%v want %v", got.AsOf(), baseTime)
	}
}

func TestEvaluate_CustomRegistryUsed(t *testing.T) {
	// Pass a registry containing only one measure; result should
	// contain only that one — confirms Evaluate honours the
	// registry parameter (not always DefaultRegistry).
	r := compute.NewRegistry()
	r.Register(compute.MeasureNetExposure, compute.NetExposure)
	p := makePortfolio(
		domain.Position{InstrumentID: "A", MarketValue: money(100, 0, "USD"), AsOf: baseTime},
	)
	got := scenario.Evaluate(p, nil, r)
	if names := got.Names(); len(names) != 1 || names[0] != compute.MeasureNetExposure {
		t.Errorf("registry not honoured; got names %v", names)
	}
}

// unknownShock implements v1.ScenarioShock but is not one of the
// dispatched types. Verifies the silent-skip contract.
type unknownShock struct{}

func (unknownShock) Description() string { return "unknown" }

// --- MODEL-01h SectorShock -------------------------------------------------

// classifierFor builds a StaticClassifier mapping each instrument to a GICS
// sector code.
func classifierFor(m map[string]string) factor.StaticClassifier {
	c := make(factor.StaticClassifier, len(m))
	for inst, code := range m {
		c[inst] = factor.Classification{Sector: factor.Sector{Taxonomy: "GICS", Code: code}}
	}
	return c
}

func TestEvaluate_SectorShockHitsOnlyMatchingSector(t *testing.T) {
	// BANK in financials (40) drops 50%; TECH in IT (45) is untouched.
	p := makePortfolio(
		domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "TECH", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	c := classifierFor(map[string]string{"BANK": "40", "TECH": "45"})
	got := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-50, -2)},
	}, nil, scenario.WithClassifier(c))

	// Net: BANK 1000→500, TECH 1000 ⇒ 1500.
	m, _ := got.Lookup(compute.MeasureNetExposure)
	if v := decToFloat(m.Value); v != 1500 {
		t.Errorf("NetExposure=%v want 1500 (only financials shocked)", v)
	}
}

func TestEvaluate_SectorShockNoClassifierIsNoOp(t *testing.T) {
	// Without a wired classifier the SectorShock cannot resolve membership,
	// so it degrades to a silent no-op (same as an unknown shock).
	p := makePortfolio(
		domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	got := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-50, -2)},
	}, nil) // no WithClassifier
	m, _ := got.Lookup(compute.MeasureNetExposure)
	if v := decToFloat(m.Value); v != 1000 {
		t.Errorf("NetExposure=%v want 1000 (no-op without classifier)", v)
	}
}

func TestEvaluate_SectorShockSkipsUnclassifiedInstrument(t *testing.T) {
	// MYSTERY isn't in the classifier ⇒ not a member of any shocked sector.
	p := makePortfolio(
		domain.Position{InstrumentID: "MYSTERY", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	c := classifierFor(map[string]string{"BANK": "40"})
	got := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-50, -2)},
	}, nil, scenario.WithClassifier(c))
	m, _ := got.Lookup(compute.MeasureNetExposure)
	if v := decToFloat(m.Value); v != 1000 {
		t.Errorf("NetExposure=%v want 1000 (unclassified untouched)", v)
	}
}

// decToFloat is a tiny numeric reader for the Decimal-representation-agnostic
// assertions above (ShockMoney changes the exponent, see the PriceShock test).
func decToFloat(d *commonpb.Decimal) float64 {
	f := float64(d.Coefficient)
	for e := d.Exponent; e < 0; e++ {
		f /= 10
	}
	for e := d.Exponent; e > 0; e-- {
		f *= 10
	}
	return f
}

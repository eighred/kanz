package scenario_test

import (
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/scenario"
	"github.com/eighred/kanz/internal/risk/scenario/library"
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
	// (common/v1/decimal.proto) is explicit: 1.0 and 1.00 are equal in
	// value but differ in fields; consumers compare numerically.
	p := makePortfolio(
		domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	got := evaluate(t, p, []v1.ScenarioShock{
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
	_ = evaluate(t, p, []v1.ScenarioShock{
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
	got := evaluate(t, p, []v1.ScenarioShock{
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
	got := evaluate(t, p, []v1.ScenarioShock{
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
	got := evaluate(t, p, []v1.ScenarioShock{
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
	got := evaluate(t, p, []v1.ScenarioShock{
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
	got := evaluate(t, p, nil, nil)
	m, _ := got.Lookup(compute.MeasureGrossExposure)
	if m.Value.Coefficient != 500 {
		t.Errorf("GrossExposure=%d want 500", m.Value.Coefficient)
	}
}

func TestEvaluate_AsOfPropagatedThroughScenario(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "A", MarketValue: money(100, 0, "USD"), AsOf: baseTime},
	)
	got := evaluate(t, p, []v1.ScenarioShock{
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
	got := evaluate(t, p, nil, r)
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
	got := evaluate(t, p, []v1.ScenarioShock{
		v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-50, -2)},
	}, nil, scenario.WithClassifier(c))

	// Net: BANK 1000→500, TECH 1000 ⇒ 1500.
	m, _ := got.Lookup(compute.MeasureNetExposure)
	if v := decToFloat(m.Value); v != 1500 {
		t.Errorf("NetExposure=%v want 1500 (only financials shocked)", v)
	}
}

// TestEvaluate_SectorShockNoClassifierIsRecordedNotSilent replaces a test named
// ...IsNoOp, which asserted the defect. It read "without a wired classifier the
// SectorShock cannot resolve membership, so it degrades to a silent no-op (same
// as an unknown shock)" and checked that the exposure came back unchanged — the
// exact behaviour #640 is about, pinned green.
//
// The unshocked VALUE is still the right value; there is nothing else the
// scenario could return. What must never happen again is returning it silently,
// so the assertion is on the coverage: a caller has to be able to tell this
// apart from a book with no financials.
func TestEvaluate_SectorShockNoClassifierIsRecordedNotSilent(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	got, cov := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-50, -2)},
	}, nil) // no WithClassifier
	if cov.ExcludedCount != 1 {
		t.Fatalf("ExcludedCount=%d want 1 — a sector shock with no classifier MUST be recorded, "+
			"or the unshocked book is served as the shocked one", cov.ExcludedCount)
	}
	if cov.Contributed != 0 {
		t.Errorf("Contributed=%d want 0 — no position's sector was resolved", cov.Contributed)
	}
	if len(cov.Exclusions) != 1 || cov.Exclusions[0].Reason != scenario.SkipNoClassifier {
		t.Fatalf("Exclusions=%+v want one %q", cov.Exclusions, scenario.SkipNoClassifier)
	}
	if id := cov.Exclusions[0].InstrumentID; id != "" {
		t.Errorf("InstrumentID=%q want empty — no classifier is a whole-evaluation gap, "+
			"not a property of any holding", id)
	}
	// The value is unchanged, which is why the record is the only signal.
	m, _ := got.Lookup(compute.MeasureNetExposure)
	if v := decToFloat(m.Value); v != 1000 {
		t.Errorf("NetExposure=%v want 1000", v)
	}
}

// TestEvaluate_SectorShockNoClassifierCountsOnceForAWholeCurve pins the count's
// MEANING. A named scenario is eleven SectorShocks, and recording the
// whole-evaluation gap per shock would report eleven exclusions for one missing
// classifier — a number nobody can act on and a sample that is eleven copies of
// nothing.
func TestEvaluate_SectorShockNoClassifierCountsOnceForAWholeCurve(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "TECH", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	_, cov := scenario.Evaluate(p, library.GlobalFinancialCrisis2008(), nil) // no classifier
	if cov.ExcludedCount != 1 {
		t.Fatalf("ExcludedCount=%d want 1 for an 11-shock curve — one missing classifier is one gap",
			cov.ExcludedCount)
	}
}

// TestEvaluate_SectorShockUnclassifiedInstrumentIsRecorded replaces a test named
// ...SkipsUnclassifiedInstrument, whose comment — "MYSTERY isn't in the
// classifier ⇒ not a member of any shocked sector" — states the inference the
// engine is not entitled to make. Not knowing an instrument's sector is not
// evidence that it is outside the shocked one.
func TestEvaluate_SectorShockUnclassifiedInstrumentIsRecorded(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "MYSTERY", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	c := classifierFor(map[string]string{"BANK": "40"})
	got, cov := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-50, -2)},
	}, nil, scenario.WithClassifier(c))
	if cov.ExcludedCount != 1 || cov.Contributed != 1 {
		t.Fatalf("coverage=%+v want ExcludedCount=1 (MYSTERY), Contributed=1 (BANK)", cov)
	}
	if len(cov.Exclusions) != 1 ||
		cov.Exclusions[0].InstrumentID != "MYSTERY" ||
		cov.Exclusions[0].Reason != scenario.SkipUnclassified {
		t.Fatalf("Exclusions=%+v want MYSTERY/%s", cov.Exclusions, scenario.SkipUnclassified)
	}
	// BANK still took the shock — an unresolvable holding does not stop the
	// shock landing on the ones that do resolve; it stops the RESULT being
	// served as complete. 500 + 1000 = 1500.
	m, _ := got.Lookup(compute.MeasureNetExposure)
	if v := decToFloat(m.Value); v != 1500 {
		t.Errorf("NetExposure=%v want 1500", v)
	}
}

// TestEvaluate_SectorShockNamingNoSectorIsRecorded covers the third way a sector
// shock cannot land, and the only one that is the caller's fault: a shock with
// neither taxonomy nor code has nothing to match against. It used to return
// silently alongside the other two.
func TestEvaluate_SectorShockNamingNoSectorIsRecorded(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	c := classifierFor(map[string]string{"BANK": "40"})
	_, cov := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.SectorShock{Pct: pct(-50, -2)}, // no taxonomy, no code
	}, nil, scenario.WithClassifier(c))
	if cov.ExcludedCount != 1 ||
		len(cov.Exclusions) != 1 ||
		cov.Exclusions[0].Reason != scenario.SkipShockNamesNoSector {
		t.Fatalf("coverage=%+v want one %q exclusion", cov, scenario.SkipShockNamesNoSector)
	}
}

// TestEvaluate_NonSectorScenarioNeedsNoClassifier is the FALSE-REFUSAL arm. A
// price/parallel scenario never consults the classifier, so it must come
// back with an empty coverage even though none is wired — otherwise the refusal
// added in #640 would take out every scenario on the platform rather than the
// ones that cannot be answered.
//
// THIS CASE USED TO CARRY A VolShock, and asserting an empty coverage over it
// was this test standing behind the #1035 defect: the vol bump reached nothing
// and the clean coverage was read here as proof no shock had gone missing. The
// vol arm moved to volshock_coverage_test.go, where the assertion is the
// opposite one. The remaining two shocks are MarketValue arithmetic the linear
// path expresses exactly, which is what "needs no classifier" was always about.
func TestEvaluate_NonSectorScenarioNeedsNoClassifier(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	_, cov := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.ParallelShift{Pct: pct(-20, -2)},
		v1.PriceShock{InstrumentID: "BANK", Pct: pct(-5, -2)},
	}, nil) // no classifier, and none needed
	if cov.ExcludedCount != 0 {
		t.Fatalf("coverage=%+v want empty — no shock here resolves a sector", cov)
	}
}

// evaluate runs a scenario that resolves everything it needs and FAILS if the
// coverage says otherwise. Every caller below is a non-sector scenario or a
// fully-classified one, so a non-empty coverage means Evaluate is refusing
// something it should have applied — which is the way the #640 fix could break
// the platform, and it would otherwise show up as a confusing value mismatch.
func evaluate(t *testing.T, p *domain.Portfolio, shocks []v1.ScenarioShock, registry *compute.Registry, opts ...scenario.Option) *domain.MeasureSet {
	t.Helper()
	got, cov := scenario.Evaluate(p, shocks, registry, opts...)
	if cov.ExcludedCount != 0 {
		t.Fatalf("shock coverage is not complete: %+v", cov)
	}
	return got
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

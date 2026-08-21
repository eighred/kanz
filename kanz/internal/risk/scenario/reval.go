package scenario

import (
	"context"
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
)

// Full-revaluation scenarios (DERIV-01e). EvaluateReval is the sibling of
// Evaluate that reprices OPTION positions through Black-Scholes/binomial under
// the scenario's price + vol shocks, instead of scaling their MarketValue
// linearly — so a long-gamma book shows its convexity and a vol shock shows its
// vega. Non-option positions keep the linear treatment, and a portfolio with no
// reval-able positions yields exactly what Evaluate would.
//
// It is a separate entry point, not a change to Evaluate, so the existing linear
// scenario path (and its callers) are untouched.

// Revaluer reprices an option position under shocks. compute.Revaluer satisfies
// it; the interface lives here so scenario does not force a particular
// implementation. ok=false ⇒ not an option / inputs unavailable (the position
// is shocked linearly instead).
type Revaluer interface {
	RevalueOption(ctx context.Context, instrumentID string, asOf time.Time, baseMV *commonpb.Money, shocks compute.RevalShocks) (*commonpb.Money, bool)
}

// EvaluateReval applies shocks to a deep clone of p — repricing option positions
// via reval and scaling the rest linearly — then computes measures against the
// revalued state. p is not mutated. A nil reval falls back to the fully linear
// Evaluate.
//
// It returns the SAME shock-application coverage Evaluate does, and for the same
// reason: sectorFrac resolves each position's sector through the classifier, so
// this path had the identical silent degradation (#640). It carries no caller
// today — EvaluateReval is a tracked dark seam in
// test/arch/no_dark_measure_seam_test.go, blocked on a Revaluer — and wiring it
// with the coverage already threaded is what stops the defect being reintroduced
// on the commit that lights it up.
func EvaluateReval(p *domain.Portfolio, shocks []v1.ScenarioShock, registry *compute.Registry, reval Revaluer, opts ...Option) (*domain.MeasureSet, v1.InputCoverage) {
	if reval == nil {
		return Evaluate(p, shocks, registry, opts...)
	}
	var cfg evalConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	cov := newShockCoverage()
	shocked := cloneWithReval(p, shocks, reval, cfg, cov)
	return compute.ComputeMeasures(shocked, registry, nil), cov.result()
}

// cloneWithReval deep-copies p and applies the shocks: each option position is
// repriced under its aggregated shocks; each other position is scaled by its
// linear price fraction.
func cloneWithReval(p *domain.Portfolio, shocks []v1.ScenarioShock, reval Revaluer, cfg evalConfig, cov *shockCoverage) *domain.Portfolio {
	cp := domain.NewPortfolio(p.ID(), p.BaseCurrency())
	cp.SetAggregate(domain.AggregateUpdate{
		AsOf:             p.AsOf(),
		DisplayName:      p.DisplayName(),
		BaseCurrency:     p.BaseCurrency(),
		CashBalance:      p.CashBalance(),
		TotalMarketValue: p.TotalMarketValue(),
		PositionCount:    p.PositionCount(),
	})
	for _, pos := range p.Positions() {
		cp.SetPosition(pos)
	}

	parallelFrac, globalVol, volByUnderlying := collectGlobalShocks(shocks)
	// ONCE, BEFORE THE LOOP: no classifier is a property of the evaluation, not
	// of any holding, so it is one whole-book exclusion. Recording it per
	// position would make a deployment-wide gap look like a per-instrument data
	// hole and would scale the count with the book size.
	if cfg.classifier == nil && hasSectorShock(shocks) {
		cov.noClassifierWired()
	}
	ctx := context.Background()
	asOf := p.AsOf()
	for _, pos := range cp.Positions() {
		priceFrac := parallelFrac + instrumentPriceFrac(shocks, pos.InstrumentID) + sectorFrac(ctx, cfg.classifier, pos, asOf, shocks, cov)
		rs := compute.RevalShocks{PriceFrac: priceFrac, GlobalVolBump: globalVol, VolByUnderlying: volByUnderlying}
		if mv, ok := reval.RevalueOption(ctx, string(pos.InstrumentID), asOf, pos.MarketValue, rs); ok {
			pos.MarketValue = mv
			cp.SetPosition(pos)
			continue
		}
		// Linear fallback for non-option positions.
		if priceFrac != 0 && pos.MarketValue != nil {
			pos.MarketValue = compute.ShockMoney(pos.MarketValue, fracDecimal(priceFrac))
			cp.SetPosition(pos)
		}
	}
	return cp
}

// collectGlobalShocks sums the book-wide price fraction (ParallelShift) and vol
// bumps (VolShock) from the shock list.
func collectGlobalShocks(shocks []v1.ScenarioShock) (parallelFrac, globalVol float64, volByUnderlying map[string]float64) {
	volByUnderlying = map[string]float64{}
	for _, sh := range shocks {
		switch s := sh.(type) {
		case v1.ParallelShift:
			parallelFrac += decFloat(s.Pct)
		case v1.VolShock:
			if s.UnderlyingID == "" {
				globalVol += decFloat(s.AbsBump)
			} else {
				volByUnderlying[string(s.UnderlyingID)] += decFloat(s.AbsBump)
			}
		}
	}
	return parallelFrac, globalVol, volByUnderlying
}

// instrumentPriceFrac sums the per-instrument PriceShocks for one instrument.
func instrumentPriceFrac(shocks []v1.ScenarioShock, id domain.InstrumentID) float64 {
	var frac float64
	for _, sh := range shocks {
		if s, ok := sh.(v1.PriceShock); ok && s.InstrumentID == id {
			frac += decFloat(s.Pct)
		}
	}
	return frac
}

// hasSectorShock reports whether the batch contains a SectorShock at all. With
// none, no position's sector is ever consulted, so nothing is missing and the
// coverage stays empty — a price-and-vol scenario must not be refused for want
// of a classifier it does not use.
func hasSectorShock(shocks []v1.ScenarioShock) bool {
	for _, sh := range shocks {
		if _, ok := sh.(v1.SectorShock); ok {
			return true
		}
	}
	return false
}

// sectorFrac sums the SectorShocks that match the position's sector, resolved
// point-in-time. An unresolvable sector contributes 0 AND is recorded on cov —
// the caller refuses on a non-empty coverage rather than treating "sector
// unknown" as "not in the shocked sector", which is the linear path's rule
// (applySectorShock) stated once for both paths.
//
// The nil-classifier case is NOT handled here: it is a property of the whole
// evaluation, so cloneWithReval records it once before the position loop rather
// than once per holding.
func sectorFrac(ctx context.Context, classifier factor.Classifier, pos domain.Position, asOf time.Time, shocks []v1.ScenarioShock, cov *shockCoverage) float64 {
	if classifier == nil || !hasSectorShock(shocks) {
		return 0
	}
	cl, ok := classifier.Classify(ctx, string(pos.InstrumentID), asOf)
	if !ok {
		cov.unclassified(pos.InstrumentID)
		return 0
	}
	cov.resolved(pos.InstrumentID)
	var frac float64
	for _, sh := range shocks {
		if s, ok := sh.(v1.SectorShock); ok && cl.Sector.Taxonomy == s.Taxonomy && cl.Sector.Code == s.Code {
			frac += decFloat(s.Pct)
		}
	}
	return frac
}

// decFloat converts a shock fraction to float64 for the revaluation model.
//
// The scale factor is math.Pow10, which is O(1). It replaced a hand-rolled
// pow10f loop for the reason #246 was filed against compute's pow10: the loop
// ran once per unit of |exponent|, Decimal.exponent is a wire field, and
// ParallelShift.Pct comes STRAIGHT off the risk engine's gRPC surface — so a
// shock at exponent -2000000000 spun two billion float divisions per position.
// The gRPC ingress now refuses that (grpcsrv.requireDecimalDomain), but a bound
// that lives only at the ingress is one missed ingress from being no bound at
// all.
//
// The old loop was also silently WRONG at the extreme: -exp for exponent
// MinInt32 stays negative, so the loop body never ran and the scale factor came
// back as 1 — a 2-billionth of a percent shock applied as ×1. math.Pow10
// saturates to 0 / +Inf instead, which propagates visibly.
func decFloat(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

// fracDecimal converts a float fraction back to a Decimal at 1e-6 precision for
// the linear ShockMoney fallback.
func fracDecimal(f float64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: int64(f * 1e6), Exponent: -6}
}
